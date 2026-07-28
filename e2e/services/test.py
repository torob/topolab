#!/usr/bin/env python3
"""Opt-in DHCP/VLAN/NAT coverage for the services example."""

from __future__ import annotations

import re
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import (  # noqa: E402
    Harness,
    dnsmasq_leases,
    ethernet_ipv4,
    pcapng_frames,
    require,
    root_network_snapshot,
    run_suite,
    wait_for,
)


def leases(harness: Harness) -> dict[str, list[dict[str, str]]]:
    helpers = harness.manifest().get("helpers", {})
    result = {}
    for key in ("services:trunk.10", "services:trunk.20"):
        require(key in helpers, f"manifest omitted DHCP helper {key}")
        result[key] = dnsmasq_leases(Path(helpers[key]["lease_path"]))
    return result


def nft_packets(
    namespace: str | None, family: str, table: str, chain: str, marker: str
) -> int:
    command = ["nft", "list", "chain", family, table, chain]
    if namespace:
        command = ["ip", "netns", "exec", namespace, *command]
    output = subprocess.run(
        command, check=True, text=True, stdout=subprocess.PIPE
    ).stdout
    return sum(
        int(match.group(1))
        for line in output.splitlines()
        if marker in line
        if (match := re.search(r"counter packets ([0-9]+)", line))
    )


def main(harness: Harness) -> None:
    before = root_network_snapshot()
    normalized = harness.run("config").stdout
    for fragment in (
        "vlan_filtering: true",
        "range_start: 10.50.10.100",
        "range_start: 10.50.20.100",
        "nat:",
        "external:",
    ):
        require(
            fragment in normalized, f"normalized services config omitted {fragment!r}"
        )
    harness.run("up")
    for node in harness.manifest()["nodes"].values():
        require(node["pid"] > 0, "a services VM is not running")

    def have_leases() -> bool:
        current = leases(harness)
        return (
            len(current["services:trunk.10"]) >= 1
            and len(current["services:trunk.20"]) >= 2
        )

    wait_for("DHCP leases on VLANs 10 and 20", have_leases, timeout=300)
    current = leases(harness)
    vlan10 = current["services:trunk.10"]
    vlan20 = current["services:trunk.20"]
    application10 = next(
        item["address"] for item in vlan10 if item["hostname"] == "application"
    )
    application20 = next(
        item["address"] for item in vlan20 if item["hostname"] == "application"
    )
    database20 = next(
        item["address"] for item in vlan20 if item["hostname"] == "database"
    )
    require(
        application10.startswith("10.50.10.")
        and application20.startswith("10.50.20.")
        and database20.startswith("10.50.20."),
        "DHCP issued an address outside its VLAN",
    )
    harness.wait_ssh(application10)
    require(harness.key is not None, "missing SSH key")
    harness.ssh(
        application10,
        "umask 077; cat > /tmp/topolab-services-key",
        input=harness.key.read_text(encoding="utf-8"),
    )

    nested = (
        "ssh -i /tmp/topolab-services-key -o BatchMode=yes -o IdentitiesOnly=yes "
        "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 "
        f"torob@{database20} true"
    )
    wait_for(
        "application-to-database SSH",
        lambda: (
            harness.ssh(application10, nested, check=False, timeout=15).returncode == 0
        ),
        timeout=240,
    )

    ps = harness.run("ps").stdout
    require(
        "management_host:admin" in ps and "isolation=true" in ps,
        "management-host isolation is not reported",
    )
    harness.ssh(application10, f"ip -4 address show trunk.10 | grep -F {application10}")
    harness.ssh(application10, f"ip -4 address show trunk.20 | grep -F {application20}")
    harness.ssh(application10, "resolvectl status | grep -F 1.1.1.1")
    harness.ssh(application10, "getent ahostsv4 cloud-images.ubuntu.com >/dev/null")

    capture = harness.popen(
        "capture",
        "application-service",
        "--endpoint",
        "application:trunk",
        "--direction",
        "both",
        "--duration",
        "10s",
        "--output",
        "captures/services/vlan.pcapng",
    )
    time.sleep(0.5)
    harness.ssh(application10, f"ping -c 3 -W 2 {database20} >/dev/null")
    harness.wait_process(capture, timeout=15)
    capture_path = harness.working_dir / "captures" / "services" / "vlan.pcapng"
    packets = [
        packet
        for frame in pcapng_frames(capture_path)
        if (packet := ethernet_ipv4(frame)) is not None
    ]
    require(
        packets and any(packet[3] for packet in packets),
        "application trunk capture contains no VLAN-aware IPv4 frames",
    )

    state = harness.manifest()["network_state"]
    service_ns = state["namespaces"]["services"]
    router_table = state["router_nft_tables"]["services"]
    local_before = nft_packets(
        service_ns, "ip", router_table, "postrouting", "router-nat"
    )
    root_table = state["root_nft_table"]
    external_before = nft_packets(
        None, "inet", root_table, "postrouting", "external-nat"
    )
    harness.ssh(
        application10,
        "ping -c 2 -W 2 1.1.1.1 >/dev/null && "
        "curl -fsSIL --connect-timeout 8 --max-time 20 https://cloud-images.ubuntu.com/ >/dev/null",
        timeout=45,
    )
    require(
        nft_packets(service_ns, "ip", router_table, "postrouting", "router-nat")
        > local_before,
        "router-local NAT counter did not advance",
    )
    require(
        nft_packets(None, "inet", root_table, "postrouting", "external-nat")
        > external_before,
        "external NAT counter did not advance",
    )

    # The management host is an endpoint, not a transit router.
    forwarding = harness.exec("sysctl", "-n", "net.ipv4.ip_forward").stdout.strip()
    require(forwarding == "0", "management-host namespace unexpectedly forwards IPv4")
    namespaces = [
        state["fabric_namespace"],
        state["management_host_namespace"],
        *state["namespaces"].values(),
    ]
    harness.run("down")
    require(
        capture_path.is_file() and capture_path.stat().st_size > 0,
        "down removed service capture",
    )
    present = {
        line.split()[0]
        for line in subprocess.run(
            ("ip", "netns", "list"), check=True, text=True, stdout=subprocess.PIPE
        ).stdout.splitlines()
    }
    require(
        not present.intersection(namespaces), "services namespace remains after down"
    )
    require(
        root_network_snapshot() == before,
        "services down did not restore root networking",
    )


if __name__ == "__main__":
    run_suite(
        "services",
        main,
        template=True,
        required_tools=("tc", "bridge", "dnsmasq", "sysctl", "ssh", "curl"),
    )
