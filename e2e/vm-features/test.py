#!/usr/bin/env python3
"""Opt-in live-reconciliation and lifecycle coverage for VM features."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import Harness, require, root_network_snapshot, run_suite, wait_for  # noqa: E402


FEATURE = "192.0.2.10"
LIFECYCLE = "192.0.2.11"
MARKER = "topolab-e2e-disk-content-v1"


def running_pid(harness: Harness, name: str) -> int:
    return int(harness.manifest()["nodes"][name]["pid"])


def wait_agent(harness: Harness, available: bool) -> None:
    expected = f"guest_agent={'available' if available else 'unavailable'}"
    wait_for(
        expected,
        lambda: (
            expected
            in next(
                line
                for line in harness.run("ps").stdout.splitlines()
                if line.startswith("feature-rich\t")
            )
        ),
        timeout=180,
        interval=2,
    )


def disk_device(harness: Harness, name: str) -> str:
    disk = next(
        item
        for item in harness.manifest()["nodes"]["feature-rich"]["additional_disks"]
        if item["name"] == name
    )
    result = harness.ssh(
        FEATURE,
        "sh",
        "-c",
        f"for device in /dev/disk/by-id/*{disk['serial']}*; do "
        'test -b "$device" && readlink -f "$device" && break; done',
    ).stdout.strip()
    require(result.startswith("/dev/"), f"guest omitted block device for disk {name}")
    return result


def make_root_ca(harness: Harness, name: str) -> Path:
    certificate = harness.test_dir / f"{name}.pem"
    private_key = harness.test_dir / f"{name}.key"
    subprocess.run(
        (
            "openssl",
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-sha256",
            "-nodes",
            "-days",
            "2",
            "-subj",
            f"/CN=TopoLab E2E {name}",
            "-addext",
            "basicConstraints=critical,CA:TRUE",
            "-addext",
            "keyUsage=critical,keyCertSign,cRLSign",
            "-keyout",
            str(private_key),
            "-out",
            str(certificate),
        ),
        check=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    return certificate


def verify_guest_trust(harness: Harness, address: str, certificate: Path) -> None:
    guest_path = "/tmp/topolab-e2e-root-ca.pem"
    harness.install_guest_file(address, certificate, guest_path, mode="0644")
    result = harness.ssh(
        address,
        "openssl",
        "verify",
        "-CAfile",
        "/etc/ssl/certs/ca-certificates.crt",
        guest_path,
    )
    require(result.stdout.strip() == f"{guest_path}: OK", "guest does not trust Root CA")
    harness.ssh(address, "sudo", "rm", "-f", guest_path)


def main(harness: Harness) -> None:
    before = root_network_snapshot()
    root_ca = make_root_ca(harness, "root-ca")
    replacement_ca = make_root_ca(harness, "replacement-root-ca")
    harness.environment["TOPOLAB_ROOT_CA_PEM"] = root_ca.read_text(encoding="utf-8")
    harness.environment["TOPOLAB_ROOT_CA_PATH"] = str(root_ca)
    normalized = harness.run("config").stdout
    for fragment in (
        "target_mb: 3072",
        "queues: 4",
        "model: e1000e",
        "bus: nvme",
        "order:\n            - disk\n            - network",
        "guest_agent: true",
        "root_ca_certificates:",
        f"path: {root_ca}",
    ):
        require(
            fragment in normalized, f"normalized VM feature config omitted {fragment!r}"
        )
    harness.run("up")
    harness.wait_ssh(FEATURE)
    harness.wait_ssh(LIFECYCLE)
    initial_feature_pid = running_pid(harness, "feature-rich")
    initial_lifecycle_pid = running_pid(harness, "lifecycle")
    verify_guest_trust(harness, FEATURE, root_ca)
    verify_guest_trust(harness, LIFECYCLE, root_ca)

    argv = Path(f"/proc/{initial_feature_pid}/cmdline").read_bytes().split(b"\0")
    command_line = " ".join(item.decode(errors="replace") for item in argv)
    for fragment in (
        "virtio-balloon-pci",
        "virtio-serial-pci",
        "virtio-net-pci",
        "mq=on",
        "vectors=10",
        "e1000e",
        "nvme",
        "virtio-blk-pci",
        "menu=on,strict=on",
    ):
        require(fragment in command_line, f"QEMU argv omitted {fragment!r}")
    require(
        command_line.index("bootindex=1") < command_line.index("bootindex=2"),
        "disk boot priorities are not ordered",
    )

    # Serial records are merged for all nodes and selectable for one node.
    wait_for(
        "feature-rich serial log",
        lambda: bool(
            harness.run("logs", "--tail", "20", "feature-rich").stdout.strip()
        ),
        timeout=180,
    )
    selected_logs = harness.run(
        "logs", "--timestamps", "--tail", "20", "feature-rich"
    ).stdout
    require(
        "feature-rich:" in selected_logs and "lifecycle:" not in selected_logs,
        "selected logs included the wrong VM",
    )
    all_logs = harness.run("logs", "--tail", "20").stdout
    require(
        "feature-rich:" in all_logs and "lifecycle:" in all_logs,
        "merged logs omitted a VM",
    )

    # Force a deterministic unavailable state even if a compatible override
    # image already ships the package.
    harness.ssh(
        FEATURE, "sudo systemctl stop qemu-guest-agent.service >/dev/null 2>&1 || true"
    )
    wait_agent(harness, False)
    # Exercise the available-agent path by installing through this VM's tested
    # external route, not by customizing or rebuilding the cached template.
    harness.ssh(
        FEATURE,
        "sudo env DEBIAN_FRONTEND=noninteractive apt-get update && "
        "sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y qemu-guest-agent && "
        "sudo systemctl enable --now qemu-guest-agent.service",
        timeout=600,
    )
    wait_agent(harness, True)

    # Write directly to an otherwise empty data disk so online growth and later
    # restarts can prove that the disk identity/content was preserved.
    data_device = disk_device(harness, "data")
    harness.ssh(
        FEATURE, f"printf %s {MARKER} | sudo dd of={data_device} conv=fsync status=none"
    )
    data_path = next(
        item["path"]
        for item in harness.manifest()["nodes"]["feature-rich"]["additional_disks"]
        if item["name"] == "data"
    )

    harness.replace_topology("target_mb: 3072", "target_mb: 2048")
    balloon_update = harness.run("up")
    require(
        "balloon target is pending" not in balloon_update.stderr,
        "QMP did not accept the live balloon target",
    )
    require(
        running_pid(harness, "feature-rich") == initial_feature_pid,
        "balloon target change restarted the VM",
    )
    feature_line = next(
        line
        for line in harness.run("ps").stdout.splitlines()
        if line.startswith("feature-rich\t")
    )
    require(
        "balloon_target_mb=2048" in feature_line,
        "ps omitted the updated balloon target",
    )

    harness.replace_topology("name: data, size_gib: 2", "name: data, size_gib: 3")
    harness.run("up")
    require(
        running_pid(harness, "feature-rich") == initial_feature_pid,
        "online disk growth restarted the VM",
    )
    data = next(
        item
        for item in harness.manifest()["nodes"]["feature-rich"]["additional_disks"]
        if item["name"] == "data"
    )
    require(
        data["path"] == data_path and data["capacity_bytes"] == 3 * 1024**3,
        "online growth replaced the data disk or recorded the wrong capacity",
    )
    observed_size = int(
        harness.ssh(FEATURE, f"sudo blockdev --getsize64 {data_device}").stdout.strip()
    )
    require(observed_size == 3 * 1024**3, "guest did not observe online disk growth")
    content = harness.ssh(
        FEATURE, f"sudo dd if={data_device} bs=1 count={len(MARKER)} status=none"
    ).stdout
    require(content == MARKER, "online disk growth damaged existing content")

    # Bootstrap networking is write-once while the overlay identity remains.
    harness.replace_topology("192.0.2.10/24", "192.0.2.12/24")
    rejected = harness.run("up", check=False)
    require(
        rejected.returncode != 0
        and "bootstrap configuration is write-once" in rejected.stderr,
        "up accepted a bootstrap-only network change",
    )
    require(
        running_pid(harness, "feature-rich") == initial_feature_pid,
        "bootstrap rejection disturbed the running VM",
    )
    harness.replace_topology("192.0.2.12/24", "192.0.2.10/24")

    # Trusted Root CAs are also write-once guest bootstrap inputs.
    harness.environment["TOPOLAB_ROOT_CA_PEM"] = replacement_ca.read_text(
        encoding="utf-8"
    )
    rejected = harness.run("up", check=False)
    require(
        rejected.returncode != 0
        and "bootstrap configuration is write-once" in rejected.stderr,
        "up accepted a Root CA bootstrap change",
    )
    require(
        running_pid(harness, "feature-rich") == initial_feature_pid
        and running_pid(harness, "lifecycle") == initial_lifecycle_pid,
        "Root CA bootstrap rejection disturbed a running VM",
    )
    harness.environment["TOPOLAB_ROOT_CA_PEM"] = root_ca.read_text(encoding="utf-8")

    # Selected lifecycle commands affect only their named VM.
    harness.run("stop", "lifecycle")
    require(
        Path(f"/proc/{initial_feature_pid}").exists(),
        "selected stop killed feature-rich",
    )
    require(
        "lifecycle\tvm\tstopped" in harness.run("ps").stdout,
        "selected stop not reported",
    )
    harness.run("start", "lifecycle")
    harness.wait_ssh(LIFECYCLE)
    require(
        running_pid(harness, "lifecycle") != initial_lifecycle_pid,
        "selected start did not launch a fresh process",
    )

    prior_feature_pid = running_pid(harness, "feature-rich")
    harness.run("restart", "feature-rich")
    harness.wait_ssh(FEATURE)
    require(
        running_pid(harness, "feature-rich") != prior_feature_pid,
        "selected restart retained the old process",
    )
    require(
        harness.ssh(
            FEATURE, f"sudo dd if={data_device} bs=1 count={len(MARKER)} status=none"
        ).stdout
        == MARKER,
        "restart damaged additional-disk content",
    )

    harness.run("stop")
    ps = harness.run("ps").stdout
    require(
        "feature-rich\tvm\tstopped" in ps and "lifecycle\tvm\tstopped" in ps,
        "all-VM stop left a VM running",
    )
    harness.run("start")
    harness.wait_ssh(FEATURE)
    harness.wait_ssh(LIFECYCLE)
    state = harness.manifest()["network_state"]
    namespaces = [
        state["fabric_namespace"],
        state["management_host_namespace"],
        *state["namespaces"].values(),
    ]
    harness.run("down")
    require(not Path(data_path).exists(), "down retained a writable data disk")
    present = {
        line.split()[0]
        for line in subprocess.run(
            ("ip", "netns", "list"), check=True, text=True, stdout=subprocess.PIPE
        ).stdout.splitlines()
    }
    require(
        not present.intersection(namespaces), "VM feature topology namespace remains"
    )
    require(
        root_network_snapshot() == before,
        "VM feature down did not restore root networking",
    )


if __name__ == "__main__":
    run_suite(
        "vm-features",
        main,
        template=True,
        required_tools=("tc", "bridge", "sysctl", "ssh", "openssl"),
    )
