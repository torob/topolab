#!/usr/bin/env python3
"""Opt-in acceptance coverage for `topolab snapshot`.

The suite needs an LVM volume group for its lvm-thin disks. Set
TOPOLAB_E2E_VG to an existing VG with at least 16 GiB free; TopoLab creates
its thin pool there and `down` removes it. Without TOPOLAB_E2E_VG, the suite
creates a sparse 16-GiB backing file under /var/tmp, a loop PV, and a
randomly named `tlsnap-*` VG, and removes them at the end.

    sudo python3 e2e/snapshot/test.py
"""

from __future__ import annotations

import json
import os
import secrets
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import E2EError, Harness, require, run_suite, wait_for  # noqa: E402


FILE_VM, THIN_VM, OFF_VM = "snap-file", "snap-thin", "snap-off"
NAMES = [FILE_VM, THIN_VM, OFF_VM]
ADDRESSES = {FILE_VM: "10.79.0.11", THIN_VM: "10.79.0.12", OFF_VM: "10.79.0.13"}
MANAGEMENT, ROUTER = "10.79.0.1", "10.79.0.254"
GUEST_FILE = "/var/lib/topolab-snapshot.txt"
LOCK_NOTICE = "waiting for the topology lock"
BACKING_BYTES = 16 << 30


def run(*command: str, check: bool = True, timeout: int = 120) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if check and result.returncode:
        raise E2EError(f"command failed ({result.returncode}): {' '.join(command)}\n{result.stdout}{result.stderr}")
    return result


class VolumeGroup:
    """The VG for the lvm-thin storage: an existing one or a loop fixture."""

    def __init__(self) -> None:
        self.existing = os.environ.get("TOPOLAB_E2E_VG", "")
        self.name = self.existing or f"tlsnap-{secrets.token_hex(4)}"
        self.backing: Path | None = None
        self.loop = ""

    def create(self) -> None:
        if self.existing:
            run("vgs", self.existing)
            return
        directory = Path(tempfile.mkdtemp(prefix="tl-snapshot-vg-", dir="/var/tmp"))
        self.backing = directory / "backing.img"
        run("truncate", "-s", str(BACKING_BYTES), str(self.backing))
        self.loop = run("losetup", "--find", "--show", str(self.backing)).stdout.strip()
        run("pvcreate", "--yes", self.loop)
        run("vgcreate", "--yes", self.name, self.loop)

    def topolab_lvs(self) -> list[str]:
        result = run("lvs", "--noheadings", "-o", "lv_name", self.name, check=False)
        return [name.strip() for name in result.stdout.split() if name.strip().startswith("tl")]

    def cleanup(self, harness: Harness, success: bool) -> None:
        if self.existing:
            return
        if self.loop:
            run("vgremove", "--yes", "--force", self.name, check=False)
            run("pvremove", "--yes", self.loop, check=False)
            run("losetup", "--detach", self.loop, check=False)
        if self.backing is not None:
            self.backing.unlink(missing_ok=True)
            self.backing.parent.rmdir()


VG = VolumeGroup()


def topology(data_gib: int = 1, with_off_vm: bool = True) -> str:
    template = "${TOPOLAB_TEMPLATE:?set by harness}"
    key = "${TOPOLAB_SSH_PUBLIC_KEY:?set by harness}"

    def vm(name: str, root: str, data: str | None, agent: bool) -> str:
        disks = f"    additional_disks: [{{name: data, size_gib: {data_gib}, storage: {data}}}]\n" if data else ""
        return f"""  - name: {name}
    template: ubuntu
    memory_mb: 1024
    root_disk: {{storage: {root}}}
{disks}    guest_agent: {str(agent).lower()}
    ssh_authorized_keys: ["{key}"]
    interfaces:
      - name: eth0
        ipv4:
          addresses: [{ADDRESSES[name]}/24]
          routes: [{{to: default, via: {ROUTER}}}]
          dns_servers: [1.1.1.1]
"""

    names = NAMES if with_off_vm else [FILE_VM, THIN_VM]
    links = "".join(
        f"  - {{name: {name}-lan, endpoints: [{{node: {name}, interface: eth0}}, {{node: lan, interface: {name}}}]}}\n"
        for name in names
    )
    off_vm = vm(OFF_VM, "local", None, False) if with_off_vm else ""
    return f"""name: snapshot
templates:
  - {{name: ubuntu, path: "{template}"}}
storages:
  - {{name: local, type: file, root_provisioning: snapshot}}
  - {{name: copies, type: file, root_provisioning: copy}}
  - {{name: thin, type: lvm-thin, volume_group: {VG.name}, pool_size_gib: 12, root_provisioning: snapshot}}
management_host:
  interfaces:
    - name: lan
      ipv4: {{addresses: [{MANAGEMENT}/24]}}
vms:
{vm(FILE_VM, "local", "copies", True)}{vm(THIN_VM, "thin", "thin", False)}{off_vm}routers:
  - name: edge
    interfaces:
      - name: lan
        ipv4: {{addresses: [{ROUTER}/24]}}
      - name: wan
        external: {{sources: [10.79.0.0/24]}}
switches: [{{name: lan}}]
links:
  - {{name: management-lan, endpoints: [{{node: management_host, interface: lan}}, {{node: lan, interface: management}}]}}
  - {{name: edge-lan, endpoints: [{{node: edge, interface: lan}}, {{node: lan, interface: edge}}]}}
{links}"""


def guest(harness: Harness, name: str, command: str, timeout: float = 120) -> str:
    return harness.ssh(ADDRESSES[name], command, timeout=timeout).stdout


def data_device(harness: Harness, name: str) -> str:
    serial = next(d["serial"] for d in harness.manifest()["nodes"][name]["additional_disks"] if d["name"] == "data")
    device = guest(harness, name, f'for d in /dev/disk/by-id/*{serial}*; do test -b "$d" && readlink -f "$d" && break; done').strip()
    require(device.startswith("/dev/"), f"{name} has no data disk device")
    return device


def write_markers(harness: Harness, name: str, value: str) -> None:
    command = f"echo {value} | sudo tee {GUEST_FILE} >/dev/null"
    if harness.manifest()["nodes"][name].get("additional_disks"):
        command += f" && printf %-64s {value} | sudo dd of={data_device(harness, name)} bs=64 count=1 conv=fsync status=none"
    guest(harness, name, command + " && sudo sync")


def read_markers(harness: Harness, name: str) -> tuple[str, str]:
    root = guest(harness, name, f"sudo cat {GUEST_FILE}").strip()
    data = ""
    if harness.manifest()["nodes"][name].get("additional_disks"):
        data = guest(harness, name, f"sudo dd if={data_device(harness, name)} bs=64 count=1 status=none").strip()
    return root, data


def boot_id(harness: Harness, name: str) -> str:
    return guest(harness, name, "cat /proc/sys/kernel/random/boot_id").strip()


def table(output: str, header: str) -> list[list[str]]:
    lines = output.splitlines()
    require(lines and lines[0] == header, f"output header is not {header!r}:\n{output}")
    return [line.split("\t") for line in lines[1:]]


def qmp_status(path: str) -> str:
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(30)
        connection.connect(path)
        stream = connection.makefile("rwb")
        require("QMP" in json.loads(stream.readline()), f"invalid QMP greeting from {path}")
        for command in ("qmp_capabilities", "query-status"):
            stream.write(json.dumps({"execute": command, "id": command}).encode() + b"\n")
            stream.flush()
            while True:
                response = json.loads(stream.readline())
                if response.get("id") == command:
                    break
        return response["return"]["status"]


def ps_row(harness: Harness, name: str) -> str:
    return next((line for line in harness.run("ps").stdout.splitlines() if line.startswith(f"{name}\tvm\t")), "")


def start_probe(harness: Harness) -> subprocess.Popen[str]:
    """Start an exec command that holds the shared network lock."""
    process = harness.popen("exec", "--user", "root", "--", "sleep", "3600")
    time.sleep(2)
    require(process.poll() is None, f"the exec probe ended at once; see {harness.log_path}")
    return process


def stop_probe(harness: Harness, process: subprocess.Popen[str]) -> None:
    process.send_signal(2)
    process.wait(timeout=30)
    log = getattr(process, "_topolab_log", None)
    if log:
        log.close()
    harness.background.remove(process)


def install_guest_agent(harness: Harness) -> None:
    guest(
        harness,
        FILE_VM,
        "sudo env DEBIAN_FRONTEND=noninteractive apt-get update && "
        "sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-guest-agent && "
        "sudo systemctl enable --now qemu-guest-agent.service",
        timeout=900,
    )
    wait_for("guest agent of snap-file", lambda: "guest_agent=available" in ps_row(harness, FILE_VM), timeout=180)


def main(harness: Harness) -> None:
    VG.create()
    harness.run("up", timeout=1200)
    for name in NAMES:
        harness.wait_ssh(ADDRESSES[name])
    install_guest_agent(harness)

    before = {name: f"before-{secrets.token_hex(8)}" for name in NAMES}
    for name in NAMES:
        write_markers(harness, name, before[name])
    harness.run("stop", OFF_VM, timeout=120)
    boots = {name: boot_id(harness, name) for name in (FILE_VM, THIN_VM)}

    # A running exec holds the shared network lock. Snapshot commands must
    # not wait for it.
    probe = start_probe(harness)
    created = harness.run("snapshot", "create", "base", timeout=600)
    stop_probe(harness, probe)
    require(LOCK_NOTICE not in created.stderr, f"snapshot create waited for a lock:\n{created.stderr}")
    rows = table(created.stdout, "NAME\tSTATE\tCONSISTENCY")
    require(rows == [[FILE_VM, "running", "frozen"], [THIN_VM, "running", "crash-consistent"], [OFF_VM, "stopped", "stopped"]], f"create rows: {rows}")
    freeze_log = guest(harness, FILE_VM, "sudo journalctl -u qemu-guest-agent -o cat --no-pager")
    require("guest-fsfreeze" in freeze_log, f"the guest agent log has no freeze:\n{freeze_log}")
    for name in (FILE_VM, THIN_VM):
        require(boot_id(harness, name) == boots[name], f"{name} rebooted during snapshot create")
        require(read_markers(harness, name)[0] == before[name], f"{name} lost its file during snapshot create")
        require(f"{name}\tvm\trunning\t" in harness.run("ps").stdout, f"{name} does not run after snapshot create")
    manifest = harness.manifest()
    status = {name: qmp_status(manifest["nodes"][name]["qmp_path"]) for name in (FILE_VM, THIN_VM)}
    require(all(value == "running" for value in status.values()), f"QEMU status after snapshot create: {status}")

    listed = json.loads(harness.run("snapshot", "list", "--format", "json").stdout)
    snapshot: dict[str, Any] = listed["snapshots"][0]
    require(listed["schema_version"] == 1 and snapshot["name"] == "base" and snapshot["state"] == "ready", f"list: {listed}")
    snapshot_lvs = [disk["lv_name"] for vm in snapshot["vms"] for disk in vm["disks"] if disk["storage_type"] == "lvm-thin"]
    require(len(snapshot_lvs) == 2, f"thin snapshot LVs: {snapshot_lvs}")
    for lv in snapshot_lvs:
        attr = run("lvs", "--noheadings", "-o", "lv_attr", f"{VG.name}/{lv}").stdout.strip()
        require(attr[1] == "r", f"snapshot LV {lv} is not read-only: {attr}")
    harness.write_evidence("snapshot-list.json", listed)

    # Change every VM after the snapshot. snap-off runs, changes, and stops.
    after = {name: f"after-{secrets.token_hex(8)}" for name in NAMES}
    harness.run("start", OFF_VM, timeout=300)
    harness.wait_ssh(ADDRESSES[OFF_VM])
    for name in NAMES:
        write_markers(harness, name, after[name])
    harness.run("stop", OFF_VM, timeout=120)

    # up refuses to remove a VM that the snapshot contains.
    harness.topology.write_text(topology(with_off_vm=False), encoding="utf-8")
    refused = harness.run("up", check=False, timeout=300)
    require(refused.returncode == 1 and f"up would remove VM {OFF_VM}, which snapshot \"base\" contains" in refused.stderr, f"up was not refused:\n{refused.stderr}")
    # Disks grow after the snapshot. The restore keeps the new size. up starts
    # every VM, so snap-off is stopped again before the restore.
    harness.topology.write_text(topology(data_gib=2), encoding="utf-8")
    harness.run("up", timeout=600)
    harness.run("stop", OFF_VM, timeout=120)

    probe = start_probe(harness)
    restored = harness.run("snapshot", "restore", "base", timeout=900)
    stop_probe(harness, probe)
    require(LOCK_NOTICE not in restored.stderr, f"snapshot restore waited for a lock:\n{restored.stderr}")
    rows = table(restored.stdout, "NAME\tBEFORE\tRESULT\tAFTER")
    require(rows == [[FILE_VM, "running", "restored", "running"], [THIN_VM, "running", "restored", "running"], [OFF_VM, "stopped", "restored", "stopped"]], f"restore rows: {rows}")
    require(f"{OFF_VM}\tvm\tstopped\t" in harness.run("ps").stdout, f"{OFF_VM} does not stay stopped")
    for name in (FILE_VM, THIN_VM):
        harness.wait_ssh(ADDRESSES[name])
        require(boot_id(harness, name) != boots[name], f"{name} did not boot again after the restore")
        size = guest(harness, name, f"lsblk -bdno SIZE {data_device(harness, name)}").strip()
        require(int(size) == 2 << 30, f"{name} data disk has {size} bytes after the restore, want the grown size")
    harness.run("start", OFF_VM, timeout=300)
    harness.wait_ssh(ADDRESSES[OFF_VM])
    for name in NAMES:
        root, data = read_markers(harness, name)
        require(root == before[name], f"{name} file after the restore: {root!r}, want {before[name]!r}")
        if data:
            require(data == before[name], f"{name} data disk after the restore: {data!r}, want {before[name]!r}")
        failed = guest(harness, name, "systemctl list-units --all --plain --no-legend --state=failed 'systemd-fsck*'").strip()
        require(not failed, f"{name}: a file system check failed after the restore: {failed}")

    harness.run("snapshot", "delete", "base", OFF_VM, timeout=300)
    listed = json.loads(harness.run("snapshot", "list", "--format", "json").stdout)
    require([vm["name"] for vm in listed["snapshots"][0]["vms"]] == [FILE_VM, THIN_VM], f"list after partial delete: {listed}")

    harness.run("down", timeout=300)
    remaining = VG.topolab_lvs()
    require(not remaining, f"LVs remain after down: {remaining}")


if __name__ == "__main__":
    run_suite(
        "snapshot",
        main,
        template=True,
        required_tools=("ssh", "lvm", "lvs", "vgs", "losetup", "pvcreate", "vgcreate", "truncate"),
        generated_topology=topology(),
        external_cleanup=VG.cleanup,
    )
