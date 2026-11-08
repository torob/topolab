# TopoLab Specification

## 1. Status and scope

This document specifies `topolab`, a Linux-only tool for
creating declarative network labs on one KVM host. The tool runs on that host
(local mode), or it runs on a separate Linux controller and operates the host
through an agent reached over SSH (remote mode, Section 12). The implementation
is written in Go and manages QEMU directly. It does not use libvirt.

The tool manages:

- QEMU/KVM virtual machines;
- network-namespace-based routers;
- isolated management-host attachments;
- switched and direct point-to-point links;
- Linux bridge access and trunk ports;
- IPv4 addressing, static routing, DHCPv4, and NAT;
- directional link latency, jitter, loss, and rate limits;
- host-side link packet capture and directional counters;
- read-only qcow2 templates with explicit file or LVM writable disks;
- persisted, multiplexed QEMU serial logs; and
- remote operation of one KVM host from a Linux controller over SSH, with
  resumable content-addressed template transfer and lifecycle jobs that survive
  loss of the controller connection.

The following features are outside the scope of this specification:

- IPv6;
- dynamic routing protocols;
- physical NIC bridging;
- QEMU live migration and live hardware reconfiguration other than online disk
  growth and VirtIO balloon-target changes;
- libvirt integration;
- integration with the Proxmox VE API, cluster file system, or storage,
  firewall, and network configuration;
- a graphical interface;
- a persistent agent, a network listener, or any remote transport other than
  OpenSSH;
- controllers that are not Linux systems;
- one topology that spans several hosts; and
- automatic restoration through systemd after host reboot.

## 2. Host contract

The supported hosts are Ubuntu 24.04, Ubuntu 26.04, and Proxmox VE 9, each on
x86-64 with hardware virtualization and an accessible `/dev/kvm`. In this
document the host is the machine that runs the VMs, routers, and namespaces. In
local mode it also runs the `topolab` process. In remote mode the `topolab`
process runs on a controller and an agent runs on the host.

The following commands are required for networking operation:

- `ip`, `bridge`, and `tc` from iproute2;
- `nft`.

Topologies containing VMs additionally require `qemu-system-x86_64`,
`qemu-img`, `xorriso`, accessible KVM, and OVMF. Router-only topologies skip
these VM-only requirements.

`dnsmasq` is additionally required when a router declares a DHCP
server. QEMU VMs require an OVMF code image and a matching OVMF variables
image. Unless `--ovmf-code` and `--ovmf-vars` select them, the tool uses the
first directory in the following list in which `OVMF_CODE_4M.fd` and
`OVMF_VARS_4M.fd` are both readable regular files:

1. `/usr/share/OVMF/` (Ubuntu);
2. `/usr/share/pve-edk2-firmware/` (Proxmox VE).

The tool never combines a code image and a variables image from different
directories.

The Linux vhost-net accelerator is optional. When `/dev/vhost-net` is
available and accessible, the tool uses it automatically for VirtIO VM
interfaces. Its absence must not prevent a VM from running.

On Proxmox VE the tool runs beside Proxmox's own virtualization management, not
inside it. It starts and supervises its own QEMU processes. It does not use
`qm`, `pvesm`, the Proxmox API, or `/etc/pve`. It never changes Proxmox
storage, firewall, or network configuration, and its VMs do not appear in
Proxmox's interface. An LVM VG supplied to the tool should be dedicated to it
and should not be managed by Proxmox storage configuration. When a router
declares an `external` interface, the host firewall policy, including
`pve-firewall`, remains the operator's responsibility. Proxmox VE does not
install `sudo` by default; Section 12.3 describes the consequence.

In remote mode the controller must be a Linux system with an OpenSSH client
named `ssh` in `PATH`. It needs no root privilege, KVM, QEMU, or network tools.
The host satisfies every requirement of this section.

`config` and `version` are read-only and may run as an ordinary user. All other
commands require the privileges described by their command contracts. In remote
mode the controller process runs as an ordinary user for every command, and
the agent requires effective UID 0 on the host (Section 12.3). The CLI
must reject runtime operations rather than silently falling back to
unaccelerated QEMU or partially configured networking.

## 3. Command-line interface

The invocation form is:

```text
topolab [GLOBAL OPTIONS] COMMAND [OPTIONS]
```

`FILE` defaults to `topology.yml`. Global options must precede the command.
The working directory is required for every command except `version` and is
selected from `--working-dir` when present, otherwise from the `WORKING_DIR`
process environment variable. The CLI must fail if neither source supplies a
non-empty value. A relative working directory is resolved against the process's
current directory. The command-line option takes precedence over the
environment.

Global VM-helper options are `--qemu-system PATH`, `--qemu-img PATH`, `--lvm PATH`,
`--ovmf-code PATH`, `--ovmf-vars PATH`, and `--xorriso PATH`. Executable
overrides must resolve to executable regular files; firmware overrides must be
readable regular files. The selected helpers are resolved once and used by
preflight and all subsequent VM operations. Defaults retain PATH lookup for
executables and the firmware lookup defined in Section 2. In remote mode these
options name paths on the host.

Global remote options are `--remote DESTINATION`, `--local`,
`--ssh-opt OPTION` (repeatable), and `--reconnect-timeout D`. The environment
variable `TOPOLAB_REMOTE` supplies a destination. Section 12.1 defines their
syntax and precedence.

### 3.1 Commands

`config`
: Parse, default, validate, and print the normalized YAML. It does not inspect
  or modify runtime state.

`up`
: Validate prerequisites and templates, compare desired configuration with
  persisted state, remove obsolete resources, replace a writable root disk only when
  its backing-template identity changes, repair missing networking, and ensure
  every QEMU VM is running.

`ps [--format table|json]`
: Print QEMU VMs, routers, and switches with their type, runtime
  state, and QEMU PID where applicable. Router output identifies external
  interfaces and their host egress. For VMs it also prints the configured and
  observed balloon state and guest-agent availability when those devices are
  enabled, plus the effective network datapath for each interface. It also
  reports both topology namespaces and every management-host interface, its
  addresses and link, and whether
  its isolation rules are installed.
  Table output remains the default. JSON uses `schema_version: 1` and contains
  typed topology and network status, nodes, namespaces, management-host
  interfaces, QEMU status and PIDs, datapaths, balloon state, guest-agent
  availability, and router external-interface details. Drift errors occupy
  dedicated fields and are never embedded in state strings.

`version`
: Print the TopoLab version without loading a topology, resolving a working
  directory, requiring root, or probing runtime dependencies. Release builds
  print their injected semantic version, versioned module installs use Go build
  information, and local development builds print `devel`.

`logs [--follow|-f] [--tail N|all] [--timestamps] [NODE ...]`
: Read timestamped serial records for selected QEMU VMs. With no node
  arguments, select all QEMU VMs. `--tail` applies per node before records are merged by
  timestamp. `--follow` continues until interrupted.

`capture LINK --endpoint NODE:INTERFACE [--direction ingress|egress|both] [--output PATH] [--snaplen N] [--duration D] [--count N]`
: Capture frames at one endpoint of a running link without entering a guest.
  Direction defaults to `both`. The foreground command stops when its optional
  duration or packet count is reached or when interrupted, and writes pcapng as
  defined in Section 8.

`counters [--format table|json] [LINK ...]`
: Read cumulative directional link and impairment counters. With no link
  arguments, select every link. Table output is the default; JSON provides the
  stable machine-readable representation defined in Section 8.

`exec [--user USER] [--cwd DIR] -- COMMAND [ARG...]`
: Require an active, ownership-validated topology and directly execute the
  supplied argv, without a shell, in its management-host network namespace.
  The command is permitted when that namespace has only loopback connectivity.
  A shared topology lock is held for the command lifetime, so `down` waits.
  The child inherits stdin, stdout, stderr, TTY state, and a sanitized process
  environment. `--cwd` selects its working directory.

  Namespace entry occurs with the privilege required by `setns`, after which
  the child drops privileges. Without `--user`, the identity is the original
  caller: first a valid `SUDO_UID`/`SUDO_GID` pair, then a non-root real UID
  and GID, and finally root for a direct-root invocation. `--user` accepts a
  locally resolvable user name or numeric UID with a passwd-database entry and
  selects that account's primary and supplementary groups. The child receives
  correct `HOME`, `USER`, and `LOGNAME` values and retains no unintended root
  capabilities. A minimal root supervisor may remain outside the child only to
  hold the lock, forward signals to the child process group, reap it, and
  return its exact exit status. Every descendant inherits the management-host
  namespace.

`router exec NAME [--user USER] [--cwd DIR] -- COMMAND [ARG...]`
: Resolve `NAME` through the configured routers and execute argv directly in
  that router's namespace. Missing, unknown, non-router, absent, legacy, or
  incorrectly owned targets are rejected before the child starts. Stdio, TTY,
  locking, signal forwarding, environment sanitization, and exact exit-status
  behavior match management-host `exec`. The default identity is root so
  router administration commands retain the necessary capabilities;
  `--user` selects a local passwd-database account and drops to its primary and
  supplementary groups.

`start [NODE ...]`, `stop [NODE ...]`, `restart [NODE ...]`
: Operate on selected QEMU VMs, or all QEMU VMs when no names are supplied.
  These commands do not stop routers or remove networking. `stop`
  leaves the fabric and management-host namespaces present. `start` reuses and
  verifies both namespaces and is rejected if the managed network is absent or
  fails ownership validation.

`down`
: Stop QEMU VMs and helper processes, validate ownership, remove external
  root resources, router namespaces, the management-host namespace, and
  finally the fabric namespace; then remove writable
  disks, NoCloud seeds, logs, and topology state. Template images are never
  removed. Packet-capture artifacts are also retained. Namespace removal is
  refused while unowned processes remain inside it. Running `down` when state
  is absent is valid and idempotent. It can safely remove a legacy manifest,
  but `up` refuses an active legacy root-datapath manifest and directs the user
  to run `down` first.

`agent install`
: Remote mode only. Install the controller's own executable on the host as the
  agent, as defined in Section 12.3. It requires `--remote` or
  `TOPOLAB_REMOTE`. It reads no topology file and needs no working directory.

`agent --stdio`
: The endpoint that the controller starts through SSH (Section 12.2). It
  requires effective UID 0 and refuses to run when its standard input or
  standard output is a terminal. It is not an interactive command.

`jobs ls [--all]`, `jobs show ID`, `jobs attach ID`, `jobs cancel ID`, `jobs prune`
: Remote mode only. List, inspect, follow, cancel, and prune lifecycle jobs, as
  defined in Section 12.6. In local mode these commands fail with a usage
  error.

`templates ls`, `templates prune [--min-age D]`
: Remote mode only. List and prune uploaded templates in the host blob store,
  as defined in Section 12.5. In local mode these commands fail with a usage
  error.

Except for `version` and `agent install`, every command loads the topology file
as Section 4 defines. In remote mode the commands behave as Section 12 defines.

## 4. Configuration format

The input is one strict YAML document. Unknown fields and additional YAML
documents are errors. Environment interpolation is performed before path
resolution and validation. Relative filesystem paths in the document are then
resolved against the directory containing the topology file.

The top-level structure is:

```yaml
name: example
remote: {ssh: operator@lab01.example}   # optional
templates: []
storages: []
management_host: {interfaces: []}
vms: []
routers: []
switches: []
links: []
```

The optional `remote` section selects remote operation and is defined in
Section 4.8. It is omitted from normalized output when absent. It is
unrelated to the rejected legacy `host` key described below.

Every named collection is an ordered YAML list whose entries carry a `name`;
VLAN entries carry an `id`. The former named-map syntax is not accepted.
Missing collections normalize to `[]`. Null entries, missing identities, and
duplicate names or VLAN IDs are errors. Entry names must match
`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`. Names of VMs, routers, and switches share
one global namespace and must be unique. The name `management_host` is
reserved for the isolated controller endpoint and may not be used by a VM,
router, or switch. The legacy top-level `host` key is rejected with a migration
diagnostic directing the user to `management_host`; this is an intentional
breaking schema and runtime behavior change.
Interface names must be unique within their node.

There must be at least one link or one external router interface.

### 4.1 Working directory and environment interpolation

The selected working directory is CLI state and is not a field in the topology
document. The `config` command requires and resolves it but does not create it.

Every ordinary topology file and directory created by `topolab` must be beneath
the selected working directory. LVs in explicitly supplied VGs and UUID-keyed
coordination locks beneath `/run/lock` are the storage-backend exceptions.
Files include manifests, topology locks, writable file-disk
images, OVMF variable stores, NoCloud seed images, PID files, Unix sockets,
serial logs, packet captures, dnsmasq files, router configuration, and
forwarding-ownership state. Runtime operations create the required directories
with root-only permissions. The tool must reject a working directory whose
resolved path cannot safely contain its generated files or whose generated
Unix socket paths would exceed the host limit.

Interpolation applies to every unquoted or double-quoted YAML string value,
but not to mapping keys or single-quoted values. Ordinary variables come from
the environment of the `topolab` process. The expression grammar and
unset-versus-empty behavior follow Docker Compose interpolation syntax:

- `$VAR` and `${VAR}` substitute `VAR` directly;
- `${VAR:-default}` uses `default` when `VAR` is unset or empty;
- `${VAR-default}` uses `default` only when `VAR` is unset;
- `${VAR:?error}` fails with `error` when `VAR` is unset or empty;
- `${VAR?error}` fails with `error` only when `VAR` is unset;
- `${VAR:+replacement}` uses `replacement` when `VAR` is set and non-empty,
  and otherwise substitutes an empty string;
- `${VAR+replacement}` uses `replacement` when `VAR` is set, and otherwise
  substitutes an empty string; and
- `$$` produces a literal `$` and prevents interpolation at that position.

Variable names must match `[_a-zA-Z][_a-zA-Z0-9]*`. Default, required, and
alternative expressions may be nested. Shell pattern substitutions such as
`${VAR/foo/bar}` are not supported. An unset variable without a default or
required expression produces a warning and is replaced with an empty string.
Interpolation never executes a shell command.

The process environment is the only interpolation-variable source. In remote
mode it is the environment of the controller's `topolab` process; the agent
never consults its own environment (Section 12.4). There are
no special interpolation variables. In particular, `WORKING_DIR` behaves like
any other process environment variable. Supplying `--working-dir` does not
define or override an interpolation variable named `WORKING_DIR`. Automatic
`.env` loading and a Compose-style `--env-file` option are outside this
specification.

For example:

```yaml
templates:
  - name: ubuntu
    path: "${TEMPLATE_DIR:?TEMPLATE_DIR must be set}/ubuntu.qcow2"
```

Environment variables are passed to the CLI in the normal way:

```bash
WORKING_DIR=./work \
TEMPLATE_DIR=./templates \
  topolab -f topology.yml config
```

### 4.2 Templates

```yaml
templates:
  - name: ubuntu
    path: ./templates/ubuntu.qcow2
```

Each `templates` entry contains a unique `name`, a `path`, and an optional
boolean `remote` that defaults to `false`. With `remote: false`, a relative
`path` is resolved against the directory containing the topology file; in remote
mode the file is on the controller. With `remote: true`, `path` must be an
absolute path, and it names a file on the host that runs the VMs. In local mode
that host is this machine, so `remote` changes nothing there except the rule
that the path is absolute. These rules do not depend on the mode. Each file must
be an existing regular file built separately from `topolab`.

Every template must use the qcow2 format and must not itself have a backing
file. The tool validates the format before use and treats the template as a
read-only source. Each VM root is then constructed using its selected
storage's explicit copy or snapshot mode: file snapshots are qcow2 overlays,
while file copies and LVM roots do not retain a direct dependency on the
template file. The guest image must include the driver needed by the VM's
selected root-disk bus in its initramfs and must be able to boot its root
filesystem from that bus. Templates used with the default `scsi` bus must
include the VirtIO SCSI driver.

Templates may be inside or outside `working_dir`. Even when a template is
inside it, `topolab` never builds, modifies, or removes the template, and
`down` removes only paths recorded as tool-created resources. Template
contents are SHA-256 identified.

In remote mode a template path names a file on the controller, not on the host.
The controller validates it, hashes it, and uploads it to the host only when the
host does not already hold content with that digest. The manifest then records
the host blob path as the template path (Section 12.5). The host never reads
the controller path.

In remote mode a template with `remote: true` names a file on the host. The
controller does not read, hash, or upload it, and `config` on the controller
does not look for it. The agent validates it on the host as this section
requires and uses it in place, as local mode does (Section 12.5). The storage
identity of a VM contains the template path (Section 7). A template that
changes between `remote: false` and `remote: true` in remote mode therefore
changes its path and replaces the root disk of every VM that uses it, also when
the content is the same.

### 4.2.1 Storage declarations

Every VM disk explicitly references one entry in the top-level `storages`
list. There is no implicit storage and no VM-level or topology-wide default.

```yaml
storages:
  - {name: file-snapshot, type: file, root_provisioning: snapshot}
  - {name: file-copy, type: file, root_provisioning: copy}
  - name: thin-copy
    type: lvm-thin
    volume_group: lab-vg
    pool_size_gib: 20
    root_provisioning: copy
    zero_new_blocks: true
  - name: thin-snapshot
    type: lvm-thin
    volume_group: lab-vg
    pool_size_gib: 20
    root_provisioning: snapshot
  - {name: thick, type: lvm-thick, volume_group: lab-vg, wipe_on_create: true}
```

| Type | Required | Forbidden |
| --- | --- | --- |
| `file` | `name`, `type`, `root_provisioning` | `volume_group`, `pool_size_gib`, `zero_new_blocks`, `wipe_on_create` |
| `lvm-thin` | `name`, `type`, `volume_group`, `pool_size_gib`, `root_provisioning` | `wipe_on_create` |
| `lvm-thick` | `name`, `type`, `volume_group` | `pool_size_gib`, `root_provisioning`, `zero_new_blocks` |

`root_provisioning` is exactly `copy` or `snapshot`. A forbidden field is an
error even if explicitly empty or zero. Multiple declarations may share a
supplied VG, but every thin declaration owns a distinct deterministic pool.
`pool_size_gib` is pool data capacity; metadata consumes additional extents.
`zero_new_blocks` and `wipe_on_create` default to `false`. The former controls
lazy zeroing of newly allocated thin-pool chunks and is immutable after pool
creation. The latter controls a full-device overwrite before a newly created
thick disk is published. Disabling either guarantee can expose stale VG data.

TopoLab resolves `lvm` only for an LVM topology and accepts `--lvm` as an
executable override. Supplied VGs and PVs are never created or removed. It
uses machine-readable VG/LV reports, locks all involved VGs in sorted VG-UUID
order, and revalidates identity, ownership, and capacity before mutation.
Storage type, VG, and root-provisioning changes are immutable while resources
exist. Thin pools and disks may increase, but not decrease.

### 4.3 Isolated management-host attachment

The optional `management_host` object declares tool-owned interfaces in the
topology's always-present management-host network namespace:

```yaml
management_host:
  interfaces:
    - name: management
      mtu: 1500
      ipv4:
        addresses: [172.31.255.1/24]
```

`management_host` accepts only an `interfaces` list and defaults to
`{interfaces: []}`. Each interface requires an `ipv4.addresses` list
containing one or more unique static IPv4 prefixes. Its optional `mtu`
defaults to `1500`. DHCP, routes, VLAN subinterfaces, NAT, and `external` are
not management-host-interface fields. Logical names use the resource-name
rules and receive deterministic, locally administered MAC addresses.

Every declared interface must occur in exactly one link endpoint using the
reserved node name `management_host`:

```yaml
links:
  - name: management
    endpoints:
      - node: management_host
        interface: management
      - node: management-switch
        interface: management-host
        port:
          mode: access
          vlan: 100
```

A management-host endpoint may connect directly to a VM or router, or to a
switch access port. It may not connect to a switch trunk port. Its node end is
one end of a veth pair in the management-host namespace; its peer connects to
the ordinary link datapath in the fabric namespace and behaves like a physical
cable. Endpoint `egress` impairments retain their ordinary meaning.

Declared addresses and their connected routes exist only in the
management-host namespace. Root-route overlap validation does not apply, and
separate topologies may reuse identical management and workload prefixes. The
tool installs forwarding-drop rules in a dedicated nftables table inside that
namespace so it cannot accidentally become a transit router. With no declared
interfaces, the namespace has loopback connectivity only and remains usable by
`topolab exec`.

`ps` reports each logical management-host interface, its actual Linux name,
configured and observed MTU, addresses, link, namespace, and isolation-rule
state. `down` removes its namespace after verifying ownership and ensuring no
unowned process remains inside it.

### 4.4 VMs

```yaml
vms:
  - name: web
    template: ubuntu
    cpu:
      model: host
      sockets: 1
      cores: 2
      threads: 1
    memory_mb: 2048
    memory_balloon:
      enabled: true
      target_mb: 1536
    root_disk:
      size_gib: 40
      bus: scsi
      storage: thin
      cache: none
      aio: auto
    additional_disks:
      - name: data
        size_gib: 100
        bus: scsi
        storage: thick
        cache: writeback
        aio: threads
    guest_agent: true
    boot:
      order: [disk, network]
      menu: false
      strict: false
    ssh_authorized_keys:
      - ssh-ed25519 AAAA... operator@example
    root_ca_certificates:
      - pem: |
          -----BEGIN CERTIFICATE-----
          ...
          -----END CERTIFICATE-----
      - path: certs/lab-root-ca.pem
    interfaces:
      - name: eth0
        model: virtio
        queues: 2
        mac: "02:00:00:00:00:10"
        mtu: 1500
        ipv4:
          addresses: [10.0.10.10/24]
          routes:
            - {to: default, via: 10.0.10.1}
          dns_servers: [10.0.10.53]
```

`template` must reference a declared template. `memory_mb` defaults to `2048`
and must be at least 128. Every VM must declare at least one interface and a
non-empty `ssh_authorized_keys` list. Each entry must be a valid OpenSSH public
key, and duplicate keys are invalid. The keys are installed for the locked-
password `torob` bootstrap user.

`root_ca_certificates` is an optional ordered list of self-signed X.509 Root
CA certificates to add to the VM's system trust store. Each entry must set
exactly one of `pem`, containing inline PEM text, or `path`, naming a PEM file.
A relative path is resolved against the topology file's directory and is
printed as an absolute path by `config`. A path must identify a readable
regular file.

Every source must contain exactly one `CERTIFICATE` PEM block and no other
non-whitespace data. The certificate must have valid CA basic constraints,
identical subject and issuer names, and a valid self-signature. Duplicate DER
certificates within one VM are invalid, including duplicates supplied through
different source forms. Certificate validity dates are not checked while
loading the topology. TopoLab canonicalizes accepted certificates from their
DER encoding and supplies them through cloud-init `ca_certs.trusted` with
`remove_defaults: false`; it never handles the corresponding private keys or
removes the template's default trust roots.

`cpu` optionally controls the CPU model and topology exposed to the guest.
`cpu.model` defaults to `host`, which uses KVM host passthrough. It may instead
name an x86 CPU model reported by `qemu-system-x86_64 -cpu help`; raw QEMU CPU
feature suffixes such as `,vmx=off` are not accepted. Before
changing runtime state, the tool must verify that the selected model exists
and can run with KVM on the current host.

The topology fields `sockets`, `cores`, and `threads` are positive integers.
When none is specified, the topology defaults to one socket, two cores, and
one thread per core. If any topology field is specified, all three are
required. Their product is the VM's total vCPU count. The tool must compute
that product with checked arithmetic, verify that QEMU and KVM support the
result on the current host, and pass the total and every topology field
explicitly to QEMU rather than relying on QEMU to infer omitted values. VM
`vcpus` is not a configuration field. CPU hotplug and NUMA topology are outside
the scope of this specification.

`memory_mb` is the amount of RAM allocated to QEMU and the maximum memory that
can be made available to the guest by balloon deflation. `memory_balloon` is
optional. Its `enabled` field defaults to `false`; when disabled, `target_mb`
must be omitted. When enabled, the tool attaches a `virtio-balloon-pci` device.
`target_mb` defaults to `memory_mb`, must be at least 128, and must not exceed
`memory_mb`.

After starting the VM, and whenever only `target_mb` changes, the tool requests
the target online through the QMP `balloon` command. Ballooning is asynchronous
and the guest may be unable to release all requested memory, so acceptance of
the request is convergence; an exact current-memory reading is not. `ps`
reports the configured target and, when supplied by the guest driver, the
current balloon value. A missing or inactive guest balloon driver is reported
as a warning and pending state, and a later `up` retries the request. The
template must provide a compatible VirtIO balloon driver.

#### 4.4.1 VM interfaces and layer-3 configuration

Each VM interface has an optional `model` selecting the network device exposed
to the guest. It defaults to `virtio` and must be one of:

| `model` | QEMU device |
| --- | --- |
| `virtio` | `virtio-net-pci` |
| `e1000e` | `e1000e` |
| `e1000` | `e1000` |
| `igb` | `igb` |
| `rtl8139` | `rtl8139` |
| `vmxnet3` | `vmxnet3` |

The host-facing backend remains the interface's persistent TAP for every
model. QEMU device and PCI addresses are generated deterministically. The tool
must validate model support and PCI-slot allocation before changing runtime
state. The VM template must contain the driver required by every selected
model; driver compatibility cannot be proven from the qcow2 format alone.

Each VM interface also accepts `queues`. It is a positive integer, defaults to
`1`, and must not exceed the VM's derived total vCPU count. A value greater
than one enables VirtIO-net multiqueue and causes the tool to create and open
the persistent TAP in multiqueue mode. Multiqueue is supported only when
`model` is `virtio`; every other model requires `queues: 1`.

Each VM interface also accepts an initial guest `mtu`, which defaults to
`1500`. It is rendered into the VM's bootstrap network configuration and does
not set the host TAP MTU; the attached link controls the TAP-side datapath.
VM-interface MTU is a write-once bootstrap value under the rules below.

`vhost` is not a configuration field. Whenever a VM process is launched, the
tool enables the Linux vhost-net accelerator automatically for every
`model: virtio` interface if `/dev/vhost-net` is a character device that the
runtime process can open for reading and writing and the installed QEMU
supports vhost-net. Otherwise it uses QEMU's userspace TAP datapath and reports
that fallback in `ps`; absence of vhost-net is not a preflight error. The tool
does not load `vhost_net` or another kernel module itself. Non-VirtIO models
always use the QEMU userspace datapath.

For `queues: N` where `N > 1`, the tool configures VirtIO-net with multiqueue
enabled and `2 * N + 2` MSI-X vectors, configures the TAP backend with `N`
queues, and passes `N` TAP file descriptors to QEMU. When vhost-net was
selected automatically, it also passes `N` vhost file descriptors.

Changing an existing interface's `model` or `queues` value requires a VM
restart and may change the interface name assigned by the guest, but it must
preserve the interface's MAC address and every writable disk. Generated
NoCloud network configuration matches interfaces by MAC address rather than
by their model-dependent kernel names. A change in host vhost-net availability
does not by itself restart a running VM; availability is reevaluated on its
next start or restart.

VM interface names must match the resource-name character set and be at most
15 characters. An explicit MAC must be a unicast address. When omitted, the
tool generates a deterministic, locally administered MAC from the topology,
VM, and interface names. MAC addresses and declared IPv4 addresses must be
unique within the topology.

A VM interface may have untagged IPv4 configuration and tagged VLAN
subinterfaces:

```yaml
vms:
  - name: app
    template: ubuntu
    interfaces:
      - name: trunk0
        mtu: 1500
        ipv4:
          dhcp: false
        vlans:
          - id: 10
            mtu: 1500
            ipv4:
              addresses: [10.0.10.10/24]
              routes:
                - {to: default, via: 10.0.10.1}
```

VLAN IDs are from 1 through 4094. The generated
`<interface>.<vlan>` guest-interface name must fit the Linux 15-character
limit. A VLAN's optional `mtu` defaults to its base interface's MTU and must
not exceed it. A VM `ipv4` object supports:

- `dhcp`: enable a DHCP client;
- `addresses`: static IPv4 prefixes;
- `routes`: `{to, via}` static routes; and
- `dns_servers`: unique IPv4 resolver addresses for a statically addressed
  layer.

DHCP and static addresses may not be combined on the same untagged or VLAN
layer. `dns_servers` may not be set on a DHCP layer because its initial
resolvers come from the DHCP lease. A route destination is an IPv4 prefix or
`default`; its next hop is an IPv4 address reachable through that layer.
`dhcp_server` is not a VM-interface field. Generated NoCloud network
configuration matches each base interface by MAC, assigns its configured
interface name, and configures its VLANs, IPv4 addresses, DHCP clients, routes,
and static DNS resolvers.

VM interface and VLAN MTUs, layer-3 fields, `ssh_authorized_keys`, and
`root_ca_certificates` are write-once bootstrap inputs, not continuously
reconciled guest configuration.
They are applied only when the VM's writable root disk is first created. The manifest
records their normalized values and a VM-bootstrap hash. While that root
overlay exists, changing any guest MTU, address, DHCP setting, VLAN, route, DNS
server, authorized key, or effective Root CA content or order in the topology
file is a preflight error and no runtime state may be changed. Changing only a
Root CA source path while it still resolves to identical certificate DER is
not a bootstrap change. The fields remain in the topology file as the
historical bootstrap inputs; they do not describe or constrain the current
guest state.

After first boot, only an operator or guest configuration system such as
Ansible may change these settings inside the VM. `topolab` does not inspect,
overwrite, or report drift in them. Package installation, Kubernetes
configuration, services, firewall policy, and every other guest concern are
also outside this tool. When an interface has neither `ipv4` nor `vlans`, the
initial network configuration matches and names the device but does not
configure layer 3 on it. If a backing-template change already requires a new
root disk, the current bootstrap inputs are applied to that new root and
become its new recorded baseline.

#### 4.4.2 VM storage

`root_disk.storage` is required and names an existing top-level storage.
`root_disk.size_gib` is optional and sets the root disk's virtual capacity in
gibibytes. When omitted, the root disk uses the template's virtual capacity.
When specified, it must be greater than or equal to the template's virtual
capacity, rounded up to whole GiB. The tool reads the template capacity with
`qemu-img` during preflight and rejects an incompatible value before creating
or changing any runtime resource. The read-only template is never resized;
the configured capacity applies to the VM's writable root disk.

`root_disk.bus` selects how QEMU presents the root disk to the guest. It
defaults to `scsi` and must be one of `scsi`, `virtio`, `sata`, `nvme`, or
`ide`.

`additional_disks` is an optional ordered list of additional writable data disks. Disk
names use the resource-name rules and must be unique within the VM. Each disk
requires a positive `size_gib` and an explicit `storage`; its optional `bus`
has the same values and defaults as `root_disk.bus`. File data disks are
standalone qcow2 images; LVM data disks are raw thick or thin LVs. Additional disks
have deterministic QEMU identifiers and serial numbers. Their partitioning,
filesystems, mounting, and application use are guest responsibilities.

`root_disk.cache` and each additional disk's `cache` select host caching for
that disk. The value defaults to `none` and must be either `none` or
`writeback`; an explicitly empty value is invalid. `none` enables direct I/O so
disk data bypasses the host page cache, while `writeback` opts that disk back
into buffered host I/O. Both modes keep flushes enabled. Direct I/O avoids
double caching and reduces host page-cache pressure, but can change workload
performance and requires support from the underlying filesystem or block
device. An unsupported direct-I/O backend must make VM startup fail clearly;
the tool must not silently fall back to buffered I/O.

`root_disk.aio` and each additional disk's `aio` select the QEMU asynchronous
I/O backend. The value defaults to `auto` and must be `auto`, `io_uring`,
`threads`, or `native`; an explicitly empty value is invalid. Before any
reconciliation, `auto` probes the resolved QEMU executable and running kernel,
uses `io_uring` when the probe succeeds, and otherwise emits one warning for
the command and uses `threads`. Explicit `io_uring` is strict and a failed
probe is a preflight error. Explicit `threads` and `native` are passed through;
`native` is invalid with `cache: writeback` because native AIO requires direct
I/O. Backend-specific filesystem or device incompatibility fails VM startup
instead of causing another fallback.

Disk declaration order is significant. The root disk is always created and
attached first, before every additional disk, and receives the VM's first disk
boot priority. Additional disks are created and attached in their YAML source
order, from top to bottom. The parser, normalized configuration output,
manifest, and QEMU argument generation must preserve this order; sorting
additional disks by name is forbidden.

The supported bus values map to fixed QEMU controller and device models:

| `bus` | QEMU presentation | Typical Linux name |
| --- | --- | --- |
| `scsi` | dedicated `virtio-scsi-pci` controller, dedicated IOThread, and `scsi-hd` device | `/dev/sdX` |
| `virtio` | `virtio-blk-pci` device | `/dev/vdX` |
| `sata` | `ich9-ahci` controller and `ide-hd` device | `/dev/sdX` |
| `nvme` | `nvme` controller with one namespace backed by the disk image | `/dev/nvmeXn1` |
| `ide` | legacy `piix3-ide` controller and `ide-hd` device | `/dev/hdX` or `/dev/sdX` |

The `scsi` mapping is the VirtIO SCSI Single model: every disk receives its
own controller and IOThread. For controller types that can host multiple
disks, the tool assigns controller, channel, port, target, and unit addresses
deterministically and creates additional controllers when required. The tool
must validate controller limits, PCI-slot availability, and all generated
addresses before changing runtime state.

Linux device letters are not stable identifiers and the typical names in the
table are not contractual. Guest configuration must identify disks through
their deterministic serial-based `/dev/disk/by-id/` links, filesystem UUIDs,
or filesystem labels. Attachment order controls QEMU controller/device
allocation but does not guarantee a global Linux device-letter order when
different bus drivers are mixed.

Root and additional disks persist across `stop`, `start`, `restart`, `up`, and
host reboot. `down` removes every writable disk created for the VM but never
removes the template. Removing a named additional disk from the configuration
removes that disk on the next successful reconciliation; unchanged root and
additional disks are preserved. Adding or removing a disk may require a VM
restart, but must not recreate or discard any unchanged writable disk.
Changing an existing disk's `bus` requires a VM restart and must preserve the
disk image and its contents. A bus change is rejected during preflight if the
requested controller/device combination or address allocation cannot be
created. Root-disk boot-driver compatibility is a template responsibility and
cannot be proven by inspecting the qcow2 format alone.
Reordering `additional_disks` also requires a VM restart because it changes
attachment addresses, but must preserve every disk image and its contents.
Changing a root or additional disk's `cache` also changes only that VM's
restart hash and restarts it without changing storage identity or recreating
any disk. Configurations written before this setting existed intentionally
adopt the `none` default on their next `up`; operators who require the prior
buffered behavior must add `cache: writeback` to each affected disk.
Changing a root or additional disk's configured `aio` policy has the same
restart-only and disk-preserving behavior. The configured policy, including
`auto`, is part of the restart hash; the host-resolved backend is not. A host
capability change therefore takes effect on the next normal VM launch without
changing disk identity.
Configurations written before `aio` existed adopt `auto` on their next `up`.

All configured disks support increase-only resizing. Before applying any
change, the tool must read the actual virtual capacity of the root disk and
every existing additional disk and validate the complete resize plan:

- a desired capacity smaller than the actual capacity is an error;
- a desired capacity equal to the actual capacity is a no-op;
- a desired capacity larger than the actual capacity is a valid increase; and
- the root disk must additionally remain greater than or equal to its template
  capacity.

All disk and template capacity checks must complete successfully before the
tool applies any disk resize or other destructive reconciliation action. Disk
sizes are converted from GiB to bytes using `1 GiB = 2^30 bytes`, with checked
arithmetic.

For a running VM, a valid increase is applied online through the QMP
`block_resize` command using the disk's stable block-node name. The tool must
not stop or restart a VM solely to enlarge a disk. For a stopped VM, the tool
may resize the qcow2 image offline before starting it. It must verify the new
virtual capacity after either operation and record it in the manifest. Disk
growth is idempotent: a retry compares the desired capacity with the actual
image capacity and does not repeat an already completed increase. A partial
multi-disk failure is recorded and retried; completed growth is never rolled
back by shrinking a disk.

Online virtual-disk growth changes the capacity exposed by QEMU. It does not
resize partitions, LVM physical or logical volumes, or filesystems inside the
guest. Guest-side online expansion must be performed by the guest, an operator,
or separate configuration management. Templates intended to consume initial
root-disk growth automatically should enable cloud-init `growpart` and
`resizefs` or provide equivalent behavior.

For `file`, snapshot roots are qcow2 overlays and copy roots are atomically
published standalone sparse qcow2 images without backing files. `lvm-thick`
roots are independent raw copies in fully allocated linear LVs. `lvm-thin`
copy roots are independent thin LVs; snapshot roots share one read-only
template LV per storage and template digest and are writable snapshots of it.
Thin-pool chunk zeroing and full thick-LV wiping are disabled by default and
are enabled only by `zero_new_blocks` and `wipe_on_create`, respectively.

Pool, template-base, staging, and disk LV names are deterministic hashes and
carry topology, storage, and kind ownership tags. Manifest version 3 records
VG, LV, and pool UUIDs plus checkpointed resource intent. A retry completes
interrupted work. Deletion is refused if UUIDs, tags, pool membership, or
unexpected resources do not match the manifest. Declared thin virtual capacity
above pool capacity and data or metadata utilization at or above 80 percent
produce warnings; they do not block operation or autoextend a pool.

#### 4.4.3 VM guest agent and boot

When `guest_agent` is `true`, the tool attaches one deterministic
`virtio-serial-pci` controller and an
`org.qemu.guest_agent.0` VirtIO serial port backed by a root-only Unix socket
below the VM's topology directory. It defaults to `false`. The template is
responsible for installing, enabling, and securing `qemu-ga`; attaching the
channel does not install software in the guest. A non-responsive agent does
not prevent the VM from starting, but `ps` reports it as unavailable. The
agent channel is not exposed outside the host and the tool must synchronize
the protocol before issuing a guest-agent command. Enabling or disabling the
channel requires a VM restart and preserves every writable disk.

`boot` is optional and has the following fields:

- `order`: a non-empty list containing each of `disk` and `network` at most
  once; it defaults to `[disk]`;
- `menu`: enable the OVMF interactive boot menu; it defaults to `false`; and
- `strict`: prevent firmware fallback to devices omitted from `order` when
  supported by OVMF; it defaults to `false`.

The tool implements the order with deterministic device `bootindex` values.
Within `disk`, the root disk is always first and additional disks follow their
declaration order. Within `network`, interfaces follow their YAML declaration
order. The NoCloud seed ISO receives no boot index. The parser, normalized
output, manifest, and QEMU argument generation must therefore preserve VM
interface declaration order as well as additional-disk order. Boot changes
require a VM restart and preserve every writable disk.

### 4.5 Routers

Each router runs in its own Linux network namespace and defines its interface
and layer-3 configuration independently from VMs.

```yaml
routers:
  - name: edge
    interfaces:
      - name: wan
        mtu: 1500
        external:
          sources: [10.0.10.0/24]
          egress_interface: auto
          source_ip: auto
      - name: lan
        mtu: 1500
        vlans:
          - id: 10
            mtu: 1500
            ipv4:
              addresses: [10.0.10.1/24]
              dhcp_server:
                range_start: 10.0.10.100
                range_end: 10.0.10.200
                lease: 12h
                dns_servers: [1.1.1.1]
```

The router object supports only `interfaces` and `routes`; every other field is
invalid under the strict configuration schema.

#### 4.5.1 Router interfaces and layer-3 configuration

Base router interface names must match
`^[A-Za-z0-9][A-Za-z0-9_-]{0,14}$`. A dot is forbidden because
`<interface>.<vlan>` identifies a VLAN subinterface. An explicit MAC must be a
unicast address. When omitted, the tool generates a deterministic, locally
administered MAC from the topology, router, and interface names. Router MAC
addresses and declared IPv4 addresses must be unique within the topology.

A router interface is a veth endpoint moved into the router's Linux network
namespace. Router interfaces do not accept the VM-only `model` or `queues`
fields. A router base interface's optional `mtu` defaults to `1500`. For a
linked interface, it configures the router side while the link MTU independently
configures the fabric side. For an external interface, the same MTU is applied
to the router and generated host transit peers.

A base router interface may declare an `external` object to connect that
router directly to the host without a `links` entry:

```yaml
interfaces:
  - name: wan
    mtu: 1500
    external:
      sources: [10.0.10.0/24, 10.0.20.0/24]
      egress_interface: auto
      source_ip: 203.0.113.10
```

`external.sources` is a required, non-empty list of unique IPv4 prefixes.
`egress_interface` is a host interface name or `auto` and defaults to `auto`,
which selects the interface used by the host default IPv4 route. An explicitly
named egress interface must already exist and must not be owned by
`topolab`; `auto` must resolve successfully during preflight. `source_ip`
is an IPv4 address or `auto` and defaults to `auto`. An explicit address must
be assigned to the resolved host egress interface at preflight and must be
written without a prefix length. `auto` selects the usable IPv4 source address
returned by a host route lookup through the resolved egress interface and
fails preflight if no such address exists. Setting `source_ip` explicitly is
therefore required when the egress interface has multiple addresses and the
operator needs a particular address to be used.

`external` and `nat` are mutually exclusive on one interface. An external
router interface may not set `ipv4`, `vlans`, or `nat`, and it must occur in no
link endpoint. Conversely, a router interface without `external` must occur in
exactly one link. External router interfaces do not support link-level `egress`
impairment. A router may declare at most one external interface.

A regular untagged router interface declares `ipv4` directly on the base
interface and must be referenced by exactly one link:

```yaml
interfaces:
  - name: lan
    mtu: 1500
    ipv4:
      addresses: [10.30.0.1/24]
      dhcp_server:
        range_start: 10.30.0.100
        range_end: 10.30.0.200
        lease: 12h
```

This creates no VLAN subinterface; Ethernet frames sent and received by
`lan` are untagged.

The tool assigns every external interface a private point-to-point `/30`
transit subnet from `198.18.0.0/15`. Allocation is deterministic from the
topology, router, and interface names, but probes forward through the pool to
avoid every host route, topology prefix, and active managed allocation. The
selected `/30`, router-side address, and host-side address are persisted in the
manifest and reused on subsequent `up` operations. Exhaustion of the pool or
an inability to validate host routes is a preflight error. Transit addresses
are implementation details, are displayed by `ps`, and must not be referenced
by configuration.

For an external router interface, the tool creates a veth pair, moves the
router side into the router namespace, and keeps the peer in the host
namespace. It assigns the generated transit addresses, installs a default
route in the router through the host-side transit address, and installs
topology-owned host routes for every `external.sources` prefix through the
router-side transit address. A source prefix must not overlap an unrelated
host route, another external interface's source prefix, or another topology's
managed source prefix.

The tool enables host IPv4 forwarding and creates topology-owned forwarding
and source-NAT rules from `external.sources` through `egress_interface`. An
explicit `source_ip` produces a fixed SNAT rule to that address; `auto` uses
masquerade semantics with the address selected by the host for the egress
interface. The packet source is therefore translated exactly once. Host-
forwarding ownership is reference-counted across active topologies, and the
original host value is restored when the last owner is removed.

A router interface may have untagged IPv4 configuration and tagged VLAN
subinterfaces. VLAN IDs are from 1 through 4094. The generated
`<interface>.<vlan>` name inside the router namespace must fit
the Linux 15-character limit. A VLAN's optional `mtu` defaults to its base
interface's MTU and must not exceed it. Router interface and VLAN MTUs are
continuously reconciled by `topolab`. A router `ipv4` object supports static
IPv4 prefixes in `addresses` and an optional managed DHCPv4 pool in
`dhcp_server`. Routers do not support DHCP clients.

A layer with `dhcp_server` must have at least one static IPv4 prefix. The
inclusive `range_start` and `range_end` must be ordered, must be usable unicast
addresses, and must be contained in one of that layer's static prefixes. Every
`dns_servers` entry must be an IPv4 address. `lease` is `infinite` or a
positive integer followed by `s`, `m`, `h`, `d`, or `w`; it defaults to `12h`
at runtime.

A base router interface may declare `nat` with a required, non-empty list of
unique IPv4 `sources`. The tool creates a source-masquerade rule for those
prefixes with the containing interface as the implicit output interface. NAT
is not allowed on a VLAN subinterface. The router receives the
rule directly in its namespace. Removing or renaming the interface therefore
cannot leave a dangling NAT output reference.

Router interface and VLAN configuration is applied directly inside its
namespace.

#### 4.5.2 Router routes

Static routes belong to the router rather than to an interface's `ipv4`
object:

```yaml
routers:
  - name: branch
    interfaces:
      - name: wan
        mtu: 1500
        ipv4:
          addresses: [10.20.0.2/24]
      - name: lan
        mtu: 1500
        vlans:
          - id: 20
            mtu: 1500
            ipv4:
              addresses: [10.30.0.1/24]
    routes:
      - to: default
        via: 10.20.0.1
        interface: wan
      - to: 10.40.0.0/16
        via: 10.30.0.2
        interface: lan.20
```

Every route requires exactly three fields: `to`, `via`, and `interface`. `to`
is an IPv4 prefix or `default`, and `via` is an IPv4 next-hop address.
`interface` is one logical router-interface name: either a declared base name
such as `wan` or the generated `<base>.<vlan>` name for a declared VLAN, such
as `lan.20`. It may not name an external interface.

The next hop must be a usable address covered by a static prefix on the named
logical interface. A route through a DHCP-only layer is therefore invalid.
Route destinations must be unique within a router. Connected
routes are generated implicitly from interface addresses and must not be
declared. A router with an external interface receives its default route
through the generated host transit connection, so it may not declare another
`default` route.

### 4.6 Switches

```yaml
switches:
  - name: lan
    mtu: 1500
    stp: false
    vlan_filtering: true
```

`mtu` defaults to 1500 and must be between 576 and 9216. `stp` and
`vlan_filtering` default to false. A trunk or an access VLAN other than 1
requires `vlan_filtering: true`.

### 4.7 Links

`links` is the source of connectivity between topology nodes and switches.
An external router interface instead synthesizes its dedicated host
connection as defined in Section 4.5.1. Every declared link has exactly two
endpoints, and a node cannot link to itself.

```yaml
links:
  - name: web-lan
    mtu: 1500
    endpoints:
      - node: web
        interface: eth0
        egress:
          latency_ms: 5
          jitter_ms: 1
          loss_percent: 0.1
          rate_mbps: 100
      - node: lan
        interface: web
        port:
          mode: access
          vlan: 10
```

For a VM or router endpoint, `interface` must name a predeclared interface and
`port` is forbidden. Every VM interface and every non-external router
interface must occur in exactly one link. An external router interface must
not occur in any link.

For a management-host endpoint, `interface` must name a declared
`management_host.interfaces` entry, and `port` is forbidden. Every declared
interface must occur in exactly one link. If its peer is a switch, that switch
endpoint must use access mode; a trunk is invalid.

For a switch endpoint, `interface` declares a unique logical port. `port`
defaults to access VLAN 1 and has one of these forms:

```yaml
port: {mode: access, vlan: 10}
```

```yaml
port:
  mode: trunk
  allowed_vlans: [10, 20, 30]
  native_vlan: 10
```

An access port cannot set trunk fields. A trunk requires at least one unique
allowed VLAN, and its optional native VLAN must be allowed.

`mtu` defaults to 1500 and has the same bounds as switch MTU. Endpoint `egress`
describes traffic leaving that endpoint toward its peer. Values are optional;
latency and jitter must be non-negative, loss must be from 0 through 100, and
rate must be non-negative. Omitted or all-zero impairment creates no qdisc.
Equivalent settings on both endpoints produce a symmetric impaired link.

Supported endpoint combinations include VM-to-VM, VM-to-router,
router-to-router, management-host-to-VM, management-host-to-router,
node-to-switch, and
switch-to-switch connections. A direct non-switch link receives a private
two-port bridge. Multi-access segments must be represented by an explicit
switch.

#### 4.7.1 End-to-end MTU behavior

Every configured MTU uses Linux network-device semantics: it is the maximum
layer-3 packet size in bytes and excludes the Ethernet header and frame check
sequence. A VLAN tag does not reduce the configured layer-3 MTU. Every
management-host, VM, router, VLAN, switch, and link MTU must be from 576
through 9216. Defaults
and ownership are:

- `management_host.interfaces.*.mtu` defaults to 1500 and is continuously
  reconciled on the owned management-host-side interface;
- `vms.*.interfaces.*.mtu` defaults to 1500 and is applied only during VM
  bootstrap;
- a VM VLAN MTU defaults to its base VM-interface MTU and is also a write-once
  bootstrap value;
- `routers.*.interfaces.*.mtu` defaults to 1500 and is continuously
  reconciled inside the router namespace;
- a router VLAN MTU defaults to its base router-interface MTU and is
  continuously reconciled;
- `switches.*.mtu` defaults to 1500 and configures the switch bridge and its
  switch-side port boundary; and
- `links.*.mtu` defaults to 1500 and configures every link-owned TAP, veth,
  shim, and hidden bridge in that link's fabric.

The two ends of a veth may therefore have different MTUs: for example, a
management-host side uses its interface MTU while its fabric peer uses
the link MTU, and a router side uses the router-interface MTU while
its fabric peer uses the link MTU. When enslaving a port would cause Linux to
automatically lower a bridge or otherwise collapse distinct switch and link
values, the tool must insert a deterministic veth/bridge boundary shim. It
must not mutate either configured value to satisfy an incidental kernel-device
constraint.

A VLAN MTU greater than its base interface MTU is invalid. Other adjacent MTU
mismatches are valid because an operator may need to reproduce an MTU black
hole. `config` emits a warning when a declared node-interface or switch MTU
differs from its attached link MTU, but the warning does not invalidate the
topology. Runtime preflight additionally warns when an external router's MTU
exceeds the observed MTU of its selected physical host egress interface; the
tool never changes that physical interface.

For a particular packet path, the effective MTU is the smallest configured or
observed MTU along the participating layer-3 interfaces, VLAN interfaces,
links, switches, and any physical egress interface. The tool sets every
managed host device to its own layer's explicit value and must not silently
replace configured values with a computed common MTU. `ps` reports configured
and observed MTUs for node interfaces and all managed host datapath devices and
marks unexpected runtime drift separately from an intentional configured
mismatch.

Normal endpoint and router IPv4 behavior determines fragmentation and path-MTU
discovery. A router may fragment a packet whose DF bit is clear or generate
ICMP fragmentation-needed for a DF packet that exceeds its egress MTU. A
layer-2 bottleneck, including a smaller link or switch MTU, may silently drop
an oversized frame and cannot generate that ICMP response. `topolab` does
not automatically clamp TCP MSS, rewrite DF, fragment packets in a bridge, or
add synthetic ICMP responses.

For Kubernetes guests, these values describe only the virtual underlay.
`topolab` does not calculate or configure CNI, tunnel, or pod MTU. Ansible
or another operator must choose those values after accounting for the
encapsulation overhead and smallest underlay MTU of the intended path.

### 4.8 Remote target

```yaml
remote:
  ssh: "${LAB_REMOTE:-operator@lab01.example}"
```

The optional `remote` section names the host that runs the topology. It
contains exactly one field, `ssh`, a non-empty string. Interpolation applies
to it. Its value is an OpenSSH destination: `[user@]hostname`,
`ssh://[user@]hostname[:port]`, or a `Host` alias from the controller's SSH
configuration. It must not begin with `-` and must not contain whitespace or
control characters. Unknown fields are errors.

The section is controller input only. It is not part of any hash, is not
recorded in the manifest, and is parsed and ignored by the agent. Adding,
changing, or removing it never causes reconciliation. Section 12.1 defines how
it combines with `--remote`, `--local`, and `TOPOLAB_REMOTE`.

## 5. Runtime mapping

Every active topology owns one deterministic fabric namespace and one
deterministic, always-present management-host namespace. Routers retain their
dedicated namespaces. Switches, hidden bridges, TAPs, link shims, `tc`, capture
sockets, counters, and every QEMU VM process live in the fabric namespace; VMs
and switches do not receive separate namespaces. Router and management-host
connections cross into the fabric through veth pairs. The management-host side
receives its configured addresses, while its fabric peer joins the ordinary
link datapath.

Each declared switch becomes a Linux bridge in the fabric namespace. Switch-
to-switch links use veth pairs plus a boundary shim whenever separate switch
and link MTUs require it. Direct node links receive a hidden bridge. VM
interfaces become persistent TAP devices passed to fabric-namespace QEMU with
`script=no,downscript=no`. Router interfaces become veth pairs whose router
endpoint is moved from fabric into the router namespace.

Without an `external` interface, `up` creates no root-namespace bridge, TAP,
veth, route, or nftables table. Namespace handles and owned processes are the
only permitted root-visible effects. With `external`, only the documented root
uplink, route, forwarding, and NAT resources are allowed. External forwarding
may still interact with the host firewall; scoped management does not carry
that risk.

Each link-endpoint attachment uses a transparent per-port bridge and veth shim.
This allows separate `tc netem` qdiscs for traffic in each link direction while
leaving guest configuration unchanged. VLAN membership is applied to the
topology-switch side of the shim.

Every endpoint shim exposes owned post-impairment observation points in both
directions. The capture collector binds only to those points, and the runtime
records raw interface and per-qdisc statistics needed for link counters. The
observation devices are part of the link datapath rather than temporary
capture resources, so starting or stopping a capture does not rebuild a link.

An external router interface uses its root-side veth peer directly. The root
and router sides receive their generated transit addresses, and the interface
uses no link impairment shim. The tool installs the owned host routes,
forwarding, and NAT rules described in Section 4.5.1.

Router addresses, VLANs, routes, forwarding, nftables, and dnsmasq are
configured directly inside the router namespace. VMs receive their network
configuration on a generated NoCloud seed ISO.

## 6. QEMU and storage contract

QEMU VMs use:

- KVM acceleration and the `q35` machine;
- the CPU model and explicit vCPU topology defined in Section 4.4;
- the selectable disk buses and VM network-device models defined in Section
  4.4;
- deterministic boot indexes, optional VirtIO balloon and guest-agent devices,
  the configured VirtIO-net queue count, and the automatically selected
  vhost-net or userspace network datapath for VMs;
- deterministic UUIDs and MAC addresses;
- QMP over a root-only Unix socket;
- a persistent TAP per configured interface;
- OVMF firmware; and
- a per-node copy of OVMF variables.

Controller, IOThread, block-node, device, namespace, socket, and serial
identifiers are deterministic from the topology, node, logical resource name,
and selected device type.

File disks use qcow2 block nodes and LVM disks use raw block nodes. Root
construction follows the selected storage's copy or snapshot semantics from
Section 4.4.2; additional disks are independent. Templates are not
chmod-modified or opened as writable disks. Running growth extends an LV
before QMP `block_resize`. File growth uses QMP without invoking `qemu-img
resize` on an image currently opened by QEMU.

For each writable disk, QEMU's protocol node (`file` or `host_device`) and
format node (`qcow2` or `raw`) receive the normalized cache policy. `none` maps
to `cache.direct=on,cache.no-flush=off`; `writeback` maps to
`cache.direct=off,cache.no-flush=off`. Explicit qcow2 backing-chain protocol
and format nodes receive the same policy. This guarantee covers VM root and
additional disk data only; firmware, seed media, metadata, guest RAM, and
unrelated host I/O are outside its scope.
The effective AIO backend is applied only to protocol nodes, where QEMU accepts
the `aio` option. File-backed qcow2 snapshot roots use explicit deterministic,
read-only backing format/protocol nodes rather than automatic backing-file
opening, so their template protocol nodes receive the same cache and AIO
settings as the writable overlay. Standalone file disks and raw LVM disks have
no backing nodes.

The generated NoCloud seed contains `meta-data`, `user-data`, and
`network-config`. For a VM, the seed contains only its hostname, locked-
password `torob` user, initial authorized keys, trusted Root CA certificates,
and initial interface names, MTUs, VLANs, IPv4 configuration, routes, and DNS
resolvers. Its instance ID
incorporates the topology name, node name, storage-identity hash, and recorded
VM-bootstrap hash. The seed and instance ID remain unchanged for the lifetime
of that writable root disk. The manifest retains the normalized bootstrap values and
the digest of the exact rendered seed. A missing VM seed may be reconstructed
only from that recorded bootstrap snapshot after the current configuration has
passed the bootstrap-hash check; it must not be reconstructed from unrecorded
current guest state.

Fabric switches, link attachments, MTU, `egress` impairment,
management-host interfaces, and generated TAP, veth, bridge, or nftables
details do not affect a VM-bootstrap hash. Tool-generated VM user-data must be
idempotent, must set `ssh_deletekeys: false`, must not clean or regenerate the
guest machine ID, and must contain no package, service, Kubernetes, or
application configuration.

A compatible VM template must enable the NoCloud data source and cloud-init
network rendering. A VM using `root_ca_certificates` additionally requires the
cloud-init CA certificates module and the guest OS trust-store tooling it
invokes.

VM shutdown first requests `system_powerdown` over QMP, waits up to 30
seconds, then uses bounded TERM and KILL fallbacks. Before signaling, the tool
verifies that the PID command line identifies the expected topology and node.

## 7. State and reconciliation

All persistent and volatile state is stored beneath the resolved
`working_dir`. Tool-created paths use this layout:

```text
<working_dir>/.topolab/topologies/<name>/
<working_dir>/.topolab/shared/
<working_dir>/.topolab/blobs/
<working_dir>/.topolab/jobs/<name>/
<working_dir>/captures/<name>/
```

The topology directory contains its manifest, node files, logs, sockets, and
other topology-specific state. The shared directory contains coordination
state such as host IPv4-forwarding ownership. The captures directory contains
diagnostic artifacts and is intentionally not topology state; reconciliation
does not read it and `down` does not remove it. User-managed content elsewhere
beneath `working_dir`, including `templates/`, is not topology state. No
runtime file is created under a fixed `/var/lib` or `/run` location.

The blobs directory holds uploaded templates shared by every topology in the
working directory (Section 12.5). The jobs directory holds the records of
remote lifecycle jobs (Section 12.6). Both exist only after remote operation
creates them. Like the captures directory, neither is topology state: `down`
does not remove them, and `up` never reads job records. A blob is removed only
by `templates prune`, and a job record is removed only by job retention or
`jobs prune`.

Only one operation may mutate a topology at a time; mutations take an
exclusive file lock. Both management-host and router `exec` hold a shared lock
for the full child lifetime.

The version-2 manifest records a datapath version; deterministic fabric and management-
host namespace names and ownership aliases; namespace scope for every managed
bridge, TAP, link, nftables table, counter, and ownership operation; distinct
management-host, router, fabric, and root nftables state; and QEMU namespace
identity. It distinguishes root-external resources from fabric resources. It
also records configuration and host-network hashes, each VM's
normalized bootstrap values and VM-bootstrap hash, template digests, VM
storage-identity and restart hashes, every writable disk's backend, path,
format, provisioning, observed capacity, VG/LV/pool UUIDs and ownership tags,
storage pool utilization and warnings, restart-only hardware settings, requested balloon targets,
TAP names, external-interface host peers, transit allocations, host routes,
and NAT state, management-host interfaces and isolation rules, per-link
datapath generation IDs and start times, process endpoints, and owned host
resources.

`up` follows these rules:

1. Validate the file and all host prerequisites. Refuse an active legacy
   root-datapath manifest with an actionable instruction to run `down` first.
2. Validate templates, every existing VM's bootstrap hash, and the complete
   desired-versus-actual disk plan before any destructive change.
3. Compute template, host-network, storage-identity, VM-bootstrap, and per-VM
   restart hashes.
4. Allocate and checkpoint required staging or replacement storage, then stop VMs affected by a network rebuild, backing-template change, or
   restart-only QEMU hardware-presentation change.
5. Apply only the explicit disk deletions and root-disk replacements defined
   below. Network changes never add a disk to this deletion plan.
6. Create and immediately checkpoint the fabric and management-host namespaces
   before network objects, then reconcile missing owned resources in their
   recorded scope. Missing or wrongly owned topology namespaces are ownership
   drift, not an empty topology; wrong ownership aborts before mutation.
7. Reuse unchanged disks, create newly declared additional disks, apply
   validated disk increases online where the VM is already running, and
   request changed balloon targets without restarting eligible running VMs.
8. Create VM NoCloud seeds for new or root-replaced VMs, reconstruct missing
   VM seeds only from validated bootstrap values, and regenerate missing
   runtime helpers.
9. Start every desired VM and atomically save the new manifest.

A VM's storage-identity hash contains the template path and content digest plus
the root storage identity and provisioning mode. In remote mode the template
path is the blob path (Section 12.5), which is a function of the digest, so the
hash changes only when the content changes. A template-identity change
replaces that root disk because an existing writable disk
must not be silently rebased onto different template contents. It preserves
every additional disk. Renaming a VM is represented as removal of the old VM
and creation of a different VM, not as a storage-identity change.

All network configuration is excluded from the storage-identity hash. This
includes the complete `management_host`, `switches`, and `links` sections;
link endpoints,
MTU, access/trunk configuration, and `egress` impairment; every VM interface's
membership, declaration order, MAC, device model, queue count, IPv4
configuration, VLANs, and link attachment; every router interface's MAC, MTU,
IPv4 configuration, VLANs, and link attachment; router routes, DHCP-server
configuration, NAT, and `external`; and all generated TAP, veth, bridge, and
nftables details. Changing any combination of these properties must preserve
the root disk and every additional disk of every surviving VM.

An attempted change to a VM's write-once guest MTU, address, DHCP, VLAN, route,
DNS, authorized-key, or Root CA bootstrap inputs follows the preflight
rejection rule in Section 4.4.1. Rejection occurs before a VM is stopped or any
host networking or disk is changed.

A network change may require an affected VM to stop and start when its
presented NIC hardware or safe host-datapath reconciliation requires it. Such
a stop is a disk-preserving restart, never node recreation. CPU
model/topology, `memory_mb`, balloon-device presence, guest-agent presence,
boot settings, additional-disk membership, desired disk capacities, disk
declaration order, disk bus settings, and VM interface model/queue settings
are also reconciled outside the storage-identity hash. Changing any of those
fields except only a disk increase or balloon `target_mb` may require a VM
restart, but must not recreate the root disk or any unchanged additional
disk. Disk increases and balloon-target changes use their online procedures
when the VM is running.

An unchanged root disk and unchanged additional disks survive `stop`,
`start`, `restart`, every network change, and host reboot. After reboot the
operator reruns `up` to restore Linux networking and QEMU processes. A root
disk is deleted only when its VM is removed, its template identity
changes, or `down` is executed. An additional disk is deleted only
when its name is explicitly removed from its VM, its VM is removed, or `down`
is executed. Template changes and network changes never delete an additional
disk. Increasing a capacity never deletes a writable disk.

If creation fails after partial host changes, the manifest records known
partial state so a subsequent `up` can clean and retry it. Template validation
and preflight failures occur before existing topology resources are replaced.

## 8. Packet capture and counters

Packet capture and counters observe only tool-owned host datapath devices.
They require the topology network to exist and pass the same ownership checks
used before cleanup. They do not require a guest agent, software inside a VM,
or changes to a guest interface.

### 8.1 Packet capture

`capture` selects one declared link and one of its exact `NODE:INTERFACE`
endpoints. The endpoint is an observation location, not a capture filter.
Direction is relative to that endpoint:

- `egress` observes frames that have passed the selected endpoint's egress
  impairment and entered the link fabric;
- `ingress` observes frames that have passed the peer endpoint's egress
  impairment and are about to be delivered to the selected endpoint; and
- `both` records both observation streams in one pcapng file.

Packets discarded by configured loss, rate limiting, an MTU bottleneck, or
another egress failure before the observation point do not appear in the
capture. Their drops are reported by `counters`. Capturing must not add a
qdisc, place an interface into promiscuous mode visible to a guest, or change
forwarding, VLAN, MTU, impairment, or counter behavior.

The output is pcapng with nanosecond-resolution UTC timestamps. It contains a
separate Interface Description Block for ingress and egress when both are
selected, and Enhanced Packet Blocks carry the standard direction flag.
Interface names and comments identify the topology, link, endpoint, direction,
and datapath generation. The capture preserves the frame as seen on the cable
side of the endpoint: an access-port frame is untagged, a trunk frame retains
its VLAN tag except for its configured native VLAN, and a node-generated VLAN
tag remains visible. The collector must preserve or reconstruct a VLAN tag
reported as Linux packet metadata rather than silently omitting it because of
host offload behavior.

`--snaplen` defaults to 262144 and must be from 64 through 262144 bytes.
`--duration` is a positive integer followed by `ms`, `s`, `m`, or `h`.
`--count` is a positive total captured-packet count across the selected
directions. At least one of duration or count may be omitted; when both are
omitted, capture continues until interrupted. When both are supplied, the
first limit reached stops capture. Interrupting the command must close a valid
pcapng file and exit successfully; a datapath removal or ownership change
closes the file and exits nonzero with a diagnostic.

When `--output` is omitted, the tool chooses a collision-resistant filename
beneath:

```text
<working_dir>/captures/<topology>/<link>-<UTC timestamp>.pcapng
```

A relative output path is resolved against `working_dir`. An absolute output
path is accepted only when its fully resolved location is beneath
`working_dir`. Symlink escape is rejected, missing parent directories beneath
the working directory are created safely, and an existing output file is
never overwritten. Capture files are diagnostic artifacts rather than
topology state: `up` does not read them and `down` does not remove them. In
remote mode the capture is written on the controller, not under the host's
`working_dir`, as Section 12.7 defines.

### 8.2 Link counters

Counters are cumulative unsigned 64-bit values for one datapath generation.
Each link endpoint reports:

- ingress and egress packets and bytes at the endpoint observation points;
- packets and bytes offered to and delivered by the endpoint's egress
  impairment stack;
- configured-loss and netem queue drops;
- rate-limiter drops, overlimits, requeues, and current queue backlog in
  packets and bytes; and
- underlying interface receive/transmit errors and drops, including MTU drops
  when the kernel exposes them separately.

The implementation must maintain distinct impairment stages or equivalent
owned counters so configured-loss drops are not incorrectly reported as
rate-limit drops. A value unavailable from the running kernel is represented
as `null` in JSON and `-` in table output; it must not be fabricated as zero.

Every result includes the topology and link names, collection timestamp,
datapath generation ID, and generation start time. Counters are monotonic
within a generation and reset only when that link's host datapath is rebuilt.
There is no counter-reset operation. Automated tests obtain
deltas by taking before and after snapshots and must reject a delta if the
generation ID changed between them.

JSON output is a single object with a `links` array. Each link contains its
`name`, `generation_id`, `generation_started_at`, and an `endpoints` array.
Each endpoint contains `node`, `interface`, `ingress`, `egress`,
`impairment`, and `device` objects holding the fields defined above. Field
names and numeric units are stable; byte counters are bytes and backlog values
are explicitly suffixed `_packets` or `_bytes`. Table output is a human-readable
projection of the same data.

## 9. Logs

A small internal collector listens on a per-VM Unix socket. QEMU connects its
serial character device to that socket with reconnect enabled. The collector
appends JSONL records containing an RFC3339-compatible UTC timestamp and one
serial line.

Logs persist with the writable VM layer. They survive process stop/start and
every network change. They are removed only when the VM is removed, its
root disk is replaced after a template-identity change, or `down`
is executed. The collector PID is verified against its command line and socket
before termination.

## 10. Ownership and safety

Generated Linux interface and namespace names are deterministic hashes
constrained to their kernel limits. Managed interfaces and namespace loopback
devices receive ownership aliases. Nftables tables contain ownership comments
and are tracked separately by namespace scope.

Every temporary `setns` operation locks its OS thread and restores the
original namespace on success, error, and panic. QEMU process membership is
verified against the recorded fabric namespace during `ps`, reconciliation,
`start`, `stop`, and `restart`. Persistent multiqueue TAP descriptors are
opened while temporarily entered into that namespace, without changing vhost-
net allocation or QEMU file-descriptor numbering. Capture sockets and counter
or `tc` inspection likewise execute in each resource's recorded namespace.

Before cleanup performs any destructive action in any scope, it verifies all
surviving interface, root peer, namespace, and nftables ownership markers. A
missing resource is
treated as recoverable drift. A present resource with the wrong marker causes
the operation to fail without deleting any recorded network resource.

The implementation invokes external commands with argument arrays and never
through a shell. Generated directories beneath `working_dir` are root-only.
Cleanup verifies recorded ownership and never recursively deletes an
unverified working directory or an unrecorded file. Before deleting a
namespace it stops all recorded QEMU and helper processes, lists remaining
namespace PIDs, and refuses deletion when any unowned process remains.

In remote mode the agent is the only component that changes the host. The
trust boundary, privilege model, and handling of transferred secrets are
defined in Section 12.9. Blob and job records under `working_dir` obey the same
root-only permission rule as every other generated directory.

## 11. Conformance checks

This specification does not prescribe a repository layout, package path,
build command, test framework, or location for implementation files. A
conforming implementation provides an executable named `topolab` and its own
automated unit and root-required integration tests.

The following complete topology is the implementation-independent smoke-test
input. It is valid after `TOPOLOGY_TEST_TEMPLATE` is set to an existing,
OVMF-bootable, NoCloud-compatible qcow2 image and `TOPOLOGY_TEST_SSH_KEY` is
set to one valid OpenSSH public key:

```yaml
name: smoke

templates:
  - name: ubuntu
    path: "${TOPOLOGY_TEST_TEMPLATE:?a test template is required}"

management_host:
  interfaces:
    - name: management
      mtu: 1500
      ipv4:
        addresses: [192.0.2.1/24]

vms:
  - name: node
    template: ubuntu
    ssh_authorized_keys:
      - "${TOPOLOGY_TEST_SSH_KEY:?a test SSH public key is required}"
    interfaces:
      - name: management
        mtu: 1500
        ipv4:
          addresses: [192.0.2.10/24]
          dns_servers: [192.0.2.53]

switches:
  - name: management
    mtu: 1500
    stp: true

links:
  - name: management-host
    mtu: 1500
    endpoints:
      - node: management_host
        interface: management
      - node: management
        interface: management-host
        port: {mode: access, vlan: 1}
  - name: node-management
    mtu: 1500
    endpoints:
      - node: node
        interface: management
      - node: management
        interface: node
        port: {mode: access, vlan: 1}
```

The test harness must write that document to a temporary file, use a separate
temporary `working_dir`, and exercise the public CLI rather than importing
implementation internals. At minimum it must verify:

- `config` accepts the smoke topology, emits normalized YAML, and performs no
  runtime mutation;
- unknown fields, unsafe paths, missing variables, invalid ownership, and all
  documented cross-field violations are rejected before mutation;
- `up`, `ps`, `stop`, `start`, `restart`, and `down` satisfy their idempotency,
  persistence, and ownership contracts;
- each router is realized in its own owned network namespace, with no QEMU
  process, writable disk, NoCloud seed, or TAP interface;
- inline and file-backed Root CAs are validated before mutation and become
  trusted through the guest system CA bundle after initial bootstrap;
- VM bootstrap values are applied initially and write-once changes are
  rejected without stopping the VM or changing a disk;
- all documented network-only changes preserve the root disk and every
  additional disk;
- online disk growth and balloon-target changes follow their respective
  convergence contracts; and
- `exec` and its descendants observe the management-host namespace while QEMU
  VMs observe the distinct fabric namespace;
- a management-host attachment permits controller-to-VM traffic while its
  forwarding isolation rules prevent it from becoming a transit router; and
- without `external`, root interfaces, routes, nftables, and existing iptables
  behavior are byte-for-byte unchanged across `up` and `down`.

The integration suite must send a known Ethernet/IP exchange across a managed
link and verify that:

- ingress and egress pcapng streams contain the delivered frames at their
  defined observation points with the expected VLAN representation;
- offered and delivered packet/byte counters increase within one generation;
- a deterministic 100-percent configured loss produces impairment drops and
  no delivered capture frame; and
- rebuilding the link changes its generation ID rather than presenting reset
  counters as a continuation of the previous generation.

The remote-mode suite uses a host that is reachable over SSH (it may be the
controller's own machine through `ssh localhost`), a controller account
without root privilege, and a host account that may run the agent as root. It
must verify at least that:

- before `agent install`, a remote command fails with `agent_missing` and
  changes nothing on the host; after the install, the agent file is root-owned
  and its SHA-256 equals the controller executable's digest; a second install
  succeeds without change;
- a host account that cannot run the agent through `sudo -n` produces
  `privilege_denied`, and the message shows the example sudoers rule;
- `config` and `version` start no `ssh` process;
- the smoke topology's `up`, `ps`, `exec`, `logs`, `capture`, `counters`,
  `stop`, `start`, `restart`, and `down` in remote mode produce the same host
  state as in local mode, and `ps --format json` has the same fields;
- `capture` writes its pcapng file on the controller only, never overwrites an
  existing file, and never follows a symbolic link;
- the first `up` uploads each used template once, a later `up` (also after
  `down`) transfers no template byte, an interrupted upload resumes from the
  reported offset, and a corrupted transfer leaves neither a blob nor a
  partial file;
- a template with `remote: true` is neither hashed nor uploaded by the
  controller: `up` transfers no template byte, the root disks use the host file
  in place, `config` in remote mode does not look for the file on the
  controller, and a missing, unreadable, or non-qcow2 host file fails with
  `host_template_invalid` before any job exists;
- a template without a `path`, a template with `remote: true` and a relative
  `path`, and a `remote` value that is not a boolean are rejected before any
  mutation, and in local mode the file of a template with `remote: true` is
  checked like any other template file;
- terminating the `ssh` process during `up` does not stop the job: it ends
  `succeeded`, every VM is running, `jobs attach` replays the complete output,
  and the controller either resumes without duplicated output within
  `--reconnect-timeout` or exits with status 3 and prints the job identifier;
- a lifecycle command while a job runs for the same topology fails with
  `job_active` and changes nothing; `jobs cancel` ends a job as `cancelled`,
  and a later `up` repairs the topology;
- ending the connection during `exec` terminates the child and releases the
  shared lock, and a PTY `exec` returns the exact child exit status and follows
  window resizes;
- `templates prune` removes only unreferenced blobs that are older than
  `--min-age`;
- adding, changing, or removing the `remote` section changes no hash and causes
  no reconciliation, and `--local` overrides it; and
- the checks above pass on Ubuntu 24.04, Ubuntu 26.04, and Proxmox VE 9,
  including the firmware lookup order of Section 2.

## 12. Remote operation

Remote operation lets a Linux controller manage a KVM host that is a different
machine. The controller runs the `topolab` executable as an unprivileged user.
Through SSH it starts a second copy of the same executable, the agent, on the
host. The agent performs every operation that Sections 3 through 10 define, on
the host. In remote mode the controller never creates, changes, or inspects
host resources itself. A command behaves as those sections define unless this
section states a difference.

### 12.1 Selecting remote mode

The first applicable rule decides the mode:

1. `--local` selects local mode.
2. `--remote DESTINATION` selects remote mode.
3. A non-empty `TOPOLAB_REMOTE` environment variable selects remote mode with
   that destination.
4. A `remote` section in the topology file (Section 4.8) selects remote mode.
5. Otherwise the invocation is in local mode.

Supplying `--local` together with `--remote` is a usage error. `version`
ignores these rules. `agent install` uses only rules 2 and 3 and requires one
of them. DESTINATION has the syntax and restrictions of `remote.ssh` in
Section 4.8.

`--ssh-opt OPTION` may repeat. The controller passes each value to `ssh` as
the argument of `-o` and never through a shell. `--reconnect-timeout D` is a
positive integer followed by `ms`, `s`, `m`, or `h`, or the value `0`. It
defaults to `2m` and applies only to lifecycle jobs (Section 12.6).

In remote mode the locations of inputs and outputs are:

| Item | Location |
| --- | --- |
| Topology file, template files with `remote: false`, Root CA files named by `path` | controller |
| Interpolation variables | controller environment |
| `capture --output` and the template digest cache | controller |
| Template files with `remote: true` | host |
| Working directory, state, disks, sockets, logs, blob store, job store | host |
| Helper executables and firmware named by `--qemu-system`, `--ovmf-code`, and the other helper options | host |

`--working-dir` and `WORKING_DIR` name a path on the host. The path must be
absolute; a relative value is a usage error. The controller does not contact the
host to check it. The agent applies every rule of Section 4.1 and reports a
failure as `working_dir_invalid`.

The controller parses and validates the topology with the rules of Section 4,
except the working-directory checks, before it contacts the host. A failure
at this stage never reaches the host. In remote mode `config` runs only on the
controller: it starts no `ssh` process, validates the files of templates with
`remote: false` on the controller, does not look for the files of templates with
`remote: true`, requires a non-empty working directory but checks it only lexically, and prints
the normalized YAML including `remote`.

### 12.2 Transport and protocol

The controller starts `ssh` with an argument vector and never through a local
shell. The arguments appear in this order: `-T`; each `--ssh-opt` value as
`-o OPTION`; then `-o BatchMode=yes`, `-o ServerAliveInterval=15`, and
`-o ServerAliveCountMax=3`; then `--`; then DESTINATION; then the remote
command. User options come before the defaults so that they take precedence,
because `ssh` uses the first value it obtains for an option. The controller
never passes an option that weakens host-key verification. The controller
starts `ssh` in its own process group, so that an interrupt typed in the
controller's terminal reaches only the controller, which decides what it means.
The standard error of `ssh` is passed to the controller's standard error
unchanged.

The remote command is `sh -c` with one constant script (Section 12.3) in which
only the agent digest varies. The script contains no single quote, backslash,
or exclamation mark, so every common login shell passes it unchanged.

The standard input and output of the `ssh` process carry the protocol. One SSH
session carries exactly one command. The agent session ends when its command
ends or when the stream closes. No agent session is long-lived and none listens
on a socket or port.

The handshake and stream rules are:

- The controller sends a hello with the protocol number `1`, its executable
  digest, and a random 128-bit nonce.
- The agent answers with a fixed preamble that contains the nonce, then its own
  hello with its executable digest, CPU architecture, kernel release, and
  version string. The version string is for display only.
- The controller scans the agent's standard output for the preamble. It
  discards at most 64 KiB of earlier bytes, such as output of login scripts, and
  copies them to its standard error. If no preamble arrives, the command fails
  with `protocol_error`.
- The controller fails with `agent_mismatch` when the agent's digest or
  architecture differs from its own.
- Each side sends a ping after 15 seconds of silence and answers every ping
  with a pong. An agent session ends after 60 seconds without any frame from
  the controller. The controller treats 60 seconds without any frame from the
  agent as loss of the connection.
- The agent and the job runner ignore `SIGPIPE`.

The byte layout of frames, the preamble, and the numeric codes are private.
They are not part of this specification, because the controller always starts
the agent whose digest equals its own. An implementation must provide:

- length-prefixed, typed frames with a payload of at most 8 MiB; a larger frame
  is a `protocol_error`;
- independent directions, so that a stalled output stream never delays a
  cancel, signal, or resize frame;
- the frame kinds hello, request, standard input data and end, standard output
  data, standard error data, resize, signal, cancel, ping, pong, blob status,
  blob data, blob end, blob acknowledgement, blob result, job notice, error, and
  exit; the agent sends `exit` last, and `error` carries a code and a message;
  and
- command output that passes through unchanged, so that `ps --format json` and
  `counters --format json` are byte-identical to local mode for the same state.

### 12.3 Agent installation and privilege

The agent for a controller is the controller's own executable. It is installed
on the host at:

```text
/usr/local/lib/topolab/<sha256>/topolab
```

`<sha256>` is the lowercase hexadecimal SHA-256 digest of the executable's
bytes. The directory and the file are owned by root and are not writable by
group or others. An installed file is never modified. Executables with
different digests coexist, and no command removes an installed agent. A
running serial collector or helper therefore stays valid after an upgrade. An
installed agent is software, not runtime state, so the rule in Section 7 about
fixed `/var/lib` and `/run` locations does not apply to it.

The remote command script does the following, in order:

1. Exit with status 97 when the agent file is not an executable file.
2. When the effective UID is 0, run the agent directly with `exec`.
3. Otherwise exit with status 98 unless `sudo -n -l` reports that the agent
   command is allowed, and then run it with `exec sudo -n`.

The agent exits with status 99 when its effective UID is not 0 or when its
standard input or output is a terminal. The controller maps 97 to
`agent_missing`, and 98 and 99 to `privilege_denied`. The agent sets its own
`PATH` and does not rely on the environment that `sudo` or the login produces.

The SSH user that the operator supplies must already be allowed by the host's
`sudoers` policy to run that command as root without a password, or must be
root. TopoLab never reads, writes, or generates `sudoers` files. A
`privilege_denied` message prints the following example for the current digest
and states that the rule gives that account root-equivalent control:

```text
operator ALL=(root) NOPASSWD: /usr/local/lib/topolab/<sha256>/topolab agent --stdio
```

A rule that covers several digests must use one bracket expression per hex
digit and must not use `*`, because `*` also matches `/` and `..`. Proxmox VE
does not install `sudo` by default. On such a host the operator either
supplies a root account or installs `sudo` and a rule.

`agent install` installs the agent in two phases. Both phases use the same
destination. It fails when the controller's executable is not Linux x86-64.

1. As the SSH user and without `sudo`, it creates a private staging directory
   with mode 0700, sends the executable over the SSH connection, and checks its
   SHA-256.
2. Through an interactive `ssh -t` session, so that `sudo` may prompt, or
   directly when the SSH user is root, it creates a root-owned temporary
   directory beside the destination, copies the staged file into it, computes
   the SHA-256 of that root-owned copy, sets mode 0755, and renames the
   directory to `<sha256>`. When the destination already exists, it verifies
   the installed digest and changes nothing. It fails when the install root
   (`/usr/local/lib/topolab`) is not a real directory owned by root, or is
   writable by group or others.

The command removes the staging directory in every case. It prints a warning
when the host's login manager is configured to kill a user's processes at
logout (`KillUserProcesses=yes`), because that setting also kills jobs and
QEMU processes (Section 12.6). The controller never installs an agent
implicitly: a remote command whose agent is missing fails with `agent_missing`
and the instruction to run `agent install`.

### 12.4 Requests and inputs

For each command the controller sends one request that contains:

- the command name and its arguments;
- for commands that need a topology, the topology text exactly as read, and the
  controller directory that contains the topology file (`base_dir`);
- the interpolation variables that the topology looked up, including the names
  that were unset, with a flag that tells whether each was set;
- the working directory and the helper overrides;
- for `up`, the name, SHA-256 digest, and size of each template with
  `remote: false` that at least one VM uses; a template with `remote: true` is
  not listed;
- the contents of each Root CA file that a VM names by `path`, keyed by its
  resolved controller path; and
- for `exec` and `router exec`, the terminal size and the value of `TERM`.

A Root CA file may not exceed 1 MiB, and a request may not exceed 8 MiB. The
controller refuses a larger request before it sends anything
(`request_too_large`). The agent handles a request as follows:

1. It parses the topology as Section 4 defines, resolves relative paths against
   `base_dir`, and uses only the request's variables as the interpolation
   source. A variable that the request does not list is a `protocol_error`; the
   agent never treats it as unset and never consults its own environment. It
   parses and ignores the `remote` section.
2. It behaves as if every transferred Root CA file had the transferred contents
   at its path.
3. For `up`, it replaces the path of each used template with `remote: false` by
   its blob path (Section 12.5). A used template with `remote: false` that the
   request does not list is an error. It leaves a template with `remote: true`
   unchanged.
4. It computes the SHA-256 of its normalized configuration and compares it with
   the value that the controller sent from its own parse, before the host
   changes in any way. A difference is a `protocol_error`.
5. For `up`, it checks each used template with `remote: true` on the host, as
   Section 12.5 requires, before it creates a job.
6. It runs the command with the semantics of Sections 3 through 10.

The agent validates every request field as untrusted input. In particular, it
takes the identity of an `exec` child only from its own environment
(Section 12.7), never from the request.

### 12.5 Template transfer and blob store

Template transfer applies to `up` and to templates with `remote: false`. The
controller validates every declared template file with `remote: false` as
Section 4.2 and `up` require, and computes the SHA-256 digest and size of each
such template that at least one VM uses. It may cache a digest
keyed by device, inode, size, modification time, and change time. It must
re-hash when any of those values differs, and it must check the file again after
a transfer. A change during the transfer causes one new hash and one retry.

The blob store is `<working_dir>/.topolab/blobs/`. It has these paths:

```text
sha256/<digest>          completed blobs
partial/<digest>         uploads in progress
locks/<digest>.lock      per-digest locks
.gc.lock                 coordination lock for prune
```

The directories are root-only. Every path component of a blob is derived from
its digest, so the template path that Section 7 records depends only on the
content.

For each digest the agent reports `present`, `partial` with the byte offset
that equals the partial file's size, or `absent`. The controller sends the
missing bytes from that offset in chunks of at most 4 MiB. The agent
acknowledges each chunk, and the controller keeps at most four chunks
unacknowledged. The controller shows progress on its standard error. It uses no
compression unless the operator passes an SSH option for it.

The agent holds the per-digest lock for the whole upload. A second uploader of
the same digest waits and then asks again. At the start of an upload the agent
reserves the declared size in the file system and fails with `blob_no_space` if
it cannot. After the last chunk the agent:

1. computes the SHA-256 digest of the partial file; on a difference it deletes
   the file and fails with `blob_digest_mismatch`, and the controller restarts
   once from offset 0;
2. validates the file as Section 4.2 requires (qcow2 format, no backing file)
   and fails with `blob_invalid` otherwise;
3. flushes the file and its directory to stable storage;
4. sets mode 0444; and
5. renames it into `sha256/` without replacing an existing file.

An `up` that selects a blob updates its modification time. When a present blob
no longer matches its name, the agent fails with `blob_corrupt` and leaves the
file in place. No command other than `templates prune` removes a blob.

While an `up` session checks for or uploads blobs, it holds `.gc.lock` in shared
mode until its job is registered. `templates ls` prints, for each blob, the
digest, the size in bytes, the last-use time, and the names of topologies that
reference it. `templates prune [--min-age D]` takes `.gc.lock` exclusively and
fails after 10 seconds if it cannot. It then removes each blob that is not
protected and is at least `D` old, where `D` defaults to `24h`. It also removes
partial files of that age whose lock is free, and it skips every digest whose
lock is busy. A blob is protected when any of these names it:

- the `template_path` of a manifest under `topologies/`;
- the backing file recorded in the qcow2 header of any file under
  `topologies/` (this covers overlays that exist before their manifest entry
  does); or
- the request of a job that is not in a terminal state.

A template with `remote: true` takes no part in the transfer. The controller
does not read, hash, or list it, and the agent makes no blob for it. Before `up`
creates a job, the agent checks each such template that at least one VM uses.
The file must exist, must be a regular file, must be a qcow2 image without a
backing file (Section 4.2), and must be readable by the agent. Otherwise `up`
fails with `host_template_invalid`, creates no job, and changes nothing on the
host.

`up` then uses the file in place, as local mode does. It computes the content
digest of the file on every `up`, and Section 7 records the host path and the
digest. TopoLab never modifies, moves, or removes the file, and
`templates prune` never touches it. A change of its content is a
template-identity change (Section 7). The operator must not change a file that
existing root disks depend on until the dependent VMs are removed.

### 12.6 Lifecycle jobs

`up`, `start`, `stop`, `restart`, and `down` run as jobs. A job is a detached
process on the host. It outlives the SSH session, the agent session, and the
controller.

The controller creates a random 128-bit job identifier, written as 32
lowercase hexadecimal digits, and sends it in the request. A request that
carries an existing identifier attaches to that job when its request digest is
equal and fails with `protocol_error` otherwise. A reconnection therefore never
creates a second job.

Job records are stored in `<working_dir>/.topolab/jobs/<name>/<job-id>/`:

- `status.json` holds the record version, state, command, arguments, creation,
  start, and finish times (UTC, RFC 3339), exit status, and the runner identity:
  PID, process start time from `/proc/<pid>/stat`, boot identifier, and
  executable path;
- `secrets.json` (mode 0600) holds the topology text, `base_dir`, the request
  variables, the Root CA contents, and the helper overrides; and
- `output.log` holds the output records.

The agent creates a job while it holds the exclusive lock
`jobs/<name>/.lock`:

1. It fails with `job_active`, naming the identifier, when another job of the
   topology is not in a terminal state and its runner is alive. A job that waits
   for the topology lock is active. A job whose runner is gone is first marked
   `lost`.
2. It writes the records with the state `pending`.
3. It starts the runner: the agent executable with an internal subcommand, in
   its own session, with `SIGHUP` ignored, and with no standard stream
   attached to the SSH session.
4. It keeps the lock until the runner records `running`, for at most 10
   seconds. After that it marks the job `failed`.

The agent session never takes the topology lock. The runner takes it exactly as
local mode does (Section 7). While the runner waits for the lock it writes one
notice to its output.

A job has one of these states: `pending`, `running`, `succeeded`, `failed`,
`cancelled`, or `lost`. The last four are terminal. `lost` means that the
runner no longer exists, which the agent detects when the PID is gone or when
its start time, executable, or boot identifier differs. The first agent that
observes this records it under the job lock. A lost job may have left the
topology partly changed. `up` is then permitted and repairs it as Section 7
defines. A command that ends because its context was cancelled records
`cancelled`; any other error records `failed`.

At the end the runner appends a final exit record to the output, then replaces
`status.json` atomically with the terminal state, and then deletes
`secrets.json`. The agent deletes `secrets.json` when it records `lost`.

`output.log` is a sequence of records. Each record has a length, a stream tag
(standard output or standard error), and at most 64 KiB of payload. Offsets are
record boundaries. A reader ignores an incomplete final record while the runner
is alive, and discards it when the runner is dead. The log is limited to 64 MiB.
After that limit the runner appends a truncation marker and stops recording
output; the job continues.

The agent session sends the output records to the controller from a given
offset, then sends the job's exit status as the final frame. The status is 0 for
`succeeded` and 1 for every other terminal state. When the controller loses
the connection it reconnects with a delay that starts at 1 second, doubles, and
never exceeds 15 seconds, until `--reconnect-timeout` has passed since the loss.
It resumes from the last record boundary that it received, so output is neither
lost nor repeated. When the time has passed, it prints the job identifier, the
destination, and the `jobs attach` command, and exits with status 3. A value of
`0` disables reconnection.

Interrupts on the controller act on a lifecycle command as follows:

- The first SIGINT sends a cancel request. The agent sends SIGTERM to the
  runner, which cancels the command exactly as an interrupt does in local mode.
  The controller stays attached and exits with the job's status.
- A second SIGINT detaches the controller. It exits with status 3.
- SIGTERM, SIGHUP, or SIGQUIT on the controller detaches it and exits with
  status 3. It never cancels the job.

The job commands are:

`jobs ls [--all]`
: List the jobs of the current topology, or of every topology in the working
  directory with `--all`. The columns are ID, TOPOLOGY, COMMAND, STATE,
  STARTED, FINISHED, and EXIT.

`jobs show ID`
: Print the contents of `status.json` as indented JSON. It never prints
  `secrets.json`.

`jobs attach ID`
: Replay the output from offset 0, follow it until the job ends, and exit with
  the job's status. A detach by SIGINT exits with status 3.

`jobs cancel ID`
: Send SIGTERM to the runner and wait until the job reaches a terminal state.

`jobs prune`
: Apply the retention rule now to the jobs of the current topology.

An unknown identifier fails with `job_not_found`. Retention applies when a job
is created and when `jobs prune` runs. The agent removes a finished job
directory only when its finish time is more than 7 days old and the job is
outside the 20 most recent finished jobs of its topology.

Neither a lost connection nor the end of an agent session or `ssh` process
stops, restarts, or changes a VM, router, helper, or network resource, other
than the changes that a running job makes itself. A job and the QEMU processes
that it starts also survive the end of the SSH session unless the host's login
manager kills the user's processes (Section 12.3).

### 12.7 Read-only, interactive, and capture commands

`ps`, `logs`, `counters`, and the `jobs ls`, `jobs show`, and `templates ls`
commands run synchronously in the agent session. The agent cancels them when
the connection ends. They change no state. `logs --follow` runs until the
controller is interrupted or the connection ends.

`exec` and `router exec` run in the agent session and hold the shared topology
lock for the lifetime of the child, as Section 3.1 defines.

- The child's identity is the agent's original caller: the `SUDO_UID` and
  `SUDO_GID` pair that `sudo` sets, or root when the agent runs directly as
  root. `--user` has its meaning in Section 3.1.
- The child's working directory is `--cwd` (a host path), otherwise the home
  directory of the child's identity, otherwise `/`. The controller's directory
  is never used.
- The only value that the controller sends to the child's environment is
  `TERM`, and only when the controller's standard input is a terminal. All
  other environment rules of Section 3.1 hold.
- When the controller's standard input and output are terminals, the agent
  allocates a pseudo-terminal and the child gets it as its controlling
  terminal, in its own session. The controller sets its own terminal to raw
  mode and restores it on every exit path. It forwards size changes, and the
  agent applies them to the pseudo-terminal. An interrupt character typed in the
  terminal reaches the child through the terminal driver. Standard error is
  merged into the terminal stream.
- Otherwise the child gets separate pipes, in its own process group. The
  controller forwards `SIGINT`, `SIGTERM`, `SIGHUP`, and `SIGQUIT` to that
  group, and the end of the controller's standard input closes the child's
  standard input. After the child exits, the agent stops copying output within
  250 ms, so that a background descendant that holds a pipe cannot block the
  command.
- The command returns the child's exact exit status, including 128 plus the
  signal number for a signalled child.
- When the connection ends, the agent sends `SIGHUP` to the child's process
  group, and `SIGKILL` after 5 seconds. It releases the shared lock when the
  child exits.

`capture` streams the pcapng data from the agent to the controller. The
controller creates the output file, and the agent does not write a capture file
on the host. The agent applies `--direction`, `--snaplen`, `--duration`, and
`--count`, and sends whole pcapng blocks only. A relative `--output` resolves
against the controller's current directory, and its parent directory must
already exist. When `--output` is omitted, the file is:

```text
./captures/<topology>/<link>-<UTC timestamp>.pcapng
```

relative to the controller's current directory, and the controller creates the
missing directories. The controller creates the file with exclusive creation and
without following symbolic links, and never overwrites an existing file. It
removes the file when no data arrived. On success it prints the controller path
that it wrote. An interrupt on the controller sends a cancel request. The
agent finishes the current block and ends the capture, and the command exits
with status 0 as Section 8.1 defines. When the connection ends instead, the file
holds complete blocks only, and the command exits with status 1.

### 12.8 Connection loss, exit statuses, and errors

The effect of a lost connection, or of a terminated controller, is:

| Command | Effect |
| --- | --- |
| `up`, `start`, `stop`, `restart`, `down` | The job continues. The controller reconnects (Section 12.6). |
| `ps`, `logs`, `counters`, `jobs ls`, `jobs show`, `templates ls` | The agent cancels the command. No state changed. |
| `jobs attach`, `jobs cancel` | The command ends. The job continues, or the cancel stays in effect. |
| `templates prune` | The agent stops after the current removal. Each removal is independent. |
| `exec`, `router exec` | The child group receives `SIGHUP`, then `SIGKILL` after 5 seconds. |
| `capture` | The agent ends the capture. The controller's file holds complete blocks only. |
| Template upload during `up` | The partial file stays. The next upload resumes from its size. |

For a command other than a lifecycle job, the first interrupt on the controller
sends a cancel request and the second ends the `ssh` process and exits with
status 1.

The controller exits with these statuses:

| Status | Meaning |
| --- | --- |
| 0 | Success. |
| 1 | The command or the remote operation failed. |
| 2 | Usage error. |
| 3 | A lifecycle job continues without the controller: the reconnection time passed, or the controller detached. |
| other | `exec` and `router exec` only: the child's exit status. |

When the connection ends without an exit frame, the status is 3 if a job
exists and 1 otherwise. The controller never reports the exit status of `ssh`.

Every error that the agent reports carries a stable lowercase code. The
controller prints it as `topolab: remote: <code>: <message>`. The codes are:

| Code | Meaning |
| --- | --- |
| `agent_missing` | The agent for the controller's digest is not installed. |
| `agent_mismatch` | The agent's digest or architecture differs from the controller's. |
| `privilege_denied` | The account cannot run the agent as root. |
| `protocol_error` | A malformed frame, a missing preamble, an unknown variable, a request digest conflict, or a configuration mismatch. |
| `request_too_large` | A request or a Root CA file exceeds its limit. |
| `working_dir_invalid` | The host rejects the working directory. |
| `job_active` | Another job of the topology is running. |
| `job_not_found` | The job identifier is unknown. |
| `blob_digest_mismatch` | An uploaded template does not match its digest. |
| `blob_invalid` | An uploaded template is not a qcow2 file without a backing file. |
| `blob_corrupt` | A stored blob does not match its name. |
| `blob_no_space` | The host cannot reserve space for an upload. |
| `host_template_invalid` | A template with `remote: true` is missing on the host, is not a regular file, is not readable, or is not a qcow2 image without a backing file. |

### 12.9 Security

The trust boundary is SSH. The controller uses the operator's `ssh` client and
its configuration, including host-key verification, and never disables or
weakens verification. The controller starts `ssh` through an argument vector,
and it validates the destination as Section 4.8 requires.

The agent accepts frames only from its standard input. It opens no network
socket. It runs only the operations that this specification defines; `exec`
and `router exec` keep their contracts in Section 3.1. It refuses to run unless
its effective UID is 0 and its standard input and output are not terminals.

The right to run the agent as root is equivalent to administrative control of
every topology and every file under any working directory that the operator
names. It also lets the account name any qcow2 file on the host as a template
with `remote: true`, and the VMs of that topology can read the file. The operator
grants it only to trusted accounts and limits it with the
`sudoers` rule of Section 12.3 and with SSH key policy.

Only the variables that a topology looks up leave the controller. They are not
logged. A job keeps them in `secrets.json` only while it runs. The blob and job
stores are root-only, as Section 10 requires. The controller writes only these
files: the `capture` output, the template digest cache under
`$XDG_CACHE_HOME/topolab` (default `~/.cache/topolab`, mode 0600), and its
temporary files while it runs.
