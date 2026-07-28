# Synthetic routed lab

This example is an intentionally synthetic three-segment topology built from
RFC 5737 documentation networks. `gateway` is a normal dual-homed VM, not a
TopoLab router. TopoLab bootstraps its two static addresses but does not manage
routing or firewall state inside it.

```sh
ssh-keygen -q -t ed25519 -N '' -f /tmp/topolab-routed-key
export ROUTED_SSH_PUBLIC_KEY="$(cat /tmp/topolab-routed-key.pub)"
export TOPOLAB_TEMPLATE=/path/to/ubuntu-26.04-topolab.qcow2
sudo --preserve-env=ROUTED_SSH_PUBLIC_KEY,TOPOLAB_TEMPLATE \
  topolab --working-dir /var/tmp/topolab -f examples/routed/topology.yml up
sudo topolab --working-dir /var/tmp/topolab -f examples/routed/topology.yml \
  exec -- ssh -i /tmp/topolab-routed-key torob@192.0.2.10
sudo topolab --working-dir /var/tmp/topolab -f examples/routed/topology.yml \
  exec -- examples/routed/provision-gateway.sh /tmp/topolab-routed-key
sudo topolab --working-dir /var/tmp/topolab -f examples/routed/topology.yml \
  exec -- examples/routed/provision-gateway.sh /tmp/topolab-routed-key # idempotency
sudo ROUTED_SSH_PRIVATE_KEY=/tmp/topolab-routed-key \
  topolab --working-dir /var/tmp/topolab -f examples/routed/topology.yml \
  exec -- ansible-playbook -i examples/routed/ansible/inventory.yml \
  examples/routed/ansible/reachability.yml
sudo topolab --working-dir /var/tmp/topolab -f examples/routed/topology.yml \
  exec -- examples/routed/verify.sh /tmp/topolab-routed-key
```

Remove the private key from `client` if an acceptance harness copied it there,
then run `down`. The provisioner owns only the
`TOPOLAB_EXAMPLE_FORWARD`/`TOPOLAB_EXAMPLE_NAT` chains, one sysctl file, one
script, and one systemd unit inside the gateway.

Service-network Internet traffic is intentionally translated twice: `gateway`
masquerades `192.0.2.0/24` to its transit address, then the edge external
attachment translates that transit source onto the host uplink. Client traffic
routed to the service network is excluded from gateway masquerading, so
`application` observes the original `203.0.113.10` source.

All controller traffic above originates in the topology's management-host
namespace at `192.0.2.254`; no root interface or route is created for that
prefix. The inventory exists only for this synthetic example.
