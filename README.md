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

Go 1.26 or newer is required to install TopoLab from source:

```sh
go install github.com/torob/topolab/cmd/topolab@latest
```

To build from a checkout:

```sh
go build -o bin/topolab ./cmd/topolab
```

## Host requirements

TopoLab supports Linux hosts with x86-64 hardware virtualization. Runtime
commands require effective UID 0 and read/write access to `/dev/kvm`. The
read-only `config` command does not require root.

The host must provide `qemu-system-x86_64`, `qemu-img`, `ip`, `bridge`, `tc`,
`nft`, and `xorriso`. QEMU nodes also require OVMF firmware at
`/usr/share/OVMF/OVMF_CODE_4M.fd` and
`/usr/share/OVMF/OVMF_VARS_4M.fd`. `dnsmasq` is required only when a router
serves DHCP. TopoLab does not install or configure these host
dependencies.

## Usage

Every active topology has an isolated fabric namespace and an always-present
management-host namespace. Switches, link devices, TAPs, captures, counters,
and QEMU VMs run in the fabric namespace; routers run in their own network
namespaces.

```sh
WORKING_DIR=/var/tmp/topolab topolab -f topology.yml config
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml up
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml ps
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml \
  exec -- ssh user@management-address
sudo WORKING_DIR=/var/tmp/topolab topolab -f topology.yml down
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
holds the lock and reaps the child.

The former top-level `host` key is intentionally unsupported. Rename it to
`management_host`; if the topology is active under the legacy root datapath,
run `down` with the new binary before the next `up`.

## Development

Run the local checks with:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Root-only network coverage is opt-in with `sudo make integration`. The full
synthetic QEMU/Internet workflow is `sudo make integration-routed`; it requires
the built Ubuntu image and the environment described below. Both harnesses use
temporary working directories, assert cleanup/ownership behavior, and retain
routed-lab captures when successful.

## Routed example

[`examples/routed/topology.yml`](examples/routed/topology.yml) is a deliberately
synthetic three-segment lab built from RFC 5737 documentation networks. An
application VM reaches a client segment and the Internet through a dual-homed
gateway VM and an external edge router. `gateway` is a normal VM, not a TopoLab
router. Run its explicit, idempotent post-`up` provisioner to enable guest
forwarding and its dedicated iptables chains. See
[`examples/routed/README.md`](examples/routed/README.md).

The opt-in Ubuntu 26.04 image build is documented in
[`hack/template/README.md`](hack/template/README.md).

## License

TopoLab is licensed under the [MIT License](LICENSE).
