#!/usr/bin/env python3
"""Root-only loop-backed LVM acceptance suite for explicit storage backends.

Normal CI only syntax-checks this file. Run the destructive fixture explicitly:
    sudo python3 e2e/storage/test.py
Recover one exact retained fixture with:
    sudo python3 e2e/storage/test.py --cleanup /path/to/fixture-state.json
"""

from __future__ import annotations

import argparse
import contextlib
import json
import os
import platform
import random
import shutil
import shlex
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any, NoReturn, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from common import E2EError, Harness, require  # noqa: E402


TOOLS = (
    "losetup", "truncate", "pvcreate", "pvs", "vgcreate", "vgs", "lvcreate",
    "lvs", "lvextend", "lvremove", "vgremove", "pvremove", "qemu-img", "lvm",
    "dmsetup",
)
BACKING_BYTES = 24 << 30
SUITE_TIMEOUT = 45 * 60


def run(command: Sequence[str], *, check: bool = True, timeout: int = 120) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if check and result.returncode:
        raise E2EError(f"command failed ({result.returncode}): {' '.join(command)}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}")
    return result


def report(command: Sequence[str], section: str) -> list[dict[str, str]]:
    result = run((*command, "--reportformat", "json", "--units", "b", "--nosuffix"))
    payload = json.loads(result.stdout)
    return [row for item in payload.get("report", []) for row in item.get(section, [])]


def qmp_execute(path: str, command: str, arguments: dict[str, Any] | None = None) -> Any:
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(30)
        connection.connect(path)
        stream = connection.makefile("rwb")
        greeting = json.loads(stream.readline())
        require("QMP" in greeting, f"invalid QMP greeting from {path}: {greeting}")

        def request(identifier: str, execute: str, args: dict[str, Any] | None = None) -> Any:
            payload: dict[str, Any] = {"execute": execute, "id": identifier}
            if args is not None:
                payload["arguments"] = args
            stream.write(json.dumps(payload).encode() + b"\n")
            stream.flush()
            while True:
                response = json.loads(stream.readline())
                if response.get("id") != identifier:
                    continue
                require("error" not in response, f"QMP {execute} failed: {response.get('error')}")
                return response.get("return")

        request("capabilities", "qmp_capabilities")
        return request("command", command, arguments)


def atomic_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".fixture-state-", dir=path.parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            json.dump(value, stream, indent=2, sort_keys=True)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(temporary)


def loop_identity(device: str) -> dict[str, Any]:
    resolved = Path(device).resolve()
    info = resolved.stat()
    require(stat.S_ISBLK(info.st_mode), f"loop target is not a block device: {resolved}")
    output = run(("losetup", "--noheadings", "--output", "NAME,BACK-FILE", str(resolved))).stdout.strip()
    fields = output.split(maxsplit=1)
    require(len(fields) == 2 and Path(fields[0]).resolve() == resolved, f"loop mapping disappeared: {device}")
    return {"path": str(resolved), "major": os.major(info.st_rdev), "minor": os.minor(info.st_rdev), "backing": str(Path(fields[1]).resolve())}


class Fixture:
    def __init__(self, root: Path | None = None) -> None:
        self.root = root or Path(tempfile.mkdtemp(prefix="tl-storage-"))
        self.root.chmod(0o700)
        self.state_path = self.root / "fixture-state.json"
        self.state: dict[str, Any] = {"version": 1, "root": str(self.root), "owned_lvs": {}, "foreign_lvs": {}, "phase": "new"}
        self.save()

    @classmethod
    def load(cls, path: Path) -> "Fixture":
        require(path.is_file(), f"fixture state does not exist: {path}")
        value = json.loads(path.read_text(encoding="utf-8"))
        fixture = cls.__new__(cls)
        fixture.state_path, fixture.state = path.resolve(), value
        fixture.root = Path(value["root"]).resolve()
        require(fixture.state_path.parent == fixture.root, "fixture state must be directly inside its recorded root")
        return fixture

    def save(self) -> None:
        atomic_json(self.state_path, self.state)

    def setup(self) -> None:
        available = shutil.disk_usage(self.root).free
        require(available >= BACKING_BYTES, f"insufficient temporary filesystem capacity: available={available} required={BACKING_BYTES}")
        backing = (self.root / "lvm-backing.img").resolve()
        run(("truncate", "--size", str(BACKING_BYTES), str(backing)))
        self.state.update(backing=str(backing), backing_dev=backing.stat().st_dev, backing_ino=backing.stat().st_ino, phase="backing-created")
        self.save()
        loop = run(("losetup", "--find", "--show", str(backing))).stdout.strip()
        identity = loop_identity(loop)
        require(identity["backing"] == str(backing), "new loop device maps an unexpected backing file")
        self.state.update(loop=identity, phase="loop-attached")
        self.save()
        self.verify_loop()
        run(("pvcreate", "--yes", loop))
        pv = self.pv_row(loop)
        self.state.update(pv_uuid=pv["pv_uuid"].strip(), phase="pv-created")
        self.save()
        vg = f"tltest-{os.getpid()}-{random.SystemRandom().randrange(1 << 32):08x}"
        run(("vgcreate", vg, loop))
        vg_row = self.vg_row(vg)
        pv = self.pv_row(loop)
        self.state.update(vg=vg, vg_uuid=vg_row["vg_uuid"].strip(), pv_uuid=pv["pv_uuid"].strip(), phase="vg-created")
        self.save()

    def pv_row(self, device: str) -> dict[str, str]:
        rows = report(("pvs", "-o", "pv_name,pv_uuid,vg_name,vg_uuid"), "pv")
        matches = [row for row in rows if Path(row["pv_name"].strip()).resolve() == Path(device).resolve()]
        require(len(matches) == 1, f"expected exactly one recorded PV row for {device}, got {matches}")
        return matches[0]

    def vg_row(self, name: str) -> dict[str, str]:
        rows = report(("vgs", "-o", "vg_name,vg_uuid,pv_count,lv_count", name), "vg")
        matches = [row for row in rows if row["vg_name"].strip() == name]
        require(len(matches) == 1, f"expected exact VG {name}, got {matches}")
        return matches[0]

    def lvs(self) -> list[dict[str, str]]:
        return report(("lvs", "-a", "-o", "lv_name,lv_uuid,vg_name,vg_uuid,pool_lv,origin,lv_tags,lv_attr,lv_size,data_percent,metadata_percent", self.state["vg"]), "lv")

    def verify_loop(self) -> None:
        expected = self.state["loop"]
        actual = loop_identity(expected["path"])
        require(actual == expected, f"loop identity changed: expected={expected}, actual={actual}")
        backing = Path(expected["backing"])
        info = backing.stat()
        require(info.st_dev == self.state["backing_dev"] and info.st_ino == self.state["backing_ino"], "backing-file identity changed")

    def verify_vg(self) -> None:
        require(self.state["vg"].startswith("tltest-"), "refusing non-test VG")
        self.verify_loop()
        vg = self.vg_row(self.state["vg"])
        require(vg["vg_uuid"].strip() == self.state["vg_uuid"], "VG UUID changed")
        pv = self.pv_row(self.state["loop"]["path"])
        require(pv["pv_uuid"].strip() == self.state["pv_uuid"], "PV UUID changed")
        require(pv["vg_name"].strip() == self.state["vg"] and pv["vg_uuid"].strip() == self.state["vg_uuid"], "VG no longer consists of the recorded PV")
        require(int(vg["pv_count"].strip()) == 1, "VG contains an unexpected PV")

    def register_inventory(self, foreign: bool = False, names: Sequence[str] = ()) -> None:
        self.verify_vg()
        target = "foreign_lvs" if foreign else "owned_lvs"
        selected = set(names)
        for row in self.lvs():
            name = row["lv_name"].strip()
            if name.startswith("["):
                continue
            tags = {item.strip() for item in row.get("lv_tags", "").split(",") if item.strip()}
            if (foreign and name in selected) or (not foreign and "topolab" in tags):
                self.state[target][name] = {"uuid": row["lv_uuid"].strip(), "tags": sorted(tags)}
        self.save()

    def verify_registered_inventory(self) -> None:
        self.verify_vg()
        registered = {**self.state.get("owned_lvs", {}), **self.state.get("foreign_lvs", {})}
        rows = {row["lv_name"].strip(): row for row in self.lvs() if not row["lv_name"].strip().startswith("[")}
        require(not (set(rows) - set(registered)), f"unregistered LVs exist: {sorted(set(rows) - set(registered))}")
        for name, row in rows.items():
            require(row["lv_uuid"].strip() == registered[name]["uuid"], f"registered LV UUID changed for {name}")

    def diagnostics(self, harness: Harness | None, reason: BaseException) -> None:
        print(f"storage E2E failure: {reason}", file=sys.stderr)
        for name, command in {
            "pvs": ("pvs", "-a", "-o", "+pv_uuid,vg_uuid"),
            "vgs": ("vgs", "-a", "-o", "+vg_uuid"),
            "lvs": ("lvs", "-a", "-o", "+lv_uuid,vg_uuid,lv_tags,pool_lv,origin"),
            "loops": ("losetup", "--list"),
        }.items():
            with contextlib.suppress(Exception):
                (self.root / f"diagnostic-{name}.txt").write_text(run(command, check=False).stdout, encoding="utf-8")
        if harness and harness.manifest_path.exists():
            shutil.copyfile(harness.manifest_path, self.root / "diagnostic-manifest.json")
        print(f"fixture evidence retained: {self.state_path}", file=sys.stderr)
        print(f"safe recovery: sudo python3 {Path(__file__).resolve()} --cleanup {self.state_path}", file=sys.stderr)

    def cleanup(self, harness: Harness | None = None, *, preserve_root: bool = False) -> None:
        if self.state.get("phase") == "cleaned":
            shutil.rmtree(self.root)
            return
        if "vg" in self.state and not self.state.get("vg_removed"):
            self.verify_registered_inventory()
        # Stop only processes whose manifest gives an exact TopoLab identity.
        if harness and harness.manifest_path.exists():
            with contextlib.suppress(Exception):
                manifest = harness.manifest()
                for node, value in manifest.get("nodes", {}).items():
                    pid = int(value.get("pid", 0))
                    if pid > 0:
                        cmdline = Path(f"/proc/{pid}/cmdline")
                        if cmdline.exists() and f"topolab:{manifest['topology']}:{node}" in cmdline.read_bytes().replace(b"\0", b" ").decode(errors="replace"):
                            os.kill(pid, signal.SIGTERM)
                cleanup_topology = Path(self.state.get("cleanup_topology", harness.topology))
                harness.run("down", check=False, timeout=180, topology=cleanup_topology)
        if "vg" in self.state and not self.state.get("vg_removed"):
            self.verify_vg()
            rows = {row["lv_name"].strip(): row for row in self.lvs() if not row["lv_name"].strip().startswith("[")}
            registered = {**self.state.get("owned_lvs", {}), **self.state.get("foreign_lvs", {})}
            unknown = sorted(set(rows) - set(registered))
            require(not unknown, f"cleanup stopped: unregistered LVs exist in fixture VG: {unknown}")
            # Remove explicit dependency leaves before origins and pools.
            while rows:
                self.verify_vg()
                current = {row["lv_name"].strip(): row for row in self.lvs()}
                candidates = [name for name, row in rows.items() if row.get("lv_attr", "")[:1] != "t" and not any(other.get("origin", "").strip() == name for other in rows.values())]
                if not candidates:
                    candidates = [name for name, row in rows.items() if row.get("lv_attr", "")[:1] == "t"]
                require(candidates, f"cannot determine dependency-safe LV cleanup order: {sorted(rows)}")
                name = sorted(candidates)[0]
                require(current[name]["lv_uuid"].strip() == registered[name]["uuid"], f"LV UUID changed for {name}")
                run(("lvremove", "--yes", f"{self.state['vg']}/{name}"))
                rows.pop(name)
            require(not [row for row in self.lvs() if not row["lv_name"].strip().startswith("[")], "VG still has LVs")
            self.verify_vg()
            run(("vgremove", "--yes", self.state["vg"]))
            Path(f"/run/lock/topolab-vg-{self.state['vg_uuid'].replace('/', '_')}.lock").unlink(missing_ok=True)
            self.state.update(vg_removed=True, phase="vg-removed")
            self.save()
        if "pv_uuid" in self.state and not self.state.get("pv_removed"):
            self.verify_loop()
            pv = self.pv_row(self.state["loop"]["path"])
            require(pv["pv_uuid"].strip() == self.state["pv_uuid"] and not pv["vg_name"].strip(), "PV identity or VG membership changed")
            run(("pvremove", "--yes", self.state["loop"]["path"]))
            self.state.update(pv_removed=True, phase="pv-removed")
            self.save()
        if "loop" in self.state and not self.state.get("loop_detached"):
            self.verify_loop()
            rows = report(("pvs", "-o", "pv_name,pv_uuid,vg_name,vg_uuid"), "pv")
            require(not any(Path(row["pv_name"].strip()).resolve() == Path(self.state["loop"]["path"]).resolve() for row in rows), "test PV signature remains before loop detach")
            run(("losetup", "--detach", self.state["loop"]["path"]))
            self.state.update(loop_detached=True, phase="loop-detached")
            self.save()
        if preserve_root:
            self.state["phase"] = "cleaned"
            self.save()
        else:
            shutil.rmtree(self.root)


def vm_block(number: int, storage: str, address: str) -> str:
    key = "${TOPOLAB_SSH_PUBLIC_KEY:?set by harness}"
    data_storage = "thick-wipe" if number == 1 else "thick" if number % 3 == 1 else "thin-copy" if number % 3 == 2 else "file-copy"
    cache = ", cache: writeback" if number == 1 else ""
    aio = ", aio: threads" if number == 1 else ", aio: native" if number == 4 else ""
    disks = [f"      - {{name: data-{number}, size_gib: 1, storage: {data_storage}{cache}{aio}}}"]
    if number == 2:
        disks.append("      - {name: pressure, size_gib: 6, storage: thin-copy}")
    return f"""  - name: vm{number}
    template: ubuntu
    memory_mb: 768
    root_disk: {{storage: {storage}, size_gib: 4}}
    additional_disks:
{os.linesep.join(disks)}
    ssh_authorized_keys: [\"{key}\"]
    interfaces:
      - name: eth0
        ipv4: {{addresses: [{address}/24]}}
"""


def topology(vg: str) -> str:
    template = "${TOPOLAB_TEMPLATE:?set by harness}"
    roots = (("file-snapshot", "10.77.0.11"), ("file-copy", "10.77.0.12"), ("thin-copy", "10.77.0.13"), ("thin-copy", "10.77.0.14"), ("thin-snapshot", "10.77.0.15"), ("thin-snapshot", "10.77.0.16"))
    vm_yaml = [vm_block(number, storage, address) for number, (storage, address) in enumerate(roots, 1)]
    links = "\n".join(f"  - {{name: vm{i}-lan, endpoints: [{{node: vm{i}, interface: eth0}}, {{node: lan, interface: vm{i}}}]}}" for i in range(1, 7))
    return f"""name: storage
templates:
  - {{name: ubuntu, path: \"{template}\"}}
storages:
  - {{name: file-snapshot, type: file, root_provisioning: snapshot}}
  - {{name: file-copy, type: file, root_provisioning: copy}}
  - {{name: thin-copy, type: lvm-thin, volume_group: {vg}, pool_size_gib: 8, root_provisioning: copy}}
  - {{name: thin-snapshot, type: lvm-thin, volume_group: {vg}, pool_size_gib: 6, root_provisioning: snapshot, zero_new_blocks: true}}
  - {{name: thick, type: lvm-thick, volume_group: {vg}}}
  - {{name: thick-wipe, type: lvm-thick, volume_group: {vg}, wipe_on_create: true}}
management_host:
  interfaces:
    - name: lan
      ipv4: {{addresses: [10.77.0.1/24]}}
vms:
{''.join(vm_yaml)}switches: [{{name: lan}}]
links:
  - {{name: management-lan, endpoints: [{{node: management_host, interface: lan}}, {{node: lan, interface: management}}]}}
{links}
"""


def inventory(fixture: Fixture) -> dict[str, tuple[str, str, str]]:
    return {row["lv_name"].strip(): (row["lv_uuid"].strip(), row.get("pool_lv", "").strip(), row.get("origin", "").strip()) for row in fixture.lvs() if not row["lv_name"].strip().startswith("[")}


def write_zero_extent(path: str, offset: int, length: int) -> None:
    block = bytes(1 << 20)
    descriptor = os.open(path, os.O_WRONLY)
    try:
        os.lseek(descriptor, offset, os.SEEK_SET)
        for _ in range(length // len(block)):
            require(os.write(descriptor, block) == len(block), f"short block-device write to {path}")
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def assert_storage_layout(harness: Harness, fixture: Fixture, auto_aio: str) -> None:
    manifest = harness.manifest()
    require(manifest["version"] == 3 and len(manifest["storages"]) == 6, "manifest lacks explicit storage identities")
    values = inventory(fixture)
    rows = {row["lv_name"].strip(): row for row in fixture.lvs()}
    pools = [value for value in manifest["storages"].values() if value["type"] == "lvm-thin"]
    require(len(pools) == 2 and all(value["pool_name"] in values for value in pools), "expected one pool per thin storage")
    for storage in pools:
        row = rows[storage["pool_name"]]
        require(row["lv_uuid"].strip() == storage["pool_uuid"] and row["vg_uuid"].strip() == storage["vg_uuid"] == fixture.state["vg_uuid"], "pool manifest identity differs from LVM")
        attr = row.get("lv_attr", "")
        require(len(attr) > 7 and (attr[7] == "z") == storage.get("zero_new_blocks", False), "thin pool zero mode differs from manifest")
    roots = [manifest["nodes"][f"vm{i}"]["root_disk"] for i in range(1, 7)]
    require(roots[2]["lv_uuid"] != roots[3]["lv_uuid"] and not values[roots[2]["lv_name"]][2] and not values[roots[3]["lv_name"]][2], "thin-copy roots are not independent")
    origins = {values[roots[index]["lv_name"]][2] for index in (4, 5)}
    require(len(origins) == 1 and next(iter(origins)), "thin-snapshot roots do not share one template base")
    base = rows[next(iter(origins))]
    require(len(base.get("lv_attr", "")) > 1 and base["lv_attr"][1] == "r", "thin template base is not read-only")
    for node in manifest["nodes"].values():
        for disk in [node["root_disk"], *node.get("additional_disks", [])]:
            if disk["format"] != "raw":
                continue
            row = rows[disk["lv_name"]]
            tags = {tag.strip() for tag in row.get("lv_tags", "").split(",")}
            require(row["lv_uuid"].strip() == disk["lv_uuid"] and row["vg_uuid"].strip() == disk["vg_uuid"] and set(disk["ownership_tags"]).issubset(tags), f"disk identity/tags differ for {disk['name']}")
            require(row.get("pool_lv", "").strip() == disk.get("pool_name", ""), f"pool membership differs for {disk['name']}")
            if disk["storage_type"] == "lvm-thick":
                require(row["lv_attr"].startswith("-wi"), f"thick disk is not a fully allocated linear LV: {row['lv_attr']}")
                require(disk.get("wipe_on_create", False) == (disk["storage"] == "thick-wipe"), f"thick wipe policy differs for {disk['name']}")
    for index in (0, 1):
        info = json.loads(run(("qemu-img", "info", "--force-share", "--output=json", roots[index]["path"])).stdout)
        if index == 0:
            require(bool(info.get("backing-filename")), "file snapshot has no backing template")
        else:
            require(not info.get("backing-filename"), "file copy unexpectedly has a backing file")
    for number in range(1, 7):
        pid = int(manifest["nodes"][f"vm{number}"]["pid"])
        argv = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
        blockdevs = [argv[index + 1].decode() for index, value in enumerate(argv[:-1]) if value == b"-blockdev"]
        blockdev_by_name = {
            next(field.removeprefix("node-name=") for field in argument.split(",") if field.startswith("node-name=")): argument
            for argument in blockdevs
        }
        node = manifest["nodes"][f"vm{number}"]
        named_nodes = {
            item["node-name"]: item
            for item in qmp_execute(node["qmp_path"], "query-named-block-nodes")
        }
        for disk in [node["root_disk"], *node.get("additional_disks", [])]:
            block_node = disk["block_node"]
            protocol = "host_device" if disk["format"] == "raw" else "file"
            protocol_node = "file-" + block_node.removeprefix("blk-")
            direct = not (number == 1 and disk["name"] == "data-1")
            direct_arg = "on" if direct else "off"
            aio = "threads" if number == 1 and disk["name"] == "data-1" else "native" if number == 4 and disk["name"] == "data-4" else auto_aio
            require(block_node in blockdev_by_name and blockdev_by_name[block_node].startswith(f"driver={disk['format']},"), f"QEMU format mismatch for vm{number}/{disk['name']}")
            require(protocol_node in blockdev_by_name and blockdev_by_name[protocol_node].startswith(f"driver={protocol},"), f"QEMU protocol mismatch for vm{number}/{disk['name']}")
            require(f"aio={aio}" in blockdev_by_name[protocol_node], f"QEMU AIO mismatch for vm{number}/{disk['name']}: {blockdev_by_name[protocol_node]}")
            require("aio=" not in blockdev_by_name[block_node], f"QEMU format node unexpectedly contains AIO for vm{number}/{disk['name']}: {blockdev_by_name[block_node]}")
            for layer in (block_node, protocol_node):
                argument = blockdev_by_name[layer]
                require(f"cache.direct={direct_arg}" in argument and "cache.no-flush=off" in argument, f"QEMU argv cache mismatch for vm{number}/{disk['name']} layer {layer}: {argument}")
                cache = named_nodes[layer]["cache"]
                require(cache["direct"] is direct and cache["no-flush"] is False, f"QMP cache mismatch for vm{number}/{disk['name']} layer {layer}: {cache}")
        if number == 1:
            template = Path(node["template_path"]).resolve()
            backing_protocols = [
                argument for argument in blockdevs
                if argument.startswith("driver=file,")
                and f"filename={template}" in argument
            ]
            require(len(backing_protocols) == 1 and f"aio={auto_aio}" in backing_protocols[0], f"qcow2 backing protocol has wrong AIO: {backing_protocols}")
            backing_nodes = [
                item for item in named_nodes.values()
                if item.get("drv") == "qcow2"
                and item.get("node-name") != node["root_disk"]["block_node"]
                and item.get("file")
                and Path(item["file"]).resolve() == template
            ]
            require(backing_nodes, "QMP omitted the qcow2 snapshot backing node")
            require(all(item["cache"]["direct"] is True and item["cache"]["no-flush"] is False for item in backing_nodes), f"qcow2 backing nodes are not direct I/O: {backing_nodes}")


def main() -> None:
    require(os.geteuid() == 0, "storage E2E requires effective UID 0; use sudo")
    require(platform.system() == "Linux" and platform.machine() in ("x86_64", "amd64"), "storage E2E requires Linux/x86-64")
    require(os.path.exists("/dev/kvm") and os.access("/dev/kvm", os.R_OK | os.W_OK), "/dev/kvm is not read/write")
    require(os.path.exists("/dev/mapper/control") and os.access("/dev/mapper/control", os.R_OK | os.W_OK), "device-mapper control is not writable")
    require("thin-pool" in run(("dmsetup", "targets")).stdout, "device-mapper thin-pool target is unavailable")
    missing = [tool for tool in TOOLS if shutil.which(tool) is None]
    require(not missing, f"missing storage E2E tools: {', '.join(missing)}")
    fixture = Fixture()
    harness: Harness | None = None
    failed = False
    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt(f"received signal {signum}")

    old_alarm = signal.signal(signal.SIGALRM, lambda _s, _f: (_ for _ in ()).throw(TimeoutError("storage suite timeout")))
    old_term = signal.signal(signal.SIGTERM, interrupted)
    old_hup = signal.signal(signal.SIGHUP, interrupted)
    signal.alarm(SUITE_TIMEOUT)
    try:
        fixture.setup()
        harness = Harness("storage", template=True, required_tools=TOOLS, generated_topology=topology(fixture.state["vg"]), test_directory=fixture.root)
        cleanup_topology = fixture.root / "cleanup-topology.yml"
        shutil.copyfile(harness.topology, cleanup_topology)
        fixture.state["cleanup_topology"] = str(cleanup_topology)
        fixture.save()
        initial_up = harness.run("up", timeout=900)
        auto_aio = "threads" if "aio auto is using threads" in initial_up.stderr else "io_uring"
        fixture.register_inventory()
        assert_storage_layout(harness, fixture, auto_aio)
        for number in range(1, 7):
            harness.wait_ssh(f"10.77.0.{10 + number}", timeout=420)
            harness.ssh(f"10.77.0.{10 + number}", "sudo", "sh", "-c", f"echo marker-{number} >/var/tmp/topolab-storage-marker")
        harness.run("stop")
        harness.run("start", timeout=600)
        harness.run("restart", "vm1", timeout=300)
        for number in range(1, 7):
            harness.wait_ssh(f"10.77.0.{10 + number}", timeout=300)
            require(harness.ssh(f"10.77.0.{10 + number}", "cat", "/var/tmp/topolab-storage-marker").stdout.strip() == f"marker-{number}", "guest marker did not persist")

        # Pool growth succeeds; a decrease is rejected without inventory mutation.
        harness.replace_topology("pool_size_gib: 8, root_provisioning: copy", "pool_size_gib: 9, root_provisioning: copy", count=1)
        harness.run("up", timeout=600)
        fixture.register_inventory()
        before = inventory(fixture)
        harness.replace_topology("pool_size_gib: 9, root_provisioning: copy", "pool_size_gib: 8, root_provisioning: copy", count=1)
        result = harness.run("up", check=False)
        require(result.returncode != 0 and "reduction" in result.stderr and inventory(fixture) == before, "thin-pool decrease mutated storage")
        harness.replace_topology("pool_size_gib: 8, root_provisioning: copy", "pool_size_gib: 9, root_provisioning: copy", count=1)

        # Controlled writes cross 80% without blocking; deleting that one disk removes only its LV.
        pressure = next(item for item in harness.manifest()["nodes"]["vm2"]["additional_disks"] if item["name"] == "pressure")
        pool_name = harness.manifest()["storages"]["thin-copy"]["pool_name"]
        written = 0
        while True:
            pool = next(row for row in fixture.lvs() if row["lv_name"].strip() == pool_name)
            if float(pool.get("data_percent", "0") or 0) >= 81:
                break
            require(written < 5 << 30, "controlled thin write could not reach 80%")
            write_zero_extent(pressure["path"], written, 256 << 20)
            written += 256 << 20
        harness.run("up", timeout=600)
        warnings = next(item for item in json.loads(harness.run("ps", "--format", "json").stdout)["storages"] if item["name"] == "thin-copy")["warnings"]
        require(any("utilization" in warning for warning in warnings), "80% thin-pool warning is absent")
        before = inventory(fixture)
        harness.replace_topology("      - {name: pressure, size_gib: 6, storage: thin-copy}\n", "")
        harness.run("up", timeout=600)
        fixture.register_inventory()
        after = inventory(fixture)
        require(set(before) - set(after) == {pressure["lv_name"]} and all(before[name] == after[name] for name in after), "removing one disk changed unrelated LVs")

        # Offline file growth and online LV growth preserve identity and never shrink.
        harness.ssh("10.77.0.13", "sudo", "sh", "-c", "printf offline-marker | dd of=/dev/sdb bs=1 seek=4096 conv=notrunc status=none")
        harness.run("stop", "vm3")
        harness.replace_topology("{name: data-3, size_gib: 1, storage: file-copy}", "{name: data-3, size_gib: 2, storage: file-copy}")
        harness.run("up", timeout=600)
        file_disk = next(item for item in harness.manifest()["nodes"]["vm3"]["additional_disks"] if item["name"] == "data-3")
        require(json.loads(run(("qemu-img", "info", "--force-share", "--output=json", file_disk["path"])).stdout)["virtual-size"] >= 2 << 30, "offline file growth was not applied")
        harness.wait_ssh("10.77.0.13", timeout=300)
        require(harness.ssh("10.77.0.13", "sudo", "dd", "if=/dev/sdb", "bs=1", "skip=4096", "count=14", "status=none").stdout == "offline-marker", "offline growth did not preserve disk content")
        require(int(harness.ssh("10.77.0.13", "sudo", "blockdev", "--getsize64", "/dev/sdb").stdout) >= 2 << 30, "guest did not observe offline growth")
        online_pid = int(harness.manifest()["nodes"]["vm2"]["pid"])
        online_before = next(item for item in harness.manifest()["nodes"]["vm2"]["additional_disks"] if item["name"] == "data-2")
        harness.ssh("10.77.0.12", "sudo", "sh", "-c", "printf online-marker | dd of=/dev/sdb bs=1 seek=4096 conv=notrunc status=none")
        harness.replace_topology("{name: data-2, size_gib: 1, storage: thin-copy}", "{name: data-2, size_gib: 2, storage: thin-copy}")
        harness.run("up", timeout=600)
        online_after = next(item for item in harness.manifest()["nodes"]["vm2"]["additional_disks"] if item["name"] == "data-2")
        require(int(harness.manifest()["nodes"]["vm2"]["pid"]) == online_pid and online_after["lv_uuid"] == online_before["lv_uuid"], "online LV growth restarted or replaced the VM disk")
        require(harness.ssh("10.77.0.12", "sudo", "dd", "if=/dev/sdb", "bs=1", "skip=4096", "count=13", "status=none").stdout == "online-marker", "online growth did not preserve disk content")
        require(int(harness.ssh("10.77.0.12", "sudo", "blockdev", "--getsize64", "/dev/sdb").stdout) >= 2 << 30, "guest did not observe online growth")
        before = inventory(fixture)
        harness.replace_topology("{name: data-2, size_gib: 2, storage: thin-copy}", "{name: data-2, size_gib: 1, storage: thin-copy}")
        result = harness.run("up", check=False)
        require(result.returncode != 0 and "shrink" in result.stderr and inventory(fixture) == before, "disk shrink mutated LVM")
        harness.replace_topology("{name: data-2, size_gib: 1, storage: thin-copy}", "{name: data-2, size_gib: 2, storage: thin-copy}")

        # Storage movement and provisioning changes fail before process/storage mutation.
        before = inventory(fixture); pids = {name: value["pid"] for name, value in harness.manifest()["nodes"].items()}
        harness.replace_topology("{name: data-2, size_gib: 2, storage: thin-copy}", "{name: data-2, size_gib: 2, storage: thick}")
        result = harness.run("up", check=False)
        require(result.returncode != 0 and inventory(fixture) == before and {name: value["pid"] for name, value in harness.manifest()["nodes"].items()} == pids, "storage movement mutated state")
        harness.replace_topology("{name: data-2, size_gib: 2, storage: thick}", "{name: data-2, size_gib: 2, storage: thin-copy}")
        harness.replace_topology("pool_size_gib: 9, root_provisioning: copy", "pool_size_gib: 9, root_provisioning: snapshot", count=1)
        result = harness.run("up", check=False)
        require(result.returncode != 0 and inventory(fixture) == before, "provisioning change mutated state")
        harness.replace_topology("pool_size_gib: 9, root_provisioning: snapshot", "pool_size_gib: 9, root_provisioning: copy", count=1)

        # Kill TopoLab immediately after one targeted LVM creation; the next up recovers the checkpoint.
        harness.replace_topology("      - {name: data-1, size_gib: 1, storage: thick-wipe, cache: writeback, aio: threads}", "      - {name: data-1, size_gib: 1, storage: thick-wipe, cache: writeback, aio: threads}\n      - {name: interrupted, size_gib: 1, storage: thick-wipe}")
        wrapper = fixture.root / "lvm-kill-wrapper"
        real_lvm = shutil.which("lvm")
        require(real_lvm is not None, "lvm disappeared")
        wrapper.write_text(f"#!/bin/sh\nset -eu\n{shlex.quote(real_lvm)} \"$@\"\ncase \" $* \" in *\" lvcreate \"*\" --name tld-\"*) kill -KILL \"$PPID\" ;; esac\n", encoding="utf-8")
        wrapper.chmod(0o700)
        killed = harness.run("--lvm", str(wrapper), "up", check=False, timeout=600)
        require(killed.returncode != 0, "LVM interruption wrapper did not terminate TopoLab")
        fixture.register_inventory()
        harness.run("up", timeout=600)
        fixture.register_inventory()
        interrupted = next(item for item in harness.manifest()["nodes"]["vm1"]["additional_disks"] if item["name"] == "interrupted")
        require(interrupted["lv_uuid"] and not interrupted.get("pending"), "interrupted LV was not recovered")

        status = json.loads(harness.run("ps", "--format", "json").stdout)
        require(status["schema_version"] == 2 and any(item["overcommit"] for item in status["storages"] if item["type"] == "lvm-thin"), "thin overcommit warning is absent")

        # Once all references are removed, deleting one thin declaration
        # removes only that storage's roots, shared base, and pool.
        harness.replace_topology("    additional_disks:\n      - {name: data-5, size_gib: 1, storage: thin-copy}\n", "")
        harness.replace_topology("    additional_disks:\n      - {name: data-6, size_gib: 1, storage: file-copy}\n", "")
        harness.run("up", timeout=600)
        fixture.register_inventory()
        storage_manifest = harness.manifest()
        thin_snapshot = storage_manifest["storages"]["thin-snapshot"]
        removed_lvs = {thin_snapshot["pool_name"], *(base["name"] for base in thin_snapshot.get("bases", []))}
        removed_lvs.update(storage_manifest["nodes"][name]["root_disk"]["lv_name"] for name in ("vm5", "vm6"))
        before = inventory(fixture)
        vm5 = vm_block(5, "thin-snapshot", "10.77.0.15").replace("    additional_disks:\n      - {name: data-5, size_gib: 1, storage: thin-copy}\n", "")
        vm6 = vm_block(6, "thin-snapshot", "10.77.0.16").replace("    additional_disks:\n      - {name: data-6, size_gib: 1, storage: file-copy}\n", "")
        harness.replace_topology(vm5, "")
        harness.replace_topology(vm6, "")
        harness.replace_topology("  - {name: thin-snapshot, type: lvm-thin, volume_group: " + fixture.state["vg"] + ", pool_size_gib: 6, root_provisioning: snapshot, zero_new_blocks: true}\n", "")
        for number in (5, 6):
            harness.replace_topology(f"  - {{name: vm{number}-lan, endpoints: [{{node: vm{number}, interface: eth0}}, {{node: lan, interface: vm{number}}}]}}\n", "")
        harness.run("up", timeout=600)
        fixture.register_inventory()
        after = inventory(fixture)
        require(set(before) - set(after) == removed_lvs and all(before[name] == after[name] for name in after), "thin storage removal changed unrelated LVs")

        # An explicitly registered foreign LV must make TopoLab refuse pool cleanup.
        foreign = f"foreign-{os.getpid()}"
        owner_tags = harness.manifest()["nodes"]["vm3"]["root_disk"]["ownership_tags"]
        run(("lvcreate", "--yes", "--virtualsize", str(64 << 20), "--thinpool", f"{fixture.state['vg']}/{harness.manifest()['storages']['thin-copy']['pool_name']}", "--name", foreign, "--addtag", owner_tags[0], "--addtag", owner_tags[1]))
        fixture.register_inventory(foreign=True, names=(foreign,))
        before = inventory(fixture)
        fixture.verify_registered_inventory()
        result = harness.run("down", check=False, timeout=300)
        require(result.returncode != 0 and inventory(fixture) == before, "foreign LV did not produce unchanged cleanup refusal")
        run(("lvremove", "--yes", f"{fixture.state['vg']}/{foreign}"))
        fixture.state["foreign_lvs"].pop(foreign, None)
        fixture.save()
        fixture.verify_registered_inventory()
        harness.run("down", timeout=600)
        fixture.register_inventory()
        require(not inventory(fixture), "final down left TopoLab LVs or pools")
        fixture.verify_vg()
        remaining = []
        for entry in os.scandir("/proc"):
            if not entry.name.isdigit():
                continue
            command_line = Path(entry.path) / "cmdline"
            with contextlib.suppress(OSError):
                if "topolab:storage:" in command_line.read_bytes().replace(b"\0", b" ").decode(errors="replace"):
                    remaining.append(command_line.parent.name)
        require(not remaining, f"final down left storage-suite QEMU processes: {remaining}")
    except BaseException as error:
        failed = True
        # LVM applies ownership tags in the same targeted lvcreate operation.
        # Persist any newly completed TopoLab resources before guarded cleanup;
        # untagged or otherwise unknown LVs still make cleanup stop safely.
        with contextlib.suppress(Exception):
            fixture.register_inventory()
        fixture.diagnostics(harness, error)
        raise
    finally:
        signal.alarm(0)
        signal.signal(signal.SIGALRM, old_alarm)
        signal.signal(signal.SIGTERM, old_term)
        signal.signal(signal.SIGHUP, old_hup)
        if harness:
            harness._finished = True  # fixture controls evidence and cleanup ordering
        try:
            fixture.cleanup(harness, preserve_root=failed)
        except BaseException as cleanup_error:
            fixture.diagnostics(harness, cleanup_error)
            raise
    print("storage E2E passed")


def cli() -> NoReturn | None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--cleanup", type=Path, help="recover exactly one recorded fixture")
    args = parser.parse_args()
    if args.cleanup:
        fixture = Fixture.load(args.cleanup)
        try:
            fixture.cleanup()
        except BaseException as error:
            fixture.diagnostics(None, error)
            raise SystemExit(1) from error
        print("fixture cleanup complete")
        return None
    try:
        main()
    except BaseException as error:
        if isinstance(error, KeyboardInterrupt):
            print("interrupted", file=sys.stderr)
        raise SystemExit(1) from error
    return None


if __name__ == "__main__":
    cli()
