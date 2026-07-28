# Ubuntu 26.04 TopoLab template

`build.sh` pins `ubuntu-vm-template-builder` v1.3.0 and the final Ubuntu
Server 26.04 ISO. It installs the builder beneath
`$HOME/.tools/ubuntu-vm-template-builder/1.3.0`, caches the verified ISO, and
uses only upstream Ubuntu and GitHub sources.

The output is a 20 GiB, UEFI-bootable standalone qcow2. It keeps NoCloud and
cloud-init networking enabled and contains OpenSSH, curl, CA certificates,
iptables/nftables, iproute2 diagnostics, ping/traceroute, qemu-guest-agent,
and dnsmasq. Machine identity, cloud-init instance state, and SSH host keys are
removed before shutdown so each TopoLab overlay initializes independently.
The autoinstall also overrides the builder's `91-topolab-network.cfg` with a
later `network: null` setting; Ubuntu 26.04 can then select each overlay's
NoCloud `network-config` instead of treating `config: enabled` as a network
document.

Building is opt-in and needs KVM, QEMU, OVMF, xorriso, several GiB of free
space, and Internet access:

```sh
hack/template/build.sh /var/lib/topolab-images/ubuntu-26.04-topolab.qcow2
```

The helper builds beside the requested output, checks the image with
`qemu-img`, verifies that it has no backing file and exactly 20 GiB virtual
capacity, then boots a disposable overlay with TopoLab's q35/UEFI,
VirtIO-SCSI, and NoCloud presentation. The smoke verifies interface naming,
cloud-init, fresh machine/SSH identities, the required packages, and a clean
guest shutdown before the image is published atomically. Existing output is
never overwritten.
