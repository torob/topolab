package network

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	fdbVMMAC   = "02:00:00:00:00:01"
	fdbHostMAC = "02:00:00:00:00:02"
)

// applyFDB applies a topology on a fake runner and returns the FDB commands.
func applyFDB(t *testing.T, cfg Config) []string {
	t.Helper()
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		call := program + " " + strings.Join(args, " ")
		if call == "ip -j -4 route show table all type unicast" {
			return []byte(`[]`), nil
		}
		if strings.Contains(call, " -j -d link show dev ") {
			return nil, &CommandError{Program: "ip", Output: "Device does not exist", Err: errors.New("exit status 1")}
		}
		if strings.HasPrefix(call, "nft -j list table") || strings.Contains(call, " nft -j list table") {
			return nil, &CommandError{Program: "nft", Output: "Error: No such file or directory", Err: errors.New("exit status 1")}
		}
		return nil, nil
	}}
	m := New(fake, Options{GenerationID: func() (string, error) { return "generation-1", nil }})
	p, err := m.Plan(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	var fdb []string
	for _, call := range fake.calls {
		if strings.Contains(call, " bridge fdb ") {
			fdb = append(fdb, call)
		}
	}
	return fdb
}

func fdbVMLink(sw Endpoint, vm Endpoint) Link {
	return Link{Name: "vm-lan", MTU: 1500, Endpoints: [2]Endpoint{vm, sw}}
}

func fdbVM(mac string) Endpoint {
	return Endpoint{Node: "igw", Interface: "eth0", Kind: EndpointVM, MTU: 1500, MAC: mac, Queues: 1}
}

func fdbSwitchEndpoint(filtering bool, port Port) Endpoint {
	return Endpoint{Node: "public-lan", Interface: "p1", Kind: EndpointSwitch, MTU: 1500, Port: port, VLANFiltering: filtering}
}

func requireFDBSuffix(t *testing.T, calls []string, want ...string) {
	t.Helper()
	if len(calls) != len(want) {
		t.Fatalf("got %d fdb calls, want %d:\n%s", len(calls), len(want), strings.Join(calls, "\n"))
	}
	for i := range want {
		if !strings.HasSuffix(calls[i], want[i]) {
			t.Errorf("fdb call %d = %q, want suffix %q", i, calls[i], want[i])
		}
	}
}

func TestSwitchLinkPinsVMMACWithVLANOnFilteringSwitch(t *testing.T) {
	port := DeviceName("tlw", "lab", "vm-lan", "1")
	calls := applyFDB(t, Config{
		Topology: "lab",
		Switches: []Switch{{Name: "public-lan", MTU: 1500, VLANFiltering: true}},
		Links:    []Link{fdbVMLink(fdbSwitchEndpoint(true, Port{Mode: "access", VLAN: 10}), fdbVM(fdbVMMAC))},
	})
	requireFDBSuffix(t, calls, "bridge fdb replace "+fdbVMMAC+" dev "+port+" master static vlan 10")
}

func TestSwitchLinkPinsVMMACWithoutVLANOnPlainSwitch(t *testing.T) {
	port := DeviceName("tlw", "lab", "vm-lan", "1")
	calls := applyFDB(t, Config{
		Topology: "lab",
		Switches: []Switch{{Name: "public-lan", MTU: 1500}},
		Links:    []Link{fdbVMLink(fdbSwitchEndpoint(false, Port{Mode: "access", VLAN: 1}), fdbVM(fdbVMMAC))},
	})
	requireFDBSuffix(t, calls, "bridge fdb replace "+fdbVMMAC+" dev "+port+" master static")
}

func TestSwitchLinkPinsVMMACOnceForEachTrunkVLAN(t *testing.T) {
	port := DeviceName("tlw", "lab", "vm-lan", "1")
	calls := applyFDB(t, Config{
		Topology: "lab",
		Switches: []Switch{{Name: "public-lan", MTU: 1500, VLANFiltering: true}},
		Links:    []Link{fdbVMLink(fdbSwitchEndpoint(true, Port{Mode: "trunk", AllowedVLANs: []int{30, 20, 30}, NativeVLAN: 20}), fdbVM(fdbVMMAC))},
	})
	requireFDBSuffix(t, calls,
		"bridge fdb replace "+fdbVMMAC+" dev "+port+" master static vlan 20",
		"bridge fdb replace "+fdbVMMAC+" dev "+port+" master static vlan 30",
	)
}

func TestSwitchLinkPinsManagementHostMAC(t *testing.T) {
	host := Endpoint{Node: "management_host", Interface: "management", Kind: EndpointManagementHost, MTU: 1500, MAC: fdbHostMAC, Addresses: []string{"192.0.2.1/24"}}
	port := DeviceName("tlw", "lab", "host-lan", "1")
	link := Link{Name: "host-lan", MTU: 1500, Endpoints: [2]Endpoint{host, fdbSwitchEndpoint(false, Port{Mode: "access", VLAN: 1})}}
	calls := applyFDB(t, Config{Topology: "lab", Switches: []Switch{{Name: "public-lan", MTU: 1500}}, Links: []Link{link}})
	requireFDBSuffix(t, calls, "bridge fdb replace "+fdbHostMAC+" dev "+port+" master static")
}

func TestSwitchLinkWithoutKnownPeerMACAddsNoFDBEntry(t *testing.T) {
	cases := map[string]Link{
		"peer without MAC": fdbVMLink(fdbSwitchEndpoint(false, Port{Mode: "access", VLAN: 1}), fdbVM("")),
		"switch to switch": {Name: "trunk", MTU: 1500, Endpoints: [2]Endpoint{
			{Node: "core", Interface: "p1", Kind: EndpointSwitch, MTU: 1500, Port: Port{Mode: "access", VLAN: 1}},
			fdbSwitchEndpoint(false, Port{Mode: "access", VLAN: 1}),
		}},
	}
	for name, link := range cases {
		t.Run(name, func(t *testing.T) {
			calls := applyFDB(t, Config{
				Topology: "lab",
				Switches: []Switch{{Name: "public-lan", MTU: 1500}, {Name: "core", MTU: 1500}},
				Links:    []Link{link},
			})
			if len(calls) != 0 {
				t.Fatalf("unexpected fdb calls:\n%s", strings.Join(calls, "\n"))
			}
		})
	}
}

func TestSwitchLinkFDBUsesReplaceSoUpIsRepeatable(t *testing.T) {
	calls := applyFDB(t, Config{
		Topology: "lab",
		Switches: []Switch{{Name: "public-lan", MTU: 1500}},
		Links:    []Link{fdbVMLink(fdbSwitchEndpoint(false, Port{Mode: "access", VLAN: 1}), fdbVM(fdbVMMAC))},
	})
	if len(calls) == 0 {
		t.Fatal("no fdb calls")
	}
	for _, call := range calls {
		if !strings.Contains(call, " bridge fdb replace ") {
			t.Errorf("fdb call is not idempotent: %s", call)
		}
	}
}
