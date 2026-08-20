#!/usr/bin/env python3
"""Opt-in root/KVM coverage for the router-only network example."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import (  # noqa: E402
    Harness,
    counter_value,
    link_counter,
    pcapng_packet_count,
    require,
    root_network_snapshot,
    run_suite,
    wait_for,
)


def main(harness: Harness) -> None:
    # Make loss deterministic for the impairment phase; later reconciliation
    # restores delivery and must rotate the counter generation.
    harness.replace_topology("loss_percent: 1", "loss_percent: 100")
    before = root_network_snapshot()
    normalized = harness.run("config").stdout
    require("loss_percent: 100" in normalized, "normalized config omitted impairment")
    require(
        not (harness.working_dir / ".topolab").exists(), "config modified runtime state"
    )

    # Config is topology-aware but unprivileged. Exercise that contract when
    # sudo identifies a real non-root caller; version is covered by CLI tests.
    caller_uid, caller_gid = (
        int(os.environ.get("SUDO_UID", "0")),
        int(os.environ.get("SUDO_GID", "0")),
    )
    if caller_uid:
        readable = harness.test_dir / "readonly-topology.yml"
        readable.write_text(
            harness.topology.read_text(encoding="utf-8"), encoding="utf-8"
        )
        readable.chmod(0o644)
        harness.test_dir.chmod(0o755)
        command = harness.command("config", topology=readable)
        result = subprocess.run(
            command,
            env=harness.environment,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            preexec_fn=lambda: (os.setgid(caller_gid), os.setuid(caller_uid)),
        )
        harness.test_dir.chmod(0o700)
        require(result.returncode == 0, f"unprivileged config failed: {result.stderr}")

    harness.run("up")
    first_manifest = harness.manifest()
    harness.run("up")
    second_manifest = harness.manifest()
    require(not second_manifest.get("nodes"), "router-only topology created QEMU nodes")
    network = second_manifest["network_state"]
    require(
        {"left", "right"}.issubset(network["namespaces"]),
        "router namespaces are absent",
    )
    require(not network.get("taps"), "router-only topology created TAPs")
    root_inode = os.readlink("/proc/self/ns/net")
    fabric_inode = subprocess.run(
        (
            "ip",
            "netns",
            "exec",
            network["fabric_namespace"],
            "readlink",
            "/proc/self/ns/net",
        ),
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    ).stdout.strip()
    management_inode = subprocess.run(
        (
            "ip",
            "netns",
            "exec",
            network["management_host_namespace"],
            "readlink",
            "/proc/self/ns/net",
        ),
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    ).stdout.strip()
    require(
        len({root_inode, fabric_inode, management_inode}) == 3,
        "root, fabric, and management network namespaces are not isolated",
    )
    require(
        first_manifest["network_state"]["fabric_namespace"]
        == network["fabric_namespace"],
        "idempotent up replaced the fabric namespace",
    )
    require(
        root_network_snapshot() == before,
        "non-external topology changed root networking",
    )

    ps = harness.run("ps").stdout
    for fragment in (
        "left\trouter\tready",
        "lan\tswitch\tready",
        "fabric-namespace",
        "management-host-namespace",
        "management_host:management",
    ):
        require(fragment in ps, f"ps omitted {fragment!r}")
    require("isolation=true" in ps, "management-host forwarding isolation is absent")
    ps_json = json.loads(harness.run("ps", "--format", "json").stdout)
    require(ps_json["schema_version"] == 2, "ps JSON schema version is wrong")
    require(
        ps_json["topology"]["state"] == "active"
        and ps_json["network"]["state"] == "ready",
        "ps JSON did not report an active ready topology",
    )

    router_identity = harness.run(
        "router", "exec", "left", "--", "id", "-u"
    ).stdout.strip()
    require(router_identity == "0", "router exec did not default to root")
    for command in (("ip", "link", "show"), ("nft", "list", "tables"), ("tc", "qdisc", "show")):
        harness.run("router", "exec", "left", "--", *command)
    require(
        harness.run(
            "router", "exec", "left", "--cwd", "/tmp", "--", "pwd"
        ).stdout.strip()
        == "/tmp",
        "router exec --cwd was ignored",
    )
    require(
        harness.run(
            "router", "exec", "left", "--", "cat", input="router-stdio"
        ).stdout
        == "router-stdio",
        "router exec lost stdio",
    )
    require(
        harness.run(
            "router", "exec", "left", "--", "sh", "-c", "exit 41", check=False
        ).returncode
        == 41,
        "router exec did not propagate child exit status",
    )

    identity = harness.exec(
        "sh",
        "-c",
        "id -u; id -g; awk '/^Cap(Inh|Prm|Eff|Amb):/ {print $2}' /proc/self/status",
    )
    expected_uid = os.environ.get("SUDO_UID", "0")
    expected_gid = os.environ.get("SUDO_GID", "0")
    require(
        harness.run(
            "router", "exec", "left", "--user", expected_uid, "--", "id", "-u"
        ).stdout.strip()
        == expected_uid,
        "router exec --user did not select the requested account",
    )
    if expected_uid != "0":
        require(
            harness.run(
                "router",
                "exec",
                "left",
                "--user",
                expected_uid,
                "--cwd",
                "/root",
                "--",
                "true",
                check=False,
            ).returncode
            == 126,
            "non-root router exec entered an inaccessible cwd",
        )
    lines = identity.stdout.splitlines()
    require(
        lines[:2] == [expected_uid, expected_gid],
        "exec did not drop to the sudo caller",
    )
    require(
        set(lines[2:]) == {"0000000000000000"}, "exec child retained Linux capabilities"
    )
    require(
        harness.exec("cat", input="topolab-stdio").stdout == "topolab-stdio",
        "exec lost stdio",
    )
    require(
        harness.run("exec", "--cwd", "/tmp", "--", "pwd").stdout.strip() == "/tmp",
        "exec --cwd was ignored",
    )
    require(
        harness.exec("sh", "-c", "exit 42", check=False).returncode == 42,
        "exec did not propagate child exit status",
    )

    marker = Path(f"/tmp/topolab-e2e-signal-{os.getpid()}")
    marker.unlink(missing_ok=True)
    signal_command = harness.command(
        "exec",
        "--",
        "sh",
        "-c",
        'trap "exit 23" TERM; : > "$1"; while :; do sleep 1; done',
        "sh",
        str(marker),
    )
    supervisor = subprocess.Popen(signal_command, env=harness.environment)
    try:
        wait_for("exec signal child", marker.exists, timeout=10, interval=0.05)
        supervisor.terminate()
        require(
            supervisor.wait(timeout=10) == 23, "exec did not forward SIGTERM status"
        )
    finally:
        marker.unlink(missing_ok=True)
        if supervisor.poll() is None:
            supervisor.kill()

    router_marker = Path(f"/tmp/topolab-e2e-router-signal-{os.getpid()}")
    router_marker.unlink(missing_ok=True)
    router_signal_command = harness.command(
        "router",
        "exec",
        "left",
        "--",
        "sh",
        "-c",
        'trap "exit 24" TERM; : > "$1"; while :; do sleep 1; done',
        "sh",
        str(router_marker),
    )
    router_supervisor = subprocess.Popen(
        router_signal_command, env=harness.environment
    )
    try:
        wait_for("router exec signal child", router_marker.exists, timeout=10, interval=0.05)
        router_supervisor.terminate()
        require(
            router_supervisor.wait(timeout=10) == 24,
            "router exec did not forward SIGTERM status",
        )
    finally:
        router_marker.unlink(missing_ok=True)
        if router_supervisor.poll() is None:
            router_supervisor.kill()

    counters_before = harness.counters("left-lan")
    capture = harness.popen(
        "capture",
        "left-lan",
        "--endpoint",
        "left:trunk",
        "--direction",
        "egress",
        "--duration",
        "3s",
        "--output",
        "captures/network/loss.pcapng",
    )
    time.sleep(0.7)
    require(
        harness.exec("ping", "-c", "2", "-W", "1", "192.0.2.1", check=False).returncode
        != 0,
        "100% loss unexpectedly delivered replies",
    )
    harness.wait_process(capture, timeout=10)
    counters_after = harness.counters("left-lan")
    old_link, new_link = (
        link_counter(counters_before, "left-lan"),
        link_counter(counters_after, "left-lan"),
    )
    require(
        old_link["generation_id"] == new_link["generation_id"],
        "counter generation changed mid-test",
    )
    old_ep = next(item for item in old_link["endpoints"] if item["node"] == "left")
    new_ep = next(item for item in new_link["endpoints"] if item["node"] == "left")
    require(
        counter_value(new_ep, "impairment", "offered_packets")
        > counter_value(old_ep, "impairment", "offered_packets"),
        "offered counter did not advance",
    )
    require(
        counter_value(new_ep, "impairment", "configured_loss_drops")
        > counter_value(old_ep, "impairment", "configured_loss_drops"),
        "loss counter did not advance",
    )
    loss_capture = harness.working_dir / "captures" / "network" / "loss.pcapng"
    require(
        pcapng_packet_count(loss_capture) == 0,
        "post-impairment capture contains delivered frames",
    )

    old_generation = new_link["generation_id"]
    harness.replace_topology("loss_percent: 100", "loss_percent: 0")
    harness.run("up")
    harness.exec("ping", "-c", "2", "-W", "2", "192.0.2.1")
    harness.exec("ping", "-c", "2", "-W", "2", "192.0.2.2")
    repaired = link_counter(harness.counters("left-lan"), "left-lan")
    require(
        repaired["generation_id"] != old_generation,
        "impairment change retained stale generation",
    )

    # Delete one owned bridge to prove repair, then disturb its alias to prove
    # both up and down refuse foreign ownership without taking it over.
    manifest = harness.manifest()
    state = manifest["network_state"]
    bridge = state["links"]["left-lan"]["bridge"]
    fabric = state["fabric_namespace"]
    subprocess.run(
        ("ip", "netns", "exec", fabric, "ip", "link", "del", "dev", bridge), check=True
    )
    harness.run("up")
    state = harness.manifest()["network_state"]
    bridge = state["links"]["left-lan"]["bridge"]
    owner = next(
        resource["alias"]
        for resource in state["resources"]
        if resource["name"] == bridge
    )
    subprocess.run(
        (
            "ip",
            "netns",
            "exec",
            fabric,
            "ip",
            "link",
            "set",
            "dev",
            bridge,
            "alias",
            "foreign-owner",
        ),
        check=True,
    )
    try:
        require(
            harness.run("down", check=False).returncode != 0,
            "down adopted a foreign bridge",
        )
        require(
            harness.run("up", check=False).returncode != 0,
            "up adopted a foreign bridge",
        )
    finally:
        subprocess.run(
            (
                "ip",
                "netns",
                "exec",
                fabric,
                "ip",
                "link",
                "set",
                "dev",
                bridge,
                "alias",
                owner,
            ),
            check=True,
        )

    # A shared exec lock must delay exclusive down. Restore the topology after
    # the check so subsequent legacy-cleanup coverage remains independent.
    lock = harness.popen("exec", "--", "sleep", "2")
    time.sleep(0.3)
    started = time.monotonic()
    harness.run("down")
    elapsed = time.monotonic() - started
    harness.wait_process(lock, timeout=5)
    require(elapsed >= 1.4, "down did not wait for the shared exec lock")
    harness.run("down")
    require(root_network_snapshot() == before, "down did not restore root networking")

    # Exercise exact-resource legacy cleanup without silently migrating v1.
    legacy = harness.test_dir / "legacy.yml"
    legacy.write_text(
        """name: legacy
management_host:
  interfaces:
    - name: management
      ipv4: {addresses: [10.88.0.2/30]}
routers:
  - name: router
    interfaces:
      - name: management
        ipv4: {addresses: [10.88.0.1/30]}
links:
  - name: management
    endpoints:
      - {node: management_host, interface: management}
      - {node: router, interface: management}
""",
        encoding="utf-8",
    )
    device = f"tllegacy{os.getpid()}"[:15]
    subprocess.run(("ip", "link", "add", "dev", device, "type", "dummy"), check=True)
    try:
        alias = "topolab:v1:legacy:legacy-dummy"
        subprocess.run(("ip", "link", "set", "dev", device, "alias", alias), check=True)
        legacy_dir = harness.working_dir / ".topolab" / "topologies" / "legacy"
        legacy_dir.mkdir(parents=True, mode=0o700)
        (legacy_dir / "manifest.json").write_text(
            json.dumps(
                {
                    "version": 3,
                    "topology": "legacy",
                    "datapath_version": 1,
                    "nodes": {},
                    "links": {},
                    "host_interfaces": {},
                    "network_state": {
                        "topology": "legacy",
                        "resources": [{"kind": "link", "name": device, "alias": alias}],
                        "links": {},
                        "fabric_namespace": "",
                        "management_host_namespace": "",
                    },
                }
            ),
            encoding="utf-8",
        )
        refused = harness.run("up", topology=legacy, check=False)
        require(
            refused.returncode != 0 and "legacy root datapath" in refused.stderr,
            "up silently migrated legacy state",
        )
        harness.run("down", topology=legacy)
        require(
            subprocess.run(
                ("ip", "link", "show", "dev", device),
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            ).returncode
            != 0,
            "legacy owned device remains",
        )
    finally:
        subprocess.run(
            ("ip", "link", "del", "dev", device),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )


if __name__ == "__main__":
    run_suite("network", main, required_tools=("tc", "bridge", "sysctl"))
