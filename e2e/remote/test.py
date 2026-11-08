#!/usr/bin/env python3
"""Opt-in root/KVM coverage for remote operation through SSH to localhost.

The suite drives the router-only network example through `--remote`. The host
is this machine, so local commands can cross-check what the remote agent did:
state created through the agent must be valid for local commands, and local and
remote `ps`, `counters`, and `exec` must agree.

Environment:
  TOPOLAB_E2E_REMOTE    SSH destination of the host (default: localhost)
  TOPOLAB_E2E_SSH_OPTS  comma-separated ssh -o options, for example
                        IdentityFile=/root/.ssh/id_ed25519,StrictHostKeyChecking=accept-new

The account must be root or must have a NOPASSWD sudoers rule for the agent.
The suite installs the agent for this build under /usr/local/lib/topolab/.
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import (  # noqa: E402
    Harness,
    E2EError,
    pcapng_packet_count,
    require,
    run_suite,
    wait_for,
)


def destination() -> str:
    return os.environ.get("TOPOLAB_E2E_REMOTE", "localhost")


def ssh_options() -> list[str]:
    options: list[str] = []
    for item in os.environ.get("TOPOLAB_E2E_SSH_OPTS", "").split(","):
        if item.strip():
            options += ["--ssh-opt", item.strip()]
    return options


def remote_command(harness: Harness, *args: str) -> tuple[str, ...]:
    return (
        str(harness.binary),
        "--remote",
        destination(),
        *ssh_options(),
        "--file",
        str(harness.topology),
        "--working-dir",
        str(harness.working_dir),
        *map(str, args),
    )


def remote(
    harness: Harness,
    *args: str,
    check: bool = True,
    timeout: float | None = 120,
    input: str | None = None,
) -> subprocess.CompletedProcess[str]:
    command = remote_command(harness, *args)
    result = subprocess.run(
        command,
        input=input,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=harness.environment,
        timeout=timeout,
    )
    harness._record(command, result)
    if check and result.returncode:
        raise E2EError(
            f"remote command failed ({result.returncode}): {' '.join(command)}\n{result.stderr.strip()}"
        )
    return result


def remote_json(harness: Harness, *args: str) -> Any:
    return json.loads(remote(harness, *args).stdout)


def local_json(harness: Harness, *args: str) -> Any:
    return json.loads(harness.run(*args).stdout)


def child_ssh_pids(pid: int) -> list[int]:
    result = subprocess.run(
        ("pgrep", "-P", str(pid), "-x", "ssh"),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )
    return [int(item) for item in result.stdout.split()]


REMOTE_TEMPLATE_TOPOLOGY = """\
name: remotetemplate
templates:
  - {{name: base, path: {path}, remote: true}}
storages:
  - {{name: local, type: file, root_provisioning: snapshot}}
vms:
  - name: vm
    template: base
    root_disk: {{storage: local}}
    ssh_authorized_keys:
      - "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test"
    interfaces: [{{name: eth0}}]
switches: [{{name: lan}}]
links:
  - name: vm-lan
    endpoints:
      - {{node: vm, interface: eth0}}
      - {{node: lan, interface: vm}}
"""


def main(harness: Harness) -> None:
    # `config` never contacts the host: an unreachable destination is fine.
    unreachable = subprocess.run(
        (
            str(harness.binary),
            "--remote",
            "unreachable.invalid",
            "--file",
            str(harness.topology),
            "--working-dir",
            str(harness.working_dir),
            "config",
        ),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=harness.environment,
        timeout=30,
    )
    require(
        unreachable.returncode == 0 and "name: network" in unreachable.stdout,
        f"remote config contacted the host: {unreachable.stderr}",
    )

    # Install the agent for this build. A second install changes nothing.
    first = remote(harness, "agent", "install")
    second = remote(harness, "agent", "install")
    require(
        "agent installed" in first.stdout or "already installed" in first.stdout,
        f"unexpected install output: {first.stdout!r}",
    )
    require(
        "already installed" in second.stdout,
        f"second install was not a no-op: {second.stdout!r}",
    )

    # Bring the topology up through a job and read it back.
    up = remote(harness, "up")
    jobs = remote(harness, "jobs", "ls").stdout
    require("succeeded" in jobs and "up" in jobs, f"jobs ls lacks the up job:\n{jobs}")
    job_id = jobs.splitlines()[1].split()[0]
    shown = json.loads(remote(harness, "jobs", "show", job_id).stdout)
    require(shown["state"] == "succeeded", f"job state is {shown['state']}")
    require(
        "secret" not in json.dumps(shown) and "env" not in shown,
        "jobs show leaked request data",
    )
    replay = remote(harness, "jobs", "attach", job_id)
    require(replay.stdout == up.stdout, "jobs attach did not replay the up output")
    require(
        not (
            harness.working_dir / ".topolab" / "jobs" / "network" / job_id / "secrets.json"
        ).exists(),
        "secrets.json remains after the job ended",
    )

    # A template with remote: true that is missing on the host fails before any job
    # exists. The suite runs on the host, so this path is also absent here.
    remote_template_file = harness.test_dir / "remote-template.yml"
    remote_template_file.write_text(
        REMOTE_TEMPLATE_TOPOLOGY.format(path=harness.test_dir / "no-such-template.qcow2")
    )
    command = list(remote_command(harness, "up"))
    command[command.index("--file") + 1] = str(remote_template_file)
    refused = subprocess.run(
        command,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=harness.environment,
        timeout=60,
    )
    require(
        refused.returncode == 1 and "host_template_invalid" in refused.stderr,
        f"missing remote template file was not refused: {refused.returncode} {refused.stderr!r}",
    )
    require(
        not (harness.working_dir / ".topolab" / "jobs" / "remotetemplate").exists(),
        "a job was created for a missing remote template file",
    )

    # Local and remote views of the same host agree.
    require(
        remote_json(harness, "ps", "--format", "json")
        == local_json(harness, "ps", "--format", "json"),
        "remote and local ps --format json differ",
    )
    require(
        remote_json(harness, "counters", "--format", "json")["links"]
        and local_json(harness, "counters", "--format", "json")["links"],
        "counters returned no links",
    )
    local_ns = harness.exec("ip", "-o", "addr").stdout
    require(
        remote(harness, "exec", "--", "ip", "-o", "addr").stdout == local_ns,
        "remote exec did not run in the management-host namespace",
    )
    require(
        remote(harness, "router", "exec", "left", "--", "ip", "-o", "link").stdout
        == harness.run("router", "exec", "left", "--", "ip", "-o", "link").stdout,
        "remote router exec output differs",
    )
    status = remote(
        harness, "exec", "--", "sh", "-c", "exit 7", check=False
    ).returncode
    require(status == 7, f"remote exec status was {status}, want 7")
    echoed = remote(harness, "exec", "--", "cat", input="through stdin\n").stdout
    require(echoed == "through stdin\n", f"stdin was not forwarded: {echoed!r}")

    # Capture writes on the controller, never under the host's working directory.
    output = harness.test_dir / "remote-capture.pcapng"
    capture = subprocess.Popen(
        remote_command(
            harness,
            "capture",
            "left-lan",
            "--endpoint",
            "left:trunk",
            "--duration",
            "4s",
            "--output",
            str(output),
        ),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=harness.environment,
    )
    time.sleep(1.5)
    harness.exec("ping", "-c", "2", "-W", "2", "192.0.2.1", check=False)
    stdout, stderr = capture.communicate(timeout=30)
    require(capture.returncode == 0, f"remote capture failed: {stderr}")
    require(str(output) in stdout, "capture did not print the controller path")
    require(
        pcapng_packet_count(output) > 0, "the remote capture holds no packets"
    )
    require(
        not list((harness.working_dir / "captures").glob("**/*.pcapng")),
        "remote capture left a file under the host working directory",
    )
    refused = remote(harness, "capture", "left-lan", "--endpoint", "left:trunk", "--duration", "1s", "--output", str(output), check=False)
    require(refused.returncode != 0, "capture overwrote an existing file")

    # A second lifecycle command is refused while a job is running, and a job
    # survives the loss of its SSH connection.
    remote(harness, "down")
    process = subprocess.Popen(
        (
            str(harness.binary),
            "--remote",
            destination(),
            *ssh_options(),
            "--reconnect-timeout",
            "0",
            "--file",
            str(harness.topology),
            "--working-dir",
            str(harness.working_dir),
            "up",
        ),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=harness.environment,
    )

    def job_listed() -> bool:
        return "up" in remote(harness, "jobs", "ls").stdout

    wait_for("the up job to be listed", job_listed, timeout=30, interval=0.1)
    for pid in child_ssh_pids(process.pid):
        os.kill(pid, signal.SIGKILL)
    code = process.wait(timeout=60)
    require(code in (0, 3), f"controller exit status after the kill was {code}")

    def job_done() -> bool:
        rows = remote(harness, "jobs", "ls").stdout.splitlines()[1:]
        return bool(rows) and all(
            row.split()[3] in ("succeeded", "failed", "cancelled", "lost")
            for row in rows
        )

    wait_for("the job to finish after the connection loss", job_done, timeout=120)
    ps = remote_json(harness, "ps", "--format", "json")
    require(ps, "ps is empty after the job survived the connection loss")
    require(
        remote(harness, "jobs", "ls").stdout.count("succeeded") >= 2,
        "the job did not succeed after the connection was lost",
    )

    remote(harness, "templates", "ls")
    remote(harness, "templates", "prune")
    remote(harness, "jobs", "prune")

    remote(harness, "down")
    gone = remote(harness, "ps", check=False)
    require(gone.returncode != 0, "ps succeeded after down")


if __name__ == "__main__":
    run_suite("network", main, required_tools=("tc", "bridge", "sysctl"))
