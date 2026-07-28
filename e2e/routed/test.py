#!/usr/bin/env python3
"""Opt-in acceptance coverage for the three-VM routed example."""

from __future__ import annotations

import ipaddress
import os
import re
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import (  # noqa: E402
    Harness,
    ethernet_ipv4,
    link_counter,
    pcapng_frames,
    pcapng_packet_count,
    require,
    root_network_snapshot,
    run_suite,
    wait_for,
)


GATEWAY = "192.0.2.1"
APPLICATION = "192.0.2.10"
CLIENT = "203.0.113.10"


def provision_gateway(harness: Harness) -> None:
    harness.install_guest_file(
        GATEWAY,
        Path(__file__).with_name("gateway-router.sh"),
        "/usr/local/sbin/topolab-example-gateway",
    )
    unit = """[Unit]
Description=TopoLab synthetic example gateway routing
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/topolab-example-gateway
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
"""
    harness.ssh(
        GATEWAY,
        "sudo tee /etc/systemd/system/topolab-routed-example.service >/dev/null",
        input=unit,
    )
    harness.ssh(
        GATEWAY,
        "sudo systemctl daemon-reload && sudo systemctl enable --now topolab-routed-example.service && sudo systemctl restart topolab-routed-example.service",
    )
    verify_gateway(harness)


def verify_gateway(harness: Harness) -> None:
    check = (
        "systemctl is-active --quiet topolab-routed-example.service && "
        'test "$(sysctl -n net.ipv4.ip_forward)" = 1 && '
        "iptables -w -C FORWARD -m comment --comment topolab-routed-example-owned-v1 "
        "-j TOPOLAB_EXAMPLE_FORWARD && "
        "iptables -w -t nat -C POSTROUTING -m comment "
        "--comment topolab-routed-example-owned-v1 -j TOPOLAB_EXAMPLE_NAT"
    )

    def ready() -> bool:
        return (
            harness.ssh(
                GATEWAY, "sudo sh -c " + repr(check), check=False, timeout=15
            ).returncode
            == 0
        )

    wait_for("gateway routing service", ready, timeout=180)


def gateway_nat_packets(harness: Harness) -> int:
    result = harness.ssh(
        GATEWAY,
        "sudo iptables -w -t nat -nvxL TOPOLAB_EXAMPLE_NAT | "
        "awk '$3 == \"MASQUERADE\" { total += $1 } END { print total + 0 }'",
    )
    return int(result.stdout.strip())


def edge_nat_packets(harness: Harness) -> int:
    state = harness.manifest()["network_state"]
    table = state["root_nft_table"]
    rules = subprocess.run(
        ("nft", "list", "chain", "inet", table, "postrouting"),
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    ).stdout
    return sum(
        int(match.group(1))
        for line in rules.splitlines()
        if "external-nat" in line
        if (match := re.search(r"counter packets ([0-9]+)", line))
    )


def internet(harness: Harness, address: str) -> None:
    harness.ssh(
        address,
        "ip route get 1.1.1.1 >/dev/null && ping -c 2 -W 2 1.1.1.1 >/dev/null && "
        "getent ahostsv4 cloud-images.ubuntu.com >/dev/null && "
        "curl -fsSIL --connect-timeout 8 --max-time 20 https://cloud-images.ubuntu.com/ >/dev/null",
        timeout=45,
    )


def prepare_nested_key(harness: Harness) -> None:
    require(harness.key is not None, "missing SSH key")
    harness.ssh(
        APPLICATION,
        "umask 077; cat > /tmp/topolab-routed-key",
        input=harness.key.read_text(encoding="utf-8"),
    )


def verify_routed(harness: Harness) -> None:
    prepare_nested_key(harness)
    command = (
        f"ping -c 2 -W 2 {CLIENT} >/dev/null && "
        f"ssh -i /tmp/topolab-routed-key -o BatchMode=yes -o IdentitiesOnly=yes "
        f"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null torob@{CLIENT} "
        "'printf %s \"$SSH_CONNECTION\"'"
    )
    connection = harness.ssh(APPLICATION, command).stdout.strip()
    require(
        connection.startswith("192.0.2.10 "),
        f"client observed unexpected routed source: {connection}",
    )


def main(harness: Harness) -> None:
    before = root_network_snapshot()
    normalized = harness.run("config").stdout
    require(
        "198.51.100.2/30" in normalized and "203.0.113.10/24" in normalized,
        "normalized routed example lost a segment",
    )
    harness.run("up")
    harness.run("up")
    manifest = harness.manifest()
    state = manifest["network_state"]
    fabric, management = state["fabric_namespace"], state["management_host_namespace"]
    gateway_pid = str(manifest["nodes"]["gateway"]["pid"])
    qemu_ns = os.readlink(f"/proc/{gateway_pid}/ns/net")
    fabric_ns = subprocess.run(
        ("ip", "netns", "exec", fabric, "readlink", "/proc/self/ns/net"),
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    ).stdout.strip()
    management_ns = subprocess.run(
        ("ip", "netns", "exec", management, "readlink", "/proc/self/ns/net"),
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    ).stdout.strip()
    require(
        qemu_ns == fabric_ns and qemu_ns != management_ns,
        "QEMU fabric and controller namespaces are not separated",
    )
    descendant_ns = harness.exec(
        "sh", "-c", "readlink /proc/self/ns/net; sh -c 'readlink /proc/self/ns/net'"
    ).stdout.splitlines()
    require(
        len(set(descendant_ns)) == 1 and descendant_ns[0] == management_ns,
        "exec descendants escaped the management namespace",
    )

    for address in (GATEWAY, APPLICATION):
        harness.wait_ssh(address)
    provision_gateway(harness)
    provision_gateway(harness)  # explicit idempotency
    prepare_nested_key(harness)
    wait_for(
        "SSH from application to client",
        lambda: (
            harness.ssh(
                APPLICATION,
                f"ssh -i /tmp/topolab-routed-key -o BatchMode=yes -o IdentitiesOnly=yes "
                f"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 "
                f"torob@{CLIENT} true",
                check=False,
                timeout=15,
            ).returncode
            == 0
        ),
        timeout=300,
    )
    verify_routed(harness)

    counters_before = harness.counters("gateway-transit")
    routed_capture = harness.popen(
        "capture",
        "gateway-transit",
        "--endpoint",
        "gateway:transit",
        "--direction",
        "both",
        "--duration",
        "12s",
        "--output",
        "captures/routed/client-to-service.pcapng",
    )
    verify_routed(harness)
    harness.wait_process(routed_capture, timeout=20)
    capture_path = (
        harness.working_dir / "captures" / "routed" / "client-to-service.pcapng"
    )
    require(pcapng_packet_count(capture_path) > 0, "routed capture is empty")

    application = ipaddress.IPv4Address(APPLICATION).packed
    client = ipaddress.IPv4Address(CLIENT).packed
    gateway_transit = ipaddress.IPv4Address("198.51.100.2").packed
    untranslated = translated = 0
    for frame in pcapng_frames(capture_path):
        packet = ethernet_ipv4(frame)
        if packet:
            source, destination, protocol, _ = packet
            if protocol == 6 and destination == client:
                untranslated += source == application
                translated += source == gateway_transit
    require(
        untranslated > 0 and translated == 0,
        "client-to-service replies did not use the explicit no-NAT path",
    )

    gateway_before, edge_before = (
        gateway_nat_packets(harness),
        edge_nat_packets(harness),
    )
    internet_capture = harness.popen(
        "capture",
        "gateway-transit",
        "--endpoint",
        "gateway:transit",
        "--direction",
        "both",
        "--duration",
        "18s",
        "--output",
        "captures/routed/internet.pcapng",
    )
    internet(harness, APPLICATION)
    internet(harness, GATEWAY)
    harness.ssh(
        APPLICATION,
        f"ssh -i /tmp/topolab-routed-key -o BatchMode=yes -o IdentitiesOnly=yes "
        f"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null torob@{CLIENT} "
        "'ping -c 2 -W 2 1.1.1.1 >/dev/null && getent ahostsv4 cloud-images.ubuntu.com >/dev/null'",
        timeout=45,
    )
    harness.wait_process(internet_capture, timeout=25)
    require(
        gateway_nat_packets(harness) > gateway_before,
        "gateway NAT counter did not advance",
    )
    require(
        edge_nat_packets(harness) > edge_before,
        "edge external NAT counter did not advance",
    )

    counters_after = harness.counters("gateway-transit")
    old, new = (
        link_counter(counters_before, "gateway-transit"),
        link_counter(counters_after, "gateway-transit"),
    )
    require(
        old["generation_id"] == new["generation_id"],
        "stable link counter generation changed",
    )
    for old_ep, new_ep in zip(old["endpoints"], new["endpoints"], strict=True):
        deltas = []
        for direction in ("ingress", "egress"):
            before_packets = old_ep[direction].get("packets")
            after_packets = new_ep[direction].get("packets")
            if before_packets is not None and after_packets is not None:
                deltas.append(after_packets - before_packets)
        require(
            any(delta > 0 for delta in deltas),
            f"no traffic delta at {new_ep['node']}:{new_ep['interface']}",
        )

    for operation in (
        ("restart", "gateway"),
        ("stop", "gateway"),
        ("start", "gateway"),
    ):
        harness.run(*operation)
        if operation[0] != "stop":
            harness.wait_ssh(GATEWAY)
            verify_gateway(harness)
    verify_routed(harness)

    # Moving a stopped overlay aside forces a fresh overlay while retaining an
    # inspectable copy in the temporary directory.
    harness.run("stop", "gateway")
    root_disk = Path(harness.manifest()["nodes"]["gateway"]["root_disk"]["path"])
    require(
        harness.working_dir in root_disk.parents,
        "manifest root disk escaped the working directory",
    )
    saved_overlay = harness.test_dir / "gateway-root-before-recreate.qcow2"
    root_disk.replace(saved_overlay)
    harness.run("up")
    harness.wait_ssh(GATEWAY)
    provision_gateway(harness)
    verify_routed(harness)

    namespaces = [
        fabric,
        management,
        *harness.manifest()["network_state"]["namespaces"].values(),
    ]
    harness.run("down")
    require(not harness.manifest_path.exists(), "manifest remains after down")
    require(
        (harness.working_dir / "captures" / "routed" / "internet.pcapng").stat().st_size
        > 0,
        "down removed retained capture",
    )
    listed = {
        line.split()[0]
        for line in subprocess.run(
            ("ip", "netns", "list"), check=True, text=True, stdout=subprocess.PIPE
        ).stdout.splitlines()
    }
    require(
        not listed.intersection(namespaces), "topology namespace remains after down"
    )
    require(
        root_network_snapshot() == before, "routed down did not restore root networking"
    )


if __name__ == "__main__":
    run_suite(
        "routed",
        main,
        template=True,
        required_tools=("tc", "bridge", "sysctl", "iptables", "ssh", "curl"),
    )
