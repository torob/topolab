package app

import (
	"strings"
	"testing"

	"github.com/torob/topolab/internal/config"
)

func TestBootDNSWarnings(t *testing.T) {
	static := func(dns ...string) *config.VMIPv4 {
		return &config.VMIPv4{Addresses: []string{"10.0.0.2/24"}, DNSServers: dns}
	}
	vm := func(name string, interfaces ...*config.VMInterface) *config.VM {
		return &config.VM{Name: name, Interfaces: interfaces}
	}
	cfg := &config.Config{Name: "lab", VMs: []*config.VM{
		vm("plain", &config.VMInterface{Name: "eth0", IPv4: static()}),
		vm("resolver", &config.VMInterface{Name: "eth0", IPv4: static("10.0.0.1")}),
		vm("optional", &config.VMInterface{Name: "eth0", Optional: true, IPv4: static()}),
		vm("mixed", &config.VMInterface{Name: "eth0", Optional: true, IPv4: static()}, &config.VMInterface{Name: "eth1", IPv4: static()}),
		vm("vlan", &config.VMInterface{Name: "trunk0", IPv4: &config.VMIPv4{}, VLANs: []*config.VMVLAN{{ID: 10, IPv4: static()}}}),
	}}
	warnings := bootDNSWarnings(cfg)
	if len(warnings) != 3 {
		t.Fatalf("warnings = %q", warnings)
	}
	for i, name := range []string{"plain", "mixed", "vlan"} {
		if !strings.HasPrefix(warnings[i], "VM "+name+" has no DNS server on an interface that its boot waits for;") || !strings.Contains(warnings[i], "about 2 minutes") {
			t.Errorf("warning %d = %q", i, warnings[i])
		}
	}
	if !strings.HasSuffix(warnings[0], "add dns_servers to one of its interfaces, or set optional: true on each interface without dns_servers") {
		t.Errorf("interface fix = %q", warnings[0])
	}
	// A VLAN cannot be optional, so the warning offers only a DNS server.
	if !strings.HasSuffix(warnings[2], "add dns_servers to one of its interfaces or VLANs") || strings.Contains(warnings[2], "optional") {
		t.Errorf("VLAN fix = %q", warnings[2])
	}
}
