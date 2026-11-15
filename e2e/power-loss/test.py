#!/usr/bin/env python3
"""Opt-in acceptance coverage for `topolab stop --hard` (VM power loss)."""

from __future__ import annotations

import json
import os
import re
import secrets
import select
import signal
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import Harness, require, root_network_snapshot, run_suite  # noqa: E402


VM_COUNT = 3
MEMORY_MB = int(os.environ.get("TOPOLAB_E2E_POWER_LOSS_MEMORY_MB", "1024"))
NAMES = [f"pl-{number}" for number in range(1, VM_COUNT + 1)]
ADDRESSES = {name: f"10.78.0.{10 + number}" for number, name in enumerate(NAMES, 1)}
MANAGEMENT = "10.78.0.1"
ROUTER = "10.78.0.254"
GUEST_FILE = "/var/lib/topolab-power-loss.txt"
LOCK_NOTICE = "waiting for the topology lock"
MAX_SPREAD_SECONDS = 1.0
# Lines that only a clean guest shutdown writes to the journal of the system
# manager (PID 1) and systemd-logind. The user manager of an SSH session also
# reaches its own shutdown.target ("Shutdown") when the session ends, so
# clean_shutdown_lines reads only those two sources. The control step proves
# that a normal stop writes at least one of these lines.
CLEAN_SHUTDOWN = re.compile(
    r"Power key pressed|System is powering down|Powering off|"
    r"Reached target (shutdown\.target - System Shutdown|poweroff\.target|final\.target)"
)
UNCLEAN_EVIDENCE = re.compile(
    r"EXT4-fs .*(recovery|orphan)|uncleanly shut down|corrupted or uncleanly", re.I
)
# Kernel lines that report a damaged filesystem or a failed disk access. The
# FAT "Volume was not properly unmounted" warning is the expected dirty bit
# after a power loss, so it is not in this list.
FILESYSTEM_ERRORS = re.compile(
    r"EXT4-fs error|EXT4-fs .*(corrupt|bad block bitmap|bad inode bitmap)|"
    r"Remounting filesystem read-only|I/O error|FAT-fs .*error"
)
# Lists the ext4 and vfat filesystems on the disk that holds /, with the start
# and the size of each partition in 512-byte sectors.
LIST_FILESYSTEMS = r"""set -eu
disk=$(lsblk -no PKNAME "$(findmnt -no SOURCE /)")
lsblk -nrbo NAME,FSTYPE,MOUNTPOINT "/dev/$disk" | while read -r name fstype mountpoint; do
  case "$fstype" in
    ext4|vfat) echo "$name $fstype $(cat "/sys/class/block/$name/start") $(cat "/sys/class/block/$name/size") ${mountpoint:-}" ;;
  esac
done"""


def topology() -> str:
    template = "${TOPOLAB_TEMPLATE:?set by harness}"
    key = "${TOPOLAB_SSH_PUBLIC_KEY:?set by harness}"
    vms = "".join(
        f"""  - name: {name}
    template: ubuntu
    memory_mb: {MEMORY_MB}
    root_disk: {{storage: local, cache: unsafe}}
    ssh_authorized_keys: ["{key}"]
    interfaces:
      - name: eth0
        optional: true
        ipv4: {{addresses: [{ADDRESSES[name]}/24]}}
"""
        for name in NAMES
    )
    links = "".join(
        f"  - {{name: {name}-lan, endpoints: [{{node: {name}, interface: eth0}}, {{node: lan, interface: {name}}}]}}\n"
        for name in NAMES
    )
    return f"""name: power-loss
templates:
  - {{name: ubuntu, path: "{template}"}}
storages:
  - {{name: local, type: file, root_provisioning: snapshot}}
management_host:
  interfaces:
    - name: lan
      ipv4: {{addresses: [{MANAGEMENT}/24]}}
vms:
{vms}routers:
  - name: edge
    interfaces:
      - name: lan
        ipv4: {{addresses: [{ROUTER}/24]}}
switches: [{{name: lan}}]
links:
  - {{name: management-lan, endpoints: [{{node: management_host, interface: lan}}, {{node: lan, interface: management}}]}}
  - {{name: edge-lan, endpoints: [{{node: edge, interface: lan}}, {{node: lan, interface: edge}}]}}
{links}"""


def parse_time(value: str) -> float:
    moment = datetime.strptime(value, "%Y-%m-%dT%H:%M:%S.%fZ")
    return moment.replace(tzinfo=timezone.utc).timestamp()


def spread(values: list[float]) -> float:
    return max(values) - min(values)


def guest(harness: Harness, name: str, command: str) -> str:
    return harness.ssh(ADDRESSES[name], command, timeout=120).stdout


def previous_boot_journal(harness: Harness, name: str) -> str:
    boots = guest(harness, name, "sudo journalctl --list-boots --no-pager")
    require(
        len([line for line in boots.splitlines() if line.strip()]) >= 3,
        f"{name} has no persistent journal of the previous boot:\n{boots}",
    )
    return guest(harness, name, "sudo journalctl -b -1 -o cat --no-pager")


def clean_shutdown_lines(harness: Harness, name: str) -> list[str]:
    journal = guest(harness, name, "sudo journalctl -b -1 -o cat --no-pager _PID=1 + _COMM=systemd-logind")
    return [line for line in journal.splitlines() if CLEAN_SHUTDOWN.search(line)]


def guest_filesystems(harness: Harness, name: str) -> list[dict[str, object]]:
    filesystems = []
    for line in guest(harness, name, LIST_FILESYSTEMS).splitlines():
        device, fstype, start, sectors, *mountpoint = line.split()
        filesystems.append({
            "device": device,
            "fstype": fstype,
            "start": int(start),
            "sectors": int(sectors),
            "mountpoint": mountpoint[0] if mountpoint else "",
        })
    require(
        any(fs["fstype"] == "ext4" and fs["mountpoint"] == "/" for fs in filesystems),
        f"{name}: no ext4 root filesystem in {filesystems}",
    )
    return filesystems


def clear_fat_dirty_bit(image: Path) -> bool:
    """Clear the FAT dirty bit, as fsck.fat -a does at boot. Return its old value.

    A power loss leaves this bit set on every vfat filesystem that the guest
    mounted for writing. The bit is in the state byte of the boot sector:
    offset 0x41 for FAT32 (16-bit sectors-per-FAT field is 0) and 0x25 for
    FAT12/16.
    """
    with image.open("r+b") as file:
        boot = bytearray(file.read(512))
        offset = 0x41 if int.from_bytes(boot[22:24], "little") == 0 else 0x25
        dirty = bool(boot[offset] & 0x01)
        boot[offset] &= 0xFE
        file.seek(0)
        file.write(boot)
    return dirty


def fsck(*command: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=600)


def check_filesystem_offline(harness: Harness, name: str, disk: str, fs: dict[str, object]) -> dict[str, object]:
    """Check one guest filesystem in a copy of the stopped VM's disk.

    The copy gets the same recovery that the guest does at the next boot and
    nothing more: ext4 replays its journal and frees its orphan inodes, and
    vfat loses its dirty bit. Then a read-only full check must find no error.
    The VM disk is not changed.
    """
    image = harness.test_dir / f"fsck-{name}-{fs['device']}.raw"
    source = (
        f"driver=raw,offset={int(fs['start']) * 512},size={int(fs['sectors']) * 512},"
        f"file.driver=qcow2,file.file.filename={disk.replace(',', ',,')}"
    )
    label = f"{name} {fs['device']} ({fs['fstype']} {fs['mountpoint'] or 'not mounted'})"
    try:
        subprocess.run(("qemu-img", "convert", "--image-opts", source, "-O", "raw", str(image)), check=True, timeout=600)
        if fs["fstype"] == "ext4":
            recovery = fsck("e2fsck", "-E", "journal_only", "-y", str(image))
            require(recovery.returncode in (0, 1), f"{label}: journal recovery failed ({recovery.returncode}):\n{recovery.stdout}")
            check = fsck("e2fsck", "-f", "-n", str(image))
            result = {"recovery": recovery.stdout.splitlines(), "check": check.stdout.splitlines()}
        else:
            dirty = clear_fat_dirty_bit(image)
            check = fsck("fsck.fat", "-n", str(image))
            result = {"dirty_bit_was_set": dirty, "check": check.stdout.splitlines()}
        require(check.returncode == 0, f"{label}: filesystem check found errors ({check.returncode}):\n{check.stdout}")
        return result
    finally:
        image.unlink(missing_ok=True)


def check_filesystems_online(harness: Harness, name: str, filesystems: list[dict[str, object]]) -> dict[str, object]:
    """Check that the booted guest found no filesystem error after the recovery."""
    mounted = [fs for fs in filesystems if fs["mountpoint"]]
    options = {}
    for fs in mounted:
        options[fs["mountpoint"]] = guest(harness, name, f"findmnt -no OPTIONS {fs['mountpoint']}").strip()
        require(options[fs["mountpoint"]].split(",")[0] == "rw", f"{name}: {fs['mountpoint']} is not mounted rw: {options[fs['mountpoint']]}")
    errors = {}
    for fs in filesystems:
        if fs["fstype"] == "ext4" and fs["mountpoint"]:
            count = guest(harness, name, f"cat /sys/fs/ext4/{fs['device']}/errors_count").strip()
            errors[fs["device"]] = int(count)
            require(int(count) == 0, f"{name}: ext4 {fs['device']} has errors_count {count}")
    kernel = guest(harness, name, "sudo journalctl -k -b 0 -o cat --no-pager")
    bad = [line for line in kernel.splitlines() if FILESYSTEM_ERRORS.search(line)]
    require(not bad, f"{name}: the kernel reports filesystem errors after the power loss: {bad}")
    failed = guest(harness, name, "systemctl list-units --all --plain --no-legend --state=failed 'systemd-fsck*'").strip()
    require(not failed, f"{name}: a boot-time filesystem check failed: {failed}")
    boot_fsck = guest(harness, name, "sudo journalctl -b 0 -o cat --no-pager -u 'systemd-fsck*'")
    return {"mount_options": options, "ext4_errors_count": errors, "boot_fsck": boot_fsck.splitlines()}


def start_probe(harness: Harness, *args: str) -> subprocess.Popen[str]:
    process = harness.popen(*args)
    time.sleep(1)
    require(process.poll() is None, f"probe {args} ended at once; see {harness.log_path}")
    return process


def stop_probe(harness: Harness, process: subprocess.Popen[str]) -> None:
    # SIGINT reaches the probe through the exec supervisor, which then exits
    # with the child's status (130).
    process.send_signal(signal.SIGINT)
    code = process.wait(timeout=30)
    log = getattr(process, "_topolab_log", None)
    if log:
        log.write(f"[exit {code}]\n")
        log.close()
    harness.background.remove(process)


def watch_exits(pids: dict[str, int]) -> tuple[threading.Thread, dict[str, float]]:
    """Record the host time when each QEMU process ends, through pidfds."""
    descriptors = {os.pidfd_open(pid): name for name, pid in pids.items()}
    exits: dict[str, float] = {}

    def run() -> None:
        poller = select.poll()
        for descriptor in descriptors:
            poller.register(descriptor, select.POLLIN)
        deadline = time.monotonic() + 120
        try:
            while len(exits) < len(descriptors) and time.monotonic() < deadline:
                for descriptor, _ in poller.poll(100):
                    exits.setdefault(descriptors[descriptor], time.time())
                    poller.unregister(descriptor)
        finally:
            for descriptor in descriptors:
                os.close(descriptor)

    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    return thread, exits


def line_count(path: Path) -> int:
    return len(path.read_text(encoding="utf-8").splitlines()) if path.exists() else 0


def lines_after(path: Path, moment: float) -> list[str]:
    """Return the probe lines that start with a time after moment."""
    return [line for line in path.read_text(encoding="utf-8").splitlines() if float(line.split()[0]) > moment]


def main(harness: Harness) -> None:
    before = root_network_snapshot()
    harness.run("up", timeout=900)
    for name in NAMES:
        harness.wait_ssh(ADDRESSES[name])

    # Control: a normal stop shuts the guest down cleanly, and the check
    # below can see that in the previous-boot journal.
    control = NAMES[0]
    harness.run("stop", control, timeout=120)
    harness.run("start", control, timeout=300)
    harness.wait_ssh(ADDRESSES[control])
    control_journal = previous_boot_journal(harness, control)
    control_clean = clean_shutdown_lines(harness, control)
    require(
        bool(control_clean),
        "the clean-shutdown check does not detect the normal stop:\n"
        + "\n".join(control_journal.splitlines()[-40:]),
    )

    # Long probes in the management-host and router namespaces. Both hold the
    # shared network lock for their full run.
    management_log = harness.test_dir / "probe-management.log"
    router_log = harness.test_dir / "probe-router.log"
    management_probe = start_probe(
        harness, "exec", "--user", "root", "--", "sh", "-c",
        "while :; do "
        f"r=fail; ping -c1 -W1 {ROUTER} >/dev/null 2>&1 && r=ok; "
        f"v=fail; ping -c1 -W1 {ADDRESSES[NAMES[-1]]} >/dev/null 2>&1 && v=ok; "
        'echo "$(date +%s.%N) router=$r vm=$v"; sleep 0.2; '
        f"done >> {management_log}",
    )
    router_probe = start_probe(
        harness, "router", "exec", "edge", "--", "sh", "-c",
        "while :; do "
        f"r=fail; ping -c1 -W1 {MANAGEMENT} >/dev/null 2>&1 && r=ok; "
        'echo "$(date +%s.%N) management=$r"; sleep 0.2; '
        f"done >> {router_log}",
    )

    # Each guest writes a file and a journal marker and flushes them.
    nonces = {name: secrets.token_hex(16) for name in NAMES}
    for name in NAMES:
        guest(
            harness,
            name,
            f"echo {nonces[name]} | sudo tee {GUEST_FILE} >/dev/null && "
            f"sudo logger -t topolab-e2e 'before hard stop {nonces[name]}' && "
            "sudo journalctl --sync && sudo sync",
        )

    filesystems = {name: guest_filesystems(harness, name) for name in NAMES}

    manifest = harness.manifest()
    pids = {name: int(manifest["nodes"][name]["pid"]) for name in NAMES}
    disks = {name: manifest["nodes"][name]["root_disk"]["path"] for name in NAMES}
    watcher, exits = watch_exits(pids)
    started = time.monotonic()
    stopped = harness.run("stop", "--hard", timeout=120)
    command_seconds = time.monotonic() - started
    stop_returned = time.time()
    watcher.join(timeout=30)
    require(LOCK_NOTICE not in stopped.stderr, f"stop --hard waited for a lock:\n{stopped.stderr}")

    lines = stopped.stdout.splitlines()
    require(lines[0] == "NAME\tRESULT\tPID\tSTOP_TIME\tEXIT_TIME", f"stop --hard header: {lines[:1]}")
    rows = [line.split("\t") for line in lines[1:]]
    require([row[0] for row in rows] == NAMES, f"stop --hard rows: {rows}")
    for name, result, pid, _, _ in rows:
        require(result == "stopped" and int(pid) == pids[name], f"stop --hard row for {name}: {result} {pid}")
    require(sorted(exits) == sorted(NAMES), f"QEMU processes still run: {sorted(set(NAMES) - set(exits))}")
    stop_times = [parse_time(row[3]) for row in rows]
    exit_times = [parse_time(row[4]) for row in rows]
    timing = {
        "vms": VM_COUNT,
        "memory_mb": MEMORY_MB,
        "command_seconds": round(command_seconds, 3),
        "stop_time_spread_ms": round(spread(stop_times) * 1000, 3),
        "exit_time_spread_ms": round(spread(exit_times) * 1000, 3),
        "harness_exit_spread_ms": round(spread(list(exits.values())) * 1000, 3),
        "max_exit_after_stop_ms": round(max(e - s for s, e in zip(stop_times, exit_times)) * 1000, 3),
        "output": stopped.stdout,
    }
    harness.write_evidence("power-loss-timing.json", timing)
    print("power-loss timing: " + json.dumps({k: v for k, v in timing.items() if k != "output"}), flush=True)
    for key in ("stop_time_spread_ms", "exit_time_spread_ms", "harness_exit_spread_ms"):
        require(timing[key] < MAX_SPREAD_SECONDS * 1000, f"{key} = {timing[key]} ms, want < 1000 ms")

    # Only the VMs stopped: the network, the namespaces, and the probes run.
    ps = harness.run("ps").stdout
    for name in NAMES:
        row = next((line for line in ps.splitlines() if line.startswith(f"{name}\tvm\t")), "")
        require(row.startswith(f"{name}\tvm\tstopped\t") and "network=ready" in row, f"ps does not show {name} as stopped:\n{ps}")
    network = harness.manifest()["network_state"]
    namespaces = {network["fabric_namespace"], network["management_host_namespace"], *network["namespaces"].values()}
    present = set(subprocess.run(("ip", "netns", "list"), check=True, text=True, stdout=subprocess.PIPE).stdout.split())
    require(namespaces <= present, f"namespaces are missing: {sorted(namespaces - present)}")
    probe_lines = (line_count(management_log), line_count(router_log))
    time.sleep(4)
    require(management_probe.poll() is None and router_probe.poll() is None, "a probe ended during stop --hard")
    require(
        line_count(management_log) > probe_lines[0] and line_count(router_log) > probe_lines[1],
        "a probe stopped writing after stop --hard",
    )
    # A failed VM ping takes 1 s, so check only the lines after the stop.
    recent = lines_after(management_log, stop_returned)
    require(recent and all("router=ok vm=fail" in line for line in recent), f"management probe after stop --hard: {recent}")
    recent = lines_after(router_log, stop_returned)
    require(recent and all("management=ok" in line for line in recent), f"router probe after stop --hard: {recent}")

    # The disk images are consistent.
    checks = {}
    for name in NAMES:
        result = subprocess.run(("qemu-img", "check", "-f", "qcow2", disks[name]), text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        checks[name] = result.stdout
        require(result.returncode == 0 and "No errors were found" in result.stdout, f"qemu-img check {disks[name]} ({result.returncode}):\n{result.stdout}")
    harness.write_evidence("qemu-img-check.json", checks)

    # Every guest filesystem is healthy after the recovery that the next boot
    # does. The check uses copies, so the start below boots the real disks.
    filesystem_health: dict[str, dict[str, object]] = {}
    for name in NAMES:
        filesystem_health[name] = {
            fs["device"]: check_filesystem_offline(harness, name, disks[name], fs) for fs in filesystems[name]
        }
    print("offline filesystem checks: " + json.dumps({
        name: {device: result["check"][-1:] for device, result in results.items()}
        for name, results in filesystem_health.items()
    }), flush=True)

    # start runs beside the probes, and every VM boots with its file.
    restarted = harness.run("start", timeout=900)
    require(LOCK_NOTICE not in restarted.stderr, f"start waited for a lock:\n{restarted.stderr}")
    evidence = {}
    for name in NAMES:
        harness.wait_ssh(ADDRESSES[name])
        content = guest(harness, name, f"sudo cat {GUEST_FILE}").strip()
        require(content == nonces[name], f"{name} lost its file: {content!r}")
        journal = previous_boot_journal(harness, name)
        require(f"before hard stop {nonces[name]}" in journal, f"{name} journal lost the marker of the previous boot")
        clean = clean_shutdown_lines(harness, name)
        require(not clean, f"{name} shut down cleanly: {clean}")
        current = guest(harness, name, "sudo journalctl -b 0 -o cat --no-pager")
        filesystem_health[name]["online"] = check_filesystems_online(harness, name, filesystems[name])
        evidence[name] = {
            "previous_boot_tail": journal.splitlines()[-5:],
            "unclean_evidence": [line for line in current.splitlines() if UNCLEAN_EVIDENCE.search(line)],
        }
    evidence["control_clean_shutdown_lines"] = control_clean
    harness.write_evidence("unclean-shutdown.json", evidence)
    harness.write_evidence("filesystem-health.json", {"filesystems": filesystems, "results": filesystem_health})
    print("control clean-shutdown lines: " + json.dumps(control_clean), flush=True)
    print("unclean-shutdown evidence: " + json.dumps({name: evidence[name]["unclean_evidence"] for name in NAMES}), flush=True)
    require(management_probe.poll() is None and router_probe.poll() is None, "a probe ended during start")

    stop_probe(harness, management_probe)
    stop_probe(harness, router_probe)
    harness.run("down", timeout=300)
    require(root_network_snapshot() == before, "power-loss down did not restore root networking")


if __name__ == "__main__":
    run_suite(
        "power-loss",
        main,
        template=True,
        required_tools=("ssh", "ping", "qemu-img", "e2fsck", "fsck.fat"),
        generated_topology=topology(),
    )
