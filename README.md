# TopoLab

[![CI](https://github.com/torob/topolab/actions/workflows/ci.yml/badge.svg)](https://github.com/torob/topolab/actions/workflows/ci.yml)

TopoLab is a Linux-only declarative infrastructure-lab runner for simulating
corporate environments with QEMU/KVM virtual machines, namespace-backed
routers, switched networks, and Linux network namespaces. It runs on the KVM
host itself or, from a Linux controller, on a remote host over SSH. Networking is the
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

TopoLab supports Ubuntu 24.04, Ubuntu 26.04, and Proxmox VE 9 hosts with x86-64
hardware virtualization. Runtime
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
management-host namespace and holds a shared topology lock. It preserves
stdio/TTY behavior, forwards signals to the child process group, and returns
the exact child status. The child runs as the original sudo caller by default,
with supplementary groups and `HOME`/`USER`/`LOGNAME` reset; `--user` selects
another local passwd entry. Root is retained only by the small supervisor that
holds the lock and reaps the child. While `exec` runs, `up`, `start`, `stop`,
`restart` and `down` wait for the lock and write `waiting for the topology lock`
to stderr.

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

## Remote operation

`topolab --remote user@host COMMAND` runs a command on another KVM host. The
controller is an ordinary Linux account: it needs no root, KVM, or QEMU, only an
OpenSSH client. On the host, an agent (the same `topolab` executable) does the
work, so namespaces, TAP devices, QEMU, and state stay on one machine. There is
no daemon and no listener; every command starts one agent over SSH.

Select the host with `--remote DESTINATION`, the `TOPOLAB_REMOTE` variable, or
a `remote` section in the topology file. `--local` overrides all three.

```yaml
remote:
  ssh: operator@lab01.example   # or ssh://user@host:port, or a Host alias
```

```sh
# Once per build: install this executable on the host as the agent.
topolab --remote operator@lab01.example agent install

WORKING_DIR=/var/lib/lab topolab --remote operator@lab01.example -f topology.yml up
WORKING_DIR=/var/lib/lab topolab --remote operator@lab01.example -f topology.yml ps
WORKING_DIR=/var/lib/lab topolab --remote operator@lab01.example -f topology.yml \
  router exec edge -- ip route show
```

The SSH account must be root, or it must be allowed to run the agent as root
without a password. TopoLab never edits `sudoers`. A `privilege_denied` error
prints the rule for your build. It gives that account root-equivalent control:

```text
operator ALL=(root) NOPASSWD: /usr/local/lib/topolab/<sha256>/topolab agent --stdio
```

Proxmox VE does not install `sudo`; use a root account or install `sudo` and
the rule. TopoLab runs beside Proxmox, not inside it: it does not use `qm`, the
Proxmox API, or Proxmox storage, and its VMs do not appear in the Proxmox
interface. OVMF firmware is found under `/usr/share/OVMF/` or
`/usr/share/pve-edk2-firmware/`.

What runs where:

- The topology file, template files with `remote: false` (the default), Root CA
  files, and interpolation variables stay on the controller. Only the variables
  that the topology looks up are sent. `--working-dir` and `WORKING_DIR` name an
  absolute path on the host.
- Templates are uploaded to `<working_dir>/.topolab/blobs/` by SHA-256 and
  reused. An interrupted upload resumes. A later `up` sends no template bytes.
  `templates ls` lists them and `templates prune [--min-age D]` removes the
  unreferenced ones.
- A template that is already on the host needs no upload. Set `remote: true`
  and give the absolute path on the host. The default is `remote: false`, which
  means that `path` is on the controller:

  ```yaml
  templates:
    - {name: ubuntu, path: /var/lib/images/ubuntu.qcow2, remote: true}
  ```

  The controller does not read or hash the file. The agent checks it on the host
  before `up` creates a job (`host_template_invalid` on failure) and uses it in
  place, as local mode does, so do not change a file that existing root disks
  depend on. Changing a template between `remote: false` and `remote: true`
  replaces the root disks of the VMs that use it.
- `up`, `start`, `stop`, `restart`, and `down` run as jobs on the host. A lost
  SSH connection never stops a job or the topology. The controller reconnects
  with backoff for `--reconnect-timeout` (default 2 minutes, `0` disables) and
  then exits with status 3. Use `jobs ls`, `jobs show ID`, `jobs attach ID`,
  `jobs cancel ID`, and `jobs prune` to follow them. The first Ctrl-C cancels a
  job; a second one, or SIGTERM, detaches without cancelling.
- `exec` and `router exec` use a pseudo-terminal when the controller has a
  terminal, and return the child's exact exit status. `capture` writes its
  pcapng file on the controller.
- `config` and `version` never contact the host.

Pass SSH options with `--ssh-opt OPTION` (for example
`--ssh-opt ProxyJump=bastion`). TopoLab runs `ssh` with host-key checking as
your configuration defines it and never weakens it. The protocol and the
guarantees are in [`spec.md`](spec.md) Section 12.

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
that the guest saw as flushed and can corrupt the disk image. TopoLab never
selects `unsafe` on its own; set it on each disk that needs it.

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

LVM zeroing is opt-in. `zero_new_blocks: true` is valid only for `lvm-thin`
and makes LVM clear newly allocated pool chunks; the setting is fixed when the
pool is created. `wipe_on_create: true` is valid only for `lvm-thick` and makes
TopoLab overwrite the full LV before publishing each new disk. Both default to
`false`. Disabling them can expose stale VG data through unwritten portions of
allocated storage. Manifest version 3 is not backward compatible; tear down an
active topology with the previous TopoLab version before upgrading.

The former top-level `host` key is intentionally unsupported. Rename it to
`management_host`; if the topology is active under the legacy root datapath,
run `down` with the new binary before the next `up`. The optional `remote`
section is unrelated to it.

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
sudo python3 e2e/remote/test.py
```

The remote suite drives the router-only network example through SSH to this
machine. Set `TOPOLAB_E2E_REMOTE` to the destination (default `localhost`) and
`TOPOLAB_E2E_SSH_OPTS` to comma-separated `ssh -o` options. The account must be
root or have the `NOPASSWD` rule shown above. The suite installs the agent for
the build under `/usr/local/lib/topolab/`. An opt-in Go test runs the controller
through a real `sshd`: `TOPOLAB_TEST_SSHD=1 go test -run TestRealSSHTransport ./cmd/topolab`.

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
- [`remote`](examples/remote/topology.yml) shows the `remote` section: one VM
  that runs on another host, with its template uploaded from the controller
  (or read in place with `remote: true`).

CI syntax-checks the Python and shell assets and normalizes all five examples
with placeholders. It intentionally does not run privileged root/KVM E2E.

## License

TopoLab is licensed under the [MIT License](LICENSE).
