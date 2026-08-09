# TopoLab Specification

## 1. Status and scope

This document specifies version 1 of `topolab`, a Linux-only tool for
creating declarative network labs on one local KVM host. The implementation is
written in Go and manages QEMU directly. It does not use libvirt.

The tool manages:

- QEMU/KVM virtual machines;
- network-namespace-based routers;
- isolated management-host attachments;
- switched and direct point-to-point links;
- Linux bridge access and trunk ports;
- IPv4 addressing, static routing, DHCPv4, and NAT;
- directional link latency, jitter, loss, and rate limits;
- host-side link packet capture and directional counters;
- read-only qcow2 templates with writable root overlays and additional disks;
- persisted, multiplexed QEMU serial logs.

The following features are outside version 1:

- IPv6;
- dynamic routing protocols;
- physical NIC bridging;
- QEMU live migration and live hardware reconfiguration other than online disk
  growth and VirtIO balloon-target changes;
- libvirt integration;
- a graphical interface; and
- automatic restoration through systemd after host reboot.

## 2. Host contract

The supported host is Ubuntu 26.04 on x86-64 with hardware virtualization and
an accessible `/dev/kvm`.

The following commands are required for networking operation:

- `ip`, `bridge`, and `tc` from iproute2;
- `nft`.

Topologies containing VMs additionally require `qemu-system-x86_64`,
`qemu-img`, `xorriso`, accessible KVM, and OVMF. Router-only topologies skip
these VM-only requirements.

`dnsmasq` is additionally required when a router declares a DHCP
server. QEMU VMs require `/usr/share/OVMF/OVMF_CODE_4M.fd` and
`/usr/share/OVMF/OVMF_VARS_4M.fd`.

The Linux vhost-net accelerator is optional. When `/dev/vhost-net` is
available and accessible, the tool uses it automatically for VirtIO VM
interfaces. Its absence must not prevent a VM from running.

`config` and `version` are read-only and may run as an ordinary user. All other
commands require the privileges described by their command contracts. The CLI
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

Global VM-helper options are `--qemu-system PATH`, `--qemu-img PATH`,
`--ovmf-code PATH`, `--ovmf-vars PATH`, and `--xorriso PATH`. Executable
overrides must resolve to executable regular files; firmware overrides must be
readable regular files. The selected helpers are resolved once and used by
preflight and all subsequent VM operations. Defaults retain PATH lookup for
executables and the standard OVMF paths from Section 2.

### 3.1 Commands

`config`
: Parse, default, validate, and print the normalized YAML. It does not inspect
  or modify runtime state.

`up`
: Validate prerequisites and templates, compare desired configuration with
  persisted state, remove obsolete resources, replace a root overlay only when
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

## 4. Configuration format

The input is one strict YAML document. Unknown fields and additional YAML
documents are errors. Environment interpolation is performed before path
resolution and validation. Relative filesystem paths in the document are then
resolved against the directory containing the topology file.

The top-level structure is:

```yaml
name: example
templates: []
management_host: {interfaces: []}
vms: []
routers: []
switches: []
links: []
```

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

Every file and directory created by `topolab` must be beneath
the selected working directory. This includes manifests, locks, writable disk
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

The process environment is the only interpolation-variable source. There are
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

Each `templates` entry contains a unique `name` and `path`. Relative
paths are resolved against the directory containing the topology file. Each
path must identify an existing regular file built separately from
`topolab`.

Every template must use the qcow2 format and must not itself have a backing
file. The tool validates the format before use. It opens the template as a
read-only base image and creates a separate writable qcow2 overlay for each
QEMU VM. The guest image must include the driver needed by the VM's selected
root-disk bus in its initramfs and must be able to boot its root filesystem
from that bus. Templates used with the default `scsi` bus must include the
VirtIO SCSI driver.

Templates may be inside or outside `working_dir`. Even when a template is
inside it, `topolab` never builds, modifies, or removes the template, and
`down` removes only paths recorded as tool-created resources. Template
contents are SHA-256 identified.

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
    additional_disks:
      - name: data
        size_gib: 100
        bus: scsi
    guest_agent: true
    boot:
      order: [disk, network]
      menu: false
      strict: false
    ssh_authorized_keys:
      - ssh-ed25519 AAAA... operator@example
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

`cpu` optionally controls the CPU model and topology exposed to the guest.
`cpu.model` defaults to `host`, which uses KVM host passthrough. It may instead
name an x86 CPU model reported by `qemu-system-x86_64 -cpu help`; raw QEMU CPU
feature suffixes such as `,vmx=off` are not accepted in this version. Before
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
this version.

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

VM interface and VLAN MTUs, layer-3 fields, and `ssh_authorized_keys` are
write-once bootstrap inputs, not continuously reconciled guest configuration.
They are applied only when the VM's root overlay is first created. The manifest
records their normalized values and a VM-bootstrap hash. While that root
overlay exists, changing any guest MTU, address, DHCP setting, VLAN, route, DNS
server, or authorized key in the topology file is a preflight error and no
runtime state may be changed. The fields remain in the topology file as the
historical bootstrap inputs; they do not describe or constrain the current
guest state.

After first boot, only an operator or guest configuration system such as
Ansible may change these settings inside the VM. `topolab` does not inspect,
overwrite, or report drift in them. Package installation, Kubernetes
configuration, services, firewall policy, and every other guest concern are
also outside this tool. When an interface has neither `ipv4` nor `vlans`, the
initial network configuration matches and names the device but does not
configure layer 3 on it. If a backing-template change already requires a new
root overlay, the current bootstrap inputs are applied to that new root and
become its new recorded baseline.

#### 4.4.2 VM storage

`root_disk.size_gib` is optional and sets the root disk's virtual capacity in
gibibytes. When omitted, the root disk uses the template's virtual capacity.
When specified, it must be greater than or equal to the template's virtual
capacity, rounded up to whole GiB. The tool reads the template capacity with
`qemu-img` during preflight and rejects an incompatible value before creating
or changing any runtime resource. The read-only template is never resized;
the configured capacity applies to the VM's writable root overlay.

`root_disk.bus` selects how QEMU presents the root disk to the guest. It
defaults to `scsi` and must be one of `scsi`, `virtio`, `sata`, `nvme`, or
`ide`.

`additional_disks` is an optional ordered list of additional writable data disks. Disk
names use the resource-name rules and must be unique within the VM. Each disk
requires a positive `size_gib`; its optional `bus` has the same values and
defaults as `root_disk.bus`. A newly declared disk is created as an empty
standalone qcow2 image beneath the VM's working directory. Additional disks
have deterministic QEMU identifiers and serial numbers. Their partitioning,
filesystems, mounting, and application use are guest responsibilities.

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

All configured disks support increase-only resizing. Before applying any
change, the tool must read the actual virtual capacity of the root overlay and
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
impairment. A router may declare at most one external interface in this
version.

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
is not allowed on a VLAN subinterface in this version. The router receives the
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
logical interface. A route through a DHCP-only layer is therefore invalid in
this version. Route destinations must be unique within a router. Connected
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

The root disk is a qcow2 overlay whose backing file is the template image. It
is writable and may have a larger virtual capacity than its backing template.
Each additional disk is an independent writable qcow2 image without a backing
file. Templates are not chmod-modified or opened as writable disks. Running
disk growth uses QMP `block_resize`; `qemu-img resize` must not be invoked on
an image currently opened by a running QEMU process.

The generated NoCloud seed contains `meta-data`, `user-data`, and
`network-config`. For a VM, the seed contains only its hostname, locked-
password `torob` user, initial authorized keys, and initial interface names,
MTUs, VLANs, IPv4 configuration, routes, and DNS resolvers. Its instance ID
incorporates the topology name, node name, storage-identity hash, and recorded
VM-bootstrap hash. The seed and instance ID remain unchanged for the lifetime
of that root overlay. The manifest retains the normalized bootstrap values and
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
network rendering.

VM shutdown first requests `system_powerdown` over QMP, waits up to 30
seconds, then uses bounded TERM and KILL fallbacks. Before signaling, the tool
verifies that the PID command line identifies the expected topology and node.

## 7. State and reconciliation

All persistent and volatile state is stored beneath the resolved
`working_dir`. Tool-created paths use this layout:

```text
<working_dir>/.topolab/topologies/<name>/
<working_dir>/.topolab/shared/
<working_dir>/captures/<name>/
```

The topology directory contains its manifest, node files, logs, sockets, and
other topology-specific state. The shared directory contains coordination
state such as host IPv4-forwarding ownership. The captures directory contains
diagnostic artifacts and is intentionally not topology state; reconciliation
does not read it and `down` does not remove it. User-managed content elsewhere
beneath `working_dir`, including `templates/`, is not topology state. No
runtime file is created under a fixed `/var/lib` or `/run` location.

Only one operation may mutate a topology at a time; mutations take an
exclusive file lock. Both management-host and router `exec` hold a shared lock
for the full child lifetime.

The manifest records a datapath version; deterministic fabric and management-
host namespace names and ownership aliases; namespace scope for every managed
bridge, TAP, link, nftables table, counter, and ownership operation; distinct
management-host, router, fabric, and root nftables state; and QEMU namespace
identity. It distinguishes root-external resources from fabric resources. It
also records configuration and host-network hashes, each VM's
normalized bootstrap values and VM-bootstrap hash, template digests, VM
storage-identity and restart hashes, every writable disk's path and observed
virtual capacity, restart-only hardware settings, requested balloon targets,
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
4. Stop VMs affected by a network rebuild, backing-template change, or
   restart-only QEMU hardware-presentation change.
5. Apply only the explicit disk deletions and root-overlay replacements defined
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

A VM's storage-identity hash contains only the template path and
template-content digest that define the backing file of its root overlay. A
change to either value replaces that root overlay because an existing overlay
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
the root overlay and every additional disk of every surviving VM.

An attempted change to a VM's write-once guest MTU, address, DHCP, VLAN, route,
DNS, or authorized-key bootstrap inputs follows the preflight rejection rule
in Section 4.4.1. Rejection occurs before a VM is stopped or any host
networking or disk is changed.

A network change may require an affected VM to stop and start when its
presented NIC hardware or safe host-datapath reconciliation requires it. Such
a stop is a disk-preserving restart, never node recreation. CPU
model/topology, `memory_mb`, balloon-device presence, guest-agent presence,
boot settings, additional-disk membership, desired disk capacities, disk
declaration order, disk bus settings, and VM interface model/queue settings
are also reconciled outside the storage-identity hash. Changing any of those
fields except only a disk increase or balloon `target_mb` may require a VM
restart, but must not recreate the root overlay or any unchanged additional
disk. Disk increases and balloon-target changes use their online procedures
when the VM is running.

An unchanged root overlay and unchanged additional disks survive `stop`,
`start`, `restart`, every network change, and host reboot. After reboot the
operator reruns `up` to restore Linux networking and QEMU processes. A root
overlay is deleted only when its VM is removed, its backing-template
identity changes, or `down` is executed. An additional disk is deleted only
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
topology state: `up` does not read them and `down` does not remove them.

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
There is no counter-reset operation in this version. Automated tests obtain
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
root overlay is replaced after a backing-template identity change, or `down`
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
- VM bootstrap values are applied initially and write-once changes are
  rejected without stopping the VM or changing a disk;
- all documented network-only changes preserve the root overlay and every
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
