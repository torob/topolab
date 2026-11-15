# TopoLab

[![CI](https://github.com/torob/topolab/actions/workflows/ci.yml/badge.svg)](https://github.com/torob/topolab/actions/workflows/ci.yml)

TopoLab is a Linux-only declarative infrastructure-lab runner for simulating
corporate environments with local QEMU/KVM virtual machines, namespace-backed
routers, switched networks, and Linux network namespaces. Networking is the
fabric of a lab, not its only purpose: the resulting isolated, reproducible
infrastructure can be used to test Ansible playbooks, Kubernetes clusters and
operators, deployment automation, distributed services, and failure or
connectivity scenarios. The public contract is [`spec.md`](spec.md).

## Install

Go 1.26.6 or newer is required to install TopoLab from source:

```sh
go install github.com/torob/topolab/cmd/topolab@latest
```

To build from a checkout:

```sh
make build
```

This produces a stripped, statically linked binary at `bin/topolab`.

## Host requirements

TopoLab supports Linux hosts with x86-64 hardware virtualization. Runtime
commands require effective UID 0. VM topologies additionally require read/write
access to `/dev/kvm`; router-only topologies skip VM-only host checks. The
read-only `config` and `version` commands do not require root.

The host must provide `ip`, `bridge`, `tc`, and `nft`. Topologies containing
QEMU nodes additionally require `qemu-system-x86_64`, `qemu-img`, `xorriso`,
and OVMF firmware at
`/usr/share/OVMF/OVMF_CODE_4M.fd` and
`/usr/share/OVMF/OVMF_VARS_4M.fd`. `dnsmasq` is required only when a router
serves DHCP. TopoLab does not install or configure these host
dependencies.

An LVM topology additionally requires the LVM2 `lvm` command, a pre-existing
VG with sufficient free extents, and device-mapper thin-pool support when any
storage is `lvm-thin`. File-only topologies do not resolve or invoke LVM.

VM helper locations may be selected with the global `--qemu-system`,
`--qemu-img`, `--lvm`, `--ovmf-code`, `--ovmf-vars`, and `--xorriso` flags. Executable
overrides must be executable regular files and firmware overrides must be
readable regular files.

## Usage

Every active topology has an isolated fabric namespace and an always-present
management-host namespace. Switches, link devices, TAPs, captures, counters,
and QEMU VMs run in the fabric namespace; routers run in their own network
namespaces.

```sh
WORKING_DIR=/var/tmp/topolab topolab -f topology.yml config
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml up
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml ps
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml ps --format json
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml \
  exec -- ssh user@management-address
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml \
  router exec edge -- ip route show
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml snapshot create base
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml snapshot restore base
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml down
topolab version
```

Global flags precede the command. State is kept under
`WORKING_DIR/.topolab`; captures are retained under `WORKING_DIR/captures`.
Templates are always treated as read-only inputs.

Without an `external` router interface, TopoLab does not create or change any
root-namespace interface, route, nftables table, or iptables rule. An
`external` interface is the narrow exception: its root uplink, routes,
forwarding ownership, NAT, and compatibility rules may interact with the host
firewall. Controller connectivity belongs under `management_host.interfaces`
and is accessed with `topolab exec`.

`exec [--user USER] [--cwd DIR] -- COMMAND...` executes argv directly in the
management-host namespace and holds the shared network lock. It preserves
stdio/TTY behavior, forwards signals to the child process group, and returns
the exact child status. The child runs as the original sudo caller by default,
with supplementary groups and `HOME`/`USER`/`LOGNAME` reset; `--user` selects
another local passwd entry. Root is retained only by the small supervisor that
holds the lock and reaps the child. While `exec` runs, `up`, `stop`, `restart`
and `down` wait for the lock and write `waiting for the topology lock` to
stderr. `start`, `stop --hard`, and the `snapshot` commands change only VMs,
so they do not wait for `exec` or `router exec`.

TopoLab keeps two locks for each topology under `WORKING_DIR/.topolab/locks/`.
The network lock (`<topology>.lock`) protects the namespaces, routers,
switches, and the management host. The VM lock (`<topology>.vms.lock`)
protects the QEMU processes and their state. A command takes the network lock
first and the VM lock second:

| Command | Network lock | VM lock |
| --- | --- | --- |
| `exec`, `router exec` | shared | - |
| `start`, `stop --hard` | shared | exclusive |
| `snapshot create`, `snapshot restore`, `snapshot delete` | shared | exclusive |
| `stop`, `restart`, `up`, `down` | exclusive | exclusive |

So only one command changes VMs at a time, and `start`, `stop --hard`, and
the `snapshot` commands run while `exec` commands run. `snapshot list` takes
no lock. The normal `stop` keeps its exclusive network lock.

`router exec NAME [--user USER] [--cwd DIR] -- COMMAND...` provides the same
direct-exec, locking, stdio, TTY, signal, and exit-status behavior in a
configured router's ownership-validated namespace. Router commands run as root
by default so tools such as `ip`, `nft`, and `tc` can administer that
namespace; `--user` explicitly selects another local account. Management-host
`exec` keeps its original-caller default described above.

`ps --format json` emits the stable schema version 2 representation of topology
and network state, nodes, namespaces, management-host interfaces, QEMU PIDs and
state, datapaths, balloon observations, guest-agent availability, and router
external interfaces. It also reports storage/VG/pool identity, provisioning,
thin utilization and overcommit, drift, and warnings. Drift errors are separate
fields rather than state text.

`up`, `start` and `restart` launch VMs one at a time, in YAML order, and do not
wait for the guest to boot. To start a VM later, set `start_delay` on it (a
positive integer and `s`, `m` or `h`). TopoLab waits that long right before it
launches the VM and writes `waiting 30s before starting VM igw` to stderr. The
first VM that a command launches never waits. A VM that already runs does not
wait and does not count as that first launch.

```yaml
vms:
  - name: igw
    start_delay: 30s
```

Some guests wait for a DNS server at boot. Ubuntu 26.04 waits for the network
until at least one interface or VLAN that the boot waits for has a DNS server,
and its SSH server starts only after that wait. Without `dns_servers` (or DHCP)
on such an interface, the wait ends at its 2-minute timeout. `up`, `start` and
`restart` print a warning for each VM in this state. To remove the wait, add
`dns_servers` to an interface, or set `optional: true` on each interface
without one. The boot does not wait for an optional interface. An interface
without `ipv4` and `vlans` is always optional, and a VLAN is never optional.

```yaml
vms:
  - name: probe
    interfaces:
      - name: eth0
        optional: true
        ipv4: {addresses: [10.0.0.10/24]}
```

## Stop and power-loss tests

`stop [NODE...]` and `stop --hard [NODE...]` stop the selected VMs, or all VMs
when no node is given. Both keep the routers, switches, and namespaces
running, and `start` boots the VMs again with the same disks. `ps` then shows
the VMs as `stopped`. Run `topolab stop --help` for a summary.

| | `stop` | `stop --hard` |
| --- | --- | --- |
| Guest | Gets an ACPI power-button event and shuts down | Loses power: no ACPI event, no guest shutdown |
| Order | One VM after another | All VMs at the same moment |
| QEMU | Up to 30 s for the guest, then SIGTERM, then SIGKILL | One SIGTERM, never SIGKILL |
| Running `exec` | Makes the stop wait | Does not make the stop wait |

`stop --hard` simulates a power loss of servers while the network stays up.
It first checks every selected QEMU process, with the same ownership checks
as `stop`: the PID must run in the topology's fabric namespace and must
identify this topology and node. If a check fails, no VM stops. Then one loop
sends SIGTERM through a pidfd to every selected QEMU process, so the time
between the first and the last signal is a few microseconds and a reused PID
never gets the signal. QEMU handles SIGTERM like the QMP command `quit`: it
stops the vCPUs, writes its disk data to the host, closes the disks, and
exits.

If a QEMU process does not exit within 30 seconds, TopoLab leaves it running,
keeps its PID in the state, prints the VM and the PID, and exits with status
1. The command prints a header and one tab-separated line for each selected
VM. Times are UTC with milliseconds; `STOP_TIME` is when TopoLab sent SIGTERM
and `EXIT_TIME` is when QEMU exited:

```text
NAME	RESULT	PID	STOP_TIME	EXIT_TIME
master-1	stopped	4242	2026-11-14T10:00:00.123Z	2026-11-14T10:00:00.161Z
master-2	not-exited	4243	2026-11-14T10:00:00.123Z	-
worker-1	not-running	-	-	-
```

Data note: like a real power loss, a hard stop loses the data that a guest did
not flush. With `cache: unsafe`, a hard stop keeps the disk images consistent,
because QEMU writes its disk data to the host before it exits. A host crash or
a host power loss can still lose data and break an image, because that data
can still be only in the host page cache.

## Snapshots

A snapshot saves the disks and the OVMF variables of VMs under a name, so
that you can bring the topology back to that point later. Run
`topolab snapshot --help` for a summary.

```text
topolab snapshot create  NAME [NODE...]
topolab snapshot restore NAME [NODE...]
topolab snapshot delete  NAME [NODE...]
topolab snapshot list    [--format table|json]
```

Without `NODE`, `create` saves all VMs of the topology, and `restore` and
`delete` use all VMs of the snapshot. `NAME` uses lowercase letters, digits,
`.`, `_`, and `-`, with at most 63 characters.

- A snapshot does not save RAM. A restore boots the restored VMs again.
- `file` disks get a qcow2 internal snapshot with the snapshot name as its
  tag. `lvm-thin` disks get a read-only thin snapshot LV (`tlx-...`) in their
  pool. A VM with an `lvm-thick` disk cannot be in a snapshot.
- Snapshot LVs use space in the thin pool when the disks change. The pool
  utilization warnings of `ps` include this space.

`create` works on running and stopped VMs, also in the same snapshot. A
stopped VM is saved directly. For the running VMs, TopoLab:

1. freezes the guest file systems of each VM that has `guest_agent: true` and
   a responsive `qemu-ga`;
2. pauses all running VMs at the same moment with QMP `stop`, so all of them
   are saved at the same point in time;
3. saves the disks and copies the OVMF variables;
4. lets the VMs continue and thaws their file systems.

The pause is short: qcow2 disks add almost no time, and each `lvm-thin` disk
adds some hundred milliseconds. The output shows the consistency of each VM:

```text
NAME	STATE	CONSISTENCY
web1	running	frozen
web2	running	crash-consistent
db1	stopped	stopped
```

`frozen` means clean file systems. `crash-consistent` is the same state as
after a power loss: the VM has no guest agent, the agent did not answer, or
the freeze failed (TopoLab then writes a warning). A journaling file system
recovers at the next boot.

`restore` keeps the run state that each VM has before the restore. TopoLab
stops each running VM with the `stop --hard` method, restores its disks and
OVMF variables, and starts it again. A VM that was stopped before the restore
stays stopped. Other VMs keep running. If a disk grew after the snapshot, it
keeps its new size. If a restore is interrupted, run the same command again
to complete it; until then, `up`, `start`, and `restart` refuse to run.

```text
NAME	BEFORE	RESULT	AFTER
web1	running	restored	running
db1	stopped	restored	stopped
```

If you restore only some VMs of a cluster, for example of etcd or a
replicated database, their state is older than the state of the other
members. TopoLab cannot detect this.

`up` refuses a change that destroys snapshot data: a template change, a VM
removal, or a disk removal of a VM in a snapshot. Delete the snapshot, or only
that VM from it (`topolab snapshot delete NAME NODE`), first. New VMs, new
disks, and disk growth are allowed. `down` deletes all snapshots.

## Explicit storage backends

Every root and additional disk names a top-level storage; there is no implicit,
VM-level, or topology-default backend. File roots can be standalone copies or
qcow2 snapshots. LVM disks are raw block devices in a supplied VG; TopoLab never
creates or removes that VG or its PVs.

```yaml
storages:
  - {name: snapshots, type: file, root_provisioning: snapshot}
  - {name: copies, type: file, root_provisioning: copy}
  - name: thin
    type: lvm-thin
    volume_group: lab-vg
    pool_size_gib: 20
    root_provisioning: snapshot
    zero_new_blocks: true
  - {name: thick, type: lvm-thick, volume_group: lab-vg, wipe_on_create: true}

vms:
  - name: node
    template: ubuntu
    root_disk: {storage: thin, size_gib: 8}
    additional_disks:
      - {name: data, storage: thick, size_gib: 4, cache: writeback, aio: threads}
```

`root_provisioning` is required and is `copy` or `snapshot` for `file` and
`lvm-thin`. `lvm-thick` forbids it. `volume_group` is required only for LVM;
`pool_size_gib` is required only for `lvm-thin`. Forbidden fields are rejected
even when explicitly empty or zero. Storage/VG/provisioning changes, disk
shrinks, and thin-pool reductions are rejected before mutation. Pools and disks
may grow; thin utilization and overcommit are warnings rather than blockers.

Every root and additional disk defaults to `cache: none`, which makes both the
QEMU format and protocol block nodes use direct I/O and bypass the host page
cache while keeping guest flushes enabled. Set `cache: writeback` on an
individual disk to retain buffered host I/O for that disk. Direct I/O avoids
double caching and host page-cache pressure, but workloads may perform
differently and the backing filesystem or block device must support it; QEMU
startup fails instead of silently falling back when it does not.

Set `cache: unsafe` on a disk of a disposable lab when guest `fsync` latency
matters more than durability, for example etcd on lab masters. It uses
buffered host I/O like `writeback` and also makes QEMU ignore guest flush
requests, like QEMU `cache=unsafe`: a guest flush completes at once and the
data stays in the host page cache. A host crash or power loss can lose writes
that the guest saw as flushed and can corrupt the disk image. A `stop --hard`
does not (see [Stop and power-loss tests](#stop-and-power-loss-tests)).
TopoLab never selects `unsafe` on its own; set it on each disk that needs it.

Changing a disk's cache policy restarts only its VM and preserves the disk.
On upgrade, existing configurations without `cache` intentionally switch to
direct I/O on their next `up`; add `cache: writeback` to disks that should
keep the previous buffered behavior.

Every disk also defaults to `aio: auto`. TopoLab probes the selected QEMU and
running kernel before reconciliation, uses `io_uring` when available, and
otherwise warns once and falls back to `threads`. Set `aio: io_uring`,
`aio: threads`, or `aio: native` for a strict per-disk choice; an explicitly
requested unavailable `io_uring` backend fails preflight. Native AIO requires
`cache: none`, so it is invalid with `writeback` and `unsafe`. For qcow2 snapshot roots, the selected cache and AIO policies
apply to both the writable overlay and its read-only template backing chain.
Changing `aio` restarts only the affected VM and preserves its disks.
Existing configurations without `aio` adopt `auto` on their next `up`.

Guest discard requests (for example `fstrim`) reach every writable disk. A file
disk frees the discarded qcow2 clusters, and a thin LV returns the chunks to its
pool. New file snapshot overlays use 4 KiB qcow2 subclusters
(`extended_l2=on,cluster_size=128k`), so small guest writes do not copy a full
cluster from the template first.

LVM zeroing is opt-in. `zero_new_blocks: true` is valid only for `lvm-thin`
and makes LVM clear newly allocated pool chunks; the setting is fixed when the
pool is created. `wipe_on_create: true` is valid only for `lvm-thick` and makes
TopoLab overwrite the full LV before publishing each new disk. Both default to
`false`. Disabling them can expose stale VG data through unwritten portions of
allocated storage. Because guest discards return thin chunks to the pool while
VMs run, another thin LV can get those chunks; set `zero_new_blocks: true`
when VMs must not see data that other VMs wrote. Manifest version 4 adds
snapshots. TopoLab reads a version-3 manifest and writes it as version 4, so
an active topology from the previous version keeps working. An older TopoLab
refuses a version-4 manifest; tear down with the new version before a
downgrade. Manifest versions 1 and 2 are not supported; tear down such a
topology with the TopoLab version that created it.

The former top-level `host` key is intentionally unsupported. Rename it to
`management_host`; if the topology is active under the legacy root datapath,
run `down` with the new binary before the next `up`.

## Development

Run the local checks with:

```sh
make test
make test-race
make vet
python3 -m py_compile e2e/common.py e2e/*/test.py
sh -n e2e/routed/gateway-router.sh
```

The race target requires a C compiler and also produces a race-instrumented
binary at `bin/topolab-race` for interactive diagnostics.

Full E2E execution is deliberately direct and opt-in because it requires root,
KVM, QEMU/OVMF, host networking tools, OpenSSH, and (for VM suites) Internet
access. Build TopoLab, then run one suite:

```sh
sudo python3 e2e/network/test.py
sudo python3 e2e/routed/test.py
sudo python3 e2e/services/test.py
sudo python3 e2e/vm-features/test.py
sudo python3 e2e/storage/test.py
sudo python3 e2e/power-loss/test.py
sudo python3 e2e/snapshot/test.py
```

The power-loss suite runs 3 VMs with `cache: unsafe` disks. After
`stop --hard`, it checks each qcow2 image with `qemu-img check`. It also checks
each ext4 and vfat filesystem on each guest disk in a copy: the copy gets the
same recovery as the next boot (ext4 journal replay, FAT dirty bit), then
`e2fsck -fn` or `fsck.fat -n` must find no error. After `start`, the guest
kernels must report no filesystem error. Set `TOPOLAB_E2E_POWER_LOSS_MEMORY_MB`
to change the memory of each VM (default 1024).

The snapshot suite runs 3 VMs with `file` and `lvm-thin` disks. It installs
`qemu-guest-agent` in one guest through an external router, so it needs
Internet access from the guest. It checks that `snapshot create` freezes and
pauses the running VMs without a reboot, that `up` refuses to remove a VM in
the snapshot, that `snapshot restore` brings back the guest files and the
data-disk contents and keeps the run state and the grown disk sizes, and that
`down` removes every snapshot LV. Set `TOPOLAB_E2E_VG` to an existing VG with
at least 16 GiB free. Without it, the suite creates a sparse 16-GiB backing
file under `/var/tmp`, a loop PV, and a `tlsnap-*` VG, and removes them at the
end.

The VM suites automatically download the official Ubuntu 26.04 amd64 cloud
image from pinned release `20260720`, verify its SHA-256, and cache it under
`${XDG_CACHE_HOME:-$HOME/.cache}/topolab-e2e/`, never in the checkout. Set
`TOPOLAB_TEMPLATE` to use another compatible image, `TOPOLAB_BINARY` to select
the executable, or `TOPOLAB_E2E_KEEP=1` to retain a successful temporary run.
Every failure retains its temporary directory and prints the location with
diagnostics. The suites generate and export their own ephemeral SSH key, and
the VM-features suite also generates its own ephemeral Root CA.

The storage suite additionally requires LVM2, device mapper's `thin-pool`
target, loop-device support, writable `/dev/mapper/control`, and at least 24 GiB
available in its temporary filesystem. It creates exactly one sparse 24-GiB
backing file, one verified loop PV, and a randomly named `tltest-*` VG. Its
controlled 45-minute timeout preserves evidence on safety-check failure. It
never scans for cleanup candidates. After an uncatchable termination, use the
exact command printed by the suite, for example
`sudo python3 e2e/storage/test.py --cleanup /tmp/tl-storage-.../fixture-state.json`.
The recovery path revalidates backing-file, loop major/minor, PV UUID, VG UUID,
and every registered LV before any destructive command. Run the suite twice for
storage acceptance and confirm both runs leave no fixture resources.

## Examples

Each example directory contains only a declarative `topology.yml` and its
hand-maintained `topology.svg` diagram. Testing and guest provisioning live in
the matching `e2e/` directory.

- [`network`](examples/network/topology.yml) is router-only and demonstrates an
  isolated management host, VLAN access/trunk ports, link impairment, capture,
  and directional counters.
- [`routed`](examples/routed/topology.yml) is the synthetic three-VM gateway and
  external-router lab. The gateway is an ordinary VM; its idempotent guest
  routing policy belongs to the routed E2E harness.
- [`services`](examples/services/topology.yml) demonstrates tagged VM
  interfaces, router VLANs, DHCP/DNS, router-local NAT, and external access.
- [`vm-features`](examples/vm-features/topology.yml) demonstrates ballooning,
  the guest-agent channel, serial logs, multiqueue and non-VirtIO NICs,
  additional disks, inline and file-backed Root CA trust, boot policy, and
  selected VM lifecycle operations.

CI syntax-checks the Python and shell assets and normalizes all four examples
with placeholders. It intentionally does not run privileged root/KVM E2E.

## License

TopoLab is licensed under the [MIT License](LICENSE).
