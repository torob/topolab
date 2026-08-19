#!/usr/bin/env python3
"""Shared, standard-library-only support for TopoLab's opt-in E2E tests."""

from __future__ import annotations

import contextlib
import fcntl
import hashlib
import json
import os
import platform
import shutil
import signal
import shlex
import struct
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path
from typing import Any, Callable, Iterable, Iterator, Mapping, Sequence


REPOSITORY = Path(__file__).resolve().parents[1]
IMAGE_RELEASE = "20260720"
IMAGE_NAME = "ubuntu-26.04-server-cloudimg-amd64.img"
IMAGE_URL = (
    "https://cloud-images.ubuntu.com/releases/server/26.04/"
    f"release-{IMAGE_RELEASE}/{IMAGE_NAME}"
)
IMAGE_SHA256 = "117816726abbdefc5ef3e38902e81a76f1c76c3610e709999d0885f9d5d9b477"
SSH_OPTIONS = (
    "-o",
    "BatchMode=yes",
    "-o",
    "IdentitiesOnly=yes",
    "-o",
    "StrictHostKeyChecking=accept-new",
    "-o",
    "ConnectTimeout=8",
    "-o",
    "ServerAliveInterval=5",
    "-o",
    "ServerAliveCountMax=3",
)


class E2EError(RuntimeError):
    """A test assertion or command failure with useful user-facing context."""


def require(condition: bool, message: str) -> None:
    if not condition:
        raise E2EError(message)


def wait_for(
    description: str,
    check: Callable[[], bool],
    timeout: float = 180,
    interval: float = 2,
) -> None:
    deadline = time.monotonic() + timeout
    last_error: BaseException | None = None
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except (OSError, subprocess.SubprocessError, E2EError) as error:
            last_error = error
        time.sleep(interval)
    suffix = f": {last_error}" if last_error else ""
    raise E2EError(f"timed out waiting for {description}{suffix}")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(4 * 1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def cached_template() -> Path:
    """Return TOPOLAB_TEMPLATE or atomically fetch the pinned official image."""
    override = os.environ.get("TOPOLAB_TEMPLATE")
    if override:
        path = Path(override).expanduser().resolve()
        require(path.is_file(), f"TOPOLAB_TEMPLATE is not a file: {path}")
        return path

    cache_home = Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache"))
    cache = cache_home / "topolab-e2e"
    cache.mkdir(mode=0o700, parents=True, exist_ok=True)
    destination = cache / f"ubuntu-26.04-{IMAGE_RELEASE}-amd64.img"
    lock_path = cache / f".{destination.name}.lock"
    with lock_path.open("a+b") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if destination.is_file() and sha256(destination) == IMAGE_SHA256:
            return destination
        if destination.exists():
            destination.unlink()
        temporary = cache / f".{destination.name}.{os.getpid()}.download"
        temporary.unlink(missing_ok=True)
        print(
            f"downloading pinned Ubuntu {IMAGE_RELEASE} cloud image to {destination}",
            flush=True,
        )
        try:
            request = urllib.request.Request(
                IMAGE_URL, headers={"User-Agent": "topolab-e2e/1"}
            )
            with (
                urllib.request.urlopen(request, timeout=60) as response,
                temporary.open("xb") as output,
            ):
                while block := response.read(4 * 1024 * 1024):
                    output.write(block)
                output.flush()
                os.fsync(output.fileno())
            actual = sha256(temporary)
            require(
                actual == IMAGE_SHA256,
                f"Ubuntu image SHA-256 mismatch: got {actual}, want {IMAGE_SHA256}",
            )
            temporary.chmod(0o600)
            temporary.replace(destination)
        finally:
            temporary.unlink(missing_ok=True)
    return destination


def _normalize_json(value: Any) -> Any:
    if isinstance(value, list):
        return [_normalize_json(item) for item in value]
    if isinstance(value, dict):
        ignored = {"gc_timer", "hello_timer", "tcn_timer", "topology_change_timer"}
        return {
            key: _normalize_json(item)
            for key, item in sorted(value.items())
            if key not in ignored
        }
    return value


def root_network_snapshot() -> dict[str, Any]:
    """Snapshot root state that non-external topologies must leave unchanged."""

    def output(*command: str) -> str:
        return subprocess.run(
            command,
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        ).stdout

    routes = json.loads(output("ip", "-j", "-4", "route", "show", "table", "all"))
    # Tailscale updates its per-peer table asynchronously. Those unrelated
    # control-plane route changes are common during multi-minute VM suites and
    # must not masquerade as a TopoLab teardown leak.
    routes = [route for route in routes if route.get("dev") != "tailscale0"]

    return {
        "links": _normalize_json(json.loads(output("ip", "-j", "-d", "link", "show"))),
        "routes": _normalize_json(routes),
        "forward": output("sysctl", "-n", "net.ipv4.ip_forward").strip(),
        "nft": output("nft", "--stateless", "list", "ruleset"),
    }


def pcapng_blocks(path: Path) -> Iterator[tuple[int, bytes]]:
    data = path.read_bytes()
    offset = 0
    while offset < len(data):
        require(offset + 12 <= len(data), f"truncated pcapng block in {path}")
        kind, size = struct.unpack_from("<II", data, offset)
        require(
            size >= 12 and offset + size <= len(data), f"invalid pcapng block in {path}"
        )
        trailer = struct.unpack_from("<I", data, offset + size - 4)[0]
        require(trailer == size, f"mismatched pcapng block length in {path}")
        yield kind, data[offset : offset + size]
        offset += size


def pcapng_packet_count(path: Path) -> int:
    return sum(kind == 6 for kind, _ in pcapng_blocks(path))


def pcapng_frames(path: Path) -> Iterator[bytes]:
    for kind, block in pcapng_blocks(path):
        if kind != 6:
            continue
        captured = struct.unpack_from("<I", block, 20)[0]
        yield block[28 : 28 + captured]


def ethernet_ipv4(frame: bytes) -> tuple[bytes, bytes, int, bool] | None:
    """Return (source, destination, protocol, VLAN-present) for an IPv4 frame."""
    if len(frame) < 34:
        return None
    ethertype = struct.unpack_from("!H", frame, 12)[0]
    offset, tagged = 14, False
    if ethertype in (0x8100, 0x88A8) and len(frame) >= 38:
        ethertype = struct.unpack_from("!H", frame, 16)[0]
        offset, tagged = 18, True
    if ethertype != 0x0800 or len(frame) < offset + 20:
        return None
    return (
        frame[offset + 12 : offset + 16],
        frame[offset + 16 : offset + 20],
        frame[offset + 9],
        tagged,
    )


def link_counter(report: Mapping[str, Any], name: str) -> Mapping[str, Any]:
    for link in report.get("links", []):
        if link.get("name") == name:
            return link
    raise E2EError(f"counter report omitted link {name}")


def counter_value(endpoint: Mapping[str, Any], section: str, name: str) -> int:
    value = endpoint.get(section, {}).get(name)
    return int(value or 0)


def dnsmasq_leases(path: Path) -> list[dict[str, str]]:
    """Parse dnsmasq's stable five-column lease file."""
    leases: list[dict[str, str]] = []
    for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        fields = line.split()
        require(len(fields) == 5, f"invalid dnsmasq lease line {number} in {path}")
        leases.append(
            dict(
                zip(
                    ("expires", "mac", "address", "hostname", "client_id"),
                    fields,
                    strict=True,
                )
            )
        )
    return leases


class Harness:
    """Own one disposable topology run and preserve its evidence on failure."""

    def __init__(
        self,
        name: str,
        *,
        template: bool = False,
        required_tools: Iterable[str] = (),
        generated_topology: str | None = None,
        external_cleanup: Callable[["Harness", bool], None] | None = None,
        test_directory: Path | None = None,
    ) -> None:
        self.name = name
        self.repository = REPOSITORY
        self.example = REPOSITORY / "examples" / name / "topology.yml"
        # Runtime paths include several nested components and Unix sockets are
        # limited to 107 bytes on Linux. Keep the disposable root deliberately
        # short so suites with longer topology and node names remain portable.
        self._owns_test_dir = test_directory is None
        self.test_dir = Path(tempfile.mkdtemp(prefix=f"tl-{name}-")) if test_directory is None else test_directory.resolve()
        self.test_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.working_dir = self.test_dir / "work"
        self.working_dir.mkdir(mode=0o700)
        self.topology = self.test_dir / "topology.yml"
        if generated_topology is None:
            shutil.copyfile(self.example, self.topology)
        else:
            self.topology.write_text(generated_topology, encoding="utf-8")
        self.log_path = self.test_dir / "commands.log"
        self.environment = os.environ.copy()
        binary = os.environ.get("TOPOLAB_BINARY", str(REPOSITORY / "bin" / "topolab"))
        self.binary = Path(binary).expanduser().resolve()
        self.key: Path | None = None
        self.background: list[subprocess.Popen[str]] = []
        self._finished = False
        self.external_cleanup = external_cleanup
        try:
            self._preflight(required_tools)
            if template:
                self.environment["TOPOLAB_TEMPLATE"] = str(cached_template())
                self._make_key()
        except BaseException:
            if self._owns_test_dir:
                shutil.rmtree(self.test_dir, ignore_errors=True)
            raise

    def _preflight(self, required_tools: Iterable[str]) -> None:
        require(
            os.geteuid() == 0, "E2E tests require effective UID 0; use sudo python3 ..."
        )
        require(
            platform.system() == "Linux" and platform.machine() in ("x86_64", "amd64"),
            "E2E tests require Linux/x86-64",
        )
        require(
            self.binary.is_file() and os.access(self.binary, os.X_OK),
            f"TopoLab binary is not executable: {self.binary}; run make build or set TOPOLAB_BINARY",
        )
        require(
            os.path.exists("/dev/kvm") and os.access("/dev/kvm", os.R_OK | os.W_OK),
            "/dev/kvm is missing or not read/write",
        )
        tools = {
            "ip",
            "nft",
            "qemu-system-x86_64",
            "qemu-img",
            "xorriso",
            *required_tools,
        }
        missing = sorted(tool for tool in tools if shutil.which(tool) is None)
        require(not missing, f"missing host tools: {', '.join(missing)}")

    def _make_key(self) -> None:
        require(shutil.which("ssh-keygen") is not None, "ssh-keygen is required")
        self.key = self.test_dir / "id_ed25519"
        subprocess.run(
            ("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(self.key)),
            check=True,
        )
        # `topolab exec` intentionally runs its payload as the original sudo
        # caller. Let that caller traverse the private harness directory, read
        # the ephemeral key, and create known_hosts without making either
        # artifact accessible to other users.
        caller_uid = int(os.environ.get("SUDO_UID", "0"))
        caller_gid = int(os.environ.get("SUDO_GID", "0"))
        if caller_uid:
            os.chown(self.test_dir, caller_uid, caller_gid)
            os.chown(self.key, caller_uid, caller_gid)
            os.chown(self.key.with_suffix(".pub"), caller_uid, caller_gid)
            self.test_dir.chmod(0o700)
            self.key.chmod(0o600)
        self.environment["TOPOLAB_SSH_PUBLIC_KEY"] = (
            self.key.with_suffix(".pub").read_text().strip()
        )

    @property
    def manifest_path(self) -> Path:
        return (
            self.working_dir / ".topolab" / "topologies" / self.name / "manifest.json"
        )

    def manifest(self) -> dict[str, Any]:
        with self.manifest_path.open(encoding="utf-8") as stream:
            return json.load(stream)

    def replace_topology(self, old: str, new: str, *, count: int = -1) -> None:
        text = self.topology.read_text(encoding="utf-8")
        changed = text.replace(old, new, count)
        require(changed != text, f"topology replacement did not match: {old!r}")
        self.topology.write_text(changed, encoding="utf-8")

    def command(self, *args: str, topology: Path | None = None) -> tuple[str, ...]:
        return (
            str(self.binary),
            "--file",
            str(topology or self.topology),
            "--working-dir",
            str(self.working_dir),
            *map(str, args),
        )

    def _record(
        self, command: Sequence[str], result: subprocess.CompletedProcess[str]
    ) -> None:
        with self.log_path.open("a", encoding="utf-8") as log:
            log.write(f"\n$ {' '.join(command)}\n")
            if result.stdout:
                log.write(result.stdout)
            if result.stderr:
                log.write("[stderr]\n" + result.stderr)
            log.write(f"[exit {result.returncode}]\n")

    def run(
        self,
        *args: str,
        check: bool = True,
        input: str | None = None,
        topology: Path | None = None,
        timeout: float | None = None,
    ) -> subprocess.CompletedProcess[str]:
        command = self.command(*args, topology=topology)
        result = subprocess.run(
            command,
            input=input,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=self.environment,
            timeout=timeout,
        )
        self._record(command, result)
        if check and result.returncode:
            raise E2EError(
                f"command failed ({result.returncode}): {' '.join(command)}\n{result.stderr.strip()}"
            )
        return result

    def popen(self, *args: str, topology: Path | None = None) -> subprocess.Popen[str]:
        command = self.command(*args, topology=topology)
        log = self.log_path.open("a", encoding="utf-8")
        log.write(f"\n$ {' '.join(command)}\n")
        log.flush()
        process = subprocess.Popen(
            command,
            text=True,
            stdout=log,
            stderr=subprocess.STDOUT,
            env=self.environment,
        )
        process._topolab_log = log  # type: ignore[attr-defined]
        self.background.append(process)
        return process

    def wait_process(
        self, process: subprocess.Popen[str], timeout: float | None = None
    ) -> None:
        code = process.wait(timeout=timeout)
        log = getattr(process, "_topolab_log", None)
        if log:
            log.write(f"[exit {code}]\n")
            log.close()
        with contextlib.suppress(ValueError):
            self.background.remove(process)
        require(code == 0, f"background command exited {code}; see {self.log_path}")

    def exec(
        self,
        *command: str,
        check: bool = True,
        input: str | None = None,
        timeout: float | None = None,
    ) -> subprocess.CompletedProcess[str]:
        return self.run(
            "exec", "--", *command, check=check, input=input, timeout=timeout
        )

    def ssh(
        self,
        address: str,
        *remote: str,
        check: bool = True,
        input: str | None = None,
        timeout: float | None = None,
    ) -> subprocess.CompletedProcess[str]:
        require(self.key is not None, "SSH key was not requested for this harness")
        remote_command = (
            remote[0]
            if len(remote) == 1
            else " ".join(shlex.quote(item) for item in remote)
        )
        command = (
            "ssh",
            "-i",
            str(self.key),
            *SSH_OPTIONS,
            "-o",
            f"UserKnownHostsFile={self.test_dir / 'known_hosts'}",
            f"torob@{address}",
            remote_command,
        )
        return self.exec(*command, check=check, input=input, timeout=timeout)

    def wait_ssh(self, address: str, timeout: float = 300) -> None:
        # VM restart/recreation is an explicit part of the lifecycle suites and
        # a fresh overlay can legitimately generate a new ephemeral host key.
        # Start each readiness cycle with an empty lab-local trust database,
        # then let StrictHostKeyChecking=accept-new enroll the current guest.
        (self.test_dir / "known_hosts").unlink(missing_ok=True)

        def ready() -> bool:
            return self.ssh(address, "true", check=False, timeout=15).returncode == 0

        wait_for(f"SSH on {address}", ready, timeout=timeout)
        self.ssh(
            address, "timeout", "300s", "cloud-init", "status", "--wait", timeout=330
        )

    def counters(self, *links: str) -> dict[str, Any]:
        return json.loads(self.run("counters", "--format", "json", *links).stdout)

    def install_guest_file(
        self, address: str, source: Path, destination: str, mode: str = "0755"
    ) -> None:
        data = source.read_text(encoding="utf-8")
        temporary = "/tmp/topolab-e2e-install"
        self.ssh(address, "sh", "-c", f"umask 077; cat > {temporary}", input=data)
        self.ssh(
            address,
            "sudo",
            "install",
            "-o",
            "root",
            "-g",
            "root",
            "-m",
            mode,
            temporary,
            destination,
        )
        self.ssh(address, "rm", "-f", temporary)

    def write_evidence(self, name: str, value: Any) -> Path:
        path = self.test_dir / name
        if isinstance(value, (dict, list)):
            path.write_text(
                json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8"
            )
        else:
            path.write_text(str(value), encoding="utf-8")
        return path

    def diagnostics(self) -> None:
        print(f"collecting failure diagnostics in {self.test_dir}", file=sys.stderr)
        with contextlib.suppress(Exception):
            self.write_evidence("diagnostic-ps.txt", self.run("ps", check=False).stdout)
        with contextlib.suppress(Exception):
            logs = self.run(
                "logs", "--timestamps", "--tail", "1000", check=False
            )
            self.write_evidence(
                "diagnostic-serial.txt", logs.stdout + logs.stderr
            )
        with contextlib.suppress(Exception):
            network = self.exec(
                "sh",
                "-c",
                "ip -br address; echo routes; ip route show table all",
                check=False,
            )
            self.write_evidence(
                "diagnostic-management-network.txt",
                network.stdout + network.stderr,
            )
        with contextlib.suppress(Exception):
            self.write_evidence(
                "diagnostic-ip-netns.txt",
                subprocess.run(
                    ("ip", "netns", "list"), text=True, stdout=subprocess.PIPE
                ).stdout,
            )
        with contextlib.suppress(Exception):
            if self.manifest_path.exists():
                self.write_evidence("diagnostic-manifest.json", self.manifest())
        with contextlib.suppress(Exception):
            self.write_evidence("diagnostic-root.json", root_network_snapshot())

    def finish(self, success: bool) -> None:
        if self._finished:
            return
        self._finished = True
        for process in self.background:
            with contextlib.suppress(ProcessLookupError):
                process.send_signal(signal.SIGINT)
            with contextlib.suppress(subprocess.TimeoutExpired):
                process.wait(timeout=10)
            log = getattr(process, "_topolab_log", None)
            if log:
                log.close()
        if not success:
            self.diagnostics()
        with contextlib.suppress(Exception):
            self.run("down", check=False, timeout=120)
        if self.external_cleanup is not None:
            self.external_cleanup(self, success)
        keep = os.environ.get("TOPOLAB_E2E_KEEP") == "1" or not success
        if keep:
            reason = "requested" if success else "test failed"
            print(
                f"E2E directory retained ({reason}): {self.test_dir}", file=sys.stderr
            )
        else:
            shutil.rmtree(self.test_dir)


def run_suite(
    name: str,
    body: Callable[[Harness], None],
    *,
    template: bool = False,
    required_tools: Iterable[str] = (),
    generated_topology: str | None = None,
    external_cleanup: Callable[[Harness, bool], None] | None = None,
    test_directory: Path | None = None,
) -> None:
    harness: Harness | None = None
    try:
        harness = Harness(
            name,
            template=template,
            required_tools=required_tools,
            generated_topology=generated_topology,
            external_cleanup=external_cleanup,
            test_directory=test_directory,
        )
        body(harness)
    except BaseException as error:
        if harness:
            harness.finish(False)
        if isinstance(error, KeyboardInterrupt):
            print("interrupted", file=sys.stderr)
        else:
            print(f"{name} E2E failed: {error}", file=sys.stderr)
        raise SystemExit(1) from error
    else:
        harness.finish(True)
        print(f"{name} E2E passed")
