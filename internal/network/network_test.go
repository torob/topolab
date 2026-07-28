package network

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	calls []string
	run   func(program string, args []string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, program string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, program+" "+strings.Join(args, " "))
	if f.run != nil {
		return f.run(program, args)
	}
	return nil, nil
}

func (f *fakeRunner) contains(fragment string) bool {
	return slices.ContainsFunc(f.calls, func(call string) bool { return strings.Contains(call, fragment) })
}

func TestCommandReportsNotFoundRejectsMissingExecutable(t *testing.T) {
	missingTool := &CommandError{Program: "ip", Err: &exec.Error{Name: "ip", Err: exec.ErrNotFound}}
	if commandReportsNotFound(missingTool) {
		t.Fatal("missing ip executable was treated as an absent network resource")
	}
	missingDevice := &CommandError{Program: "ip", Output: `Cannot find device "tl0"`, Err: errors.New("exit status 1")}
	if !commandReportsNotFound(missingDevice) {
		t.Fatal("missing device was not recognized")
	}
}

func TestDeviceNameDeterministicAndBounded(t *testing.T) {
	a := DeviceName("toolong", "topology", "node", "interface")
	b := DeviceName("toolong", "topology", "node", "interface")
	c := DeviceName("toolong", "topology", "node", "other")
	if a != b {
		t.Fatalf("names differ: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("different resources collided: %q", a)
	}
	if len(a) != 15 {
		t.Fatalf("name has length %d: %q", len(a), a)
	}
}

func TestPlanResolvesExternalAndAllocatesTransit(t *testing.T) {
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		call := program + " " + strings.Join(args, " ")
		switch call {
		case "ip -j -4 route show table all type unicast":
			return []byte(`[{"dst":"10.0.0.0/24"},{"dst":"default"}]`), nil
		case "ip -j -4 route get 1.1.1.1":
			return []byte(`[{"dev":"eth0","prefsrc":"203.0.113.9"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command %s", call)
		}
	}}
	m := New(fake, Options{})
	p, err := m.Plan(context.Background(), Config{Topology: "lab", Externals: []External{{Router: "edge", Interface: "wan", MTU: 1500, EgressInterface: "auto", SourceIP: "auto", Sources: []string{"10.10.0.0/24"}}}})
	if err != nil {
		t.Fatal(err)
	}
	got := p.External[0]
	if got.EgressInterface != "eth0" || got.SourceIP != "203.0.113.9" || !got.Masquerade {
		t.Fatalf("unexpected resolution: %#v", got)
	}
	if got.TransitPrefix == "" || got.RouterAddress == "" || got.HostAddress == "" {
		t.Fatalf("transit was not allocated: %#v", got)
	}
	if got.TransitPrefix[:7] != "198.18." && got.TransitPrefix[:7] != "198.19." {
		t.Fatalf("transit outside benchmark pool: %s", got.TransitPrefix)
	}
}

func TestApplyBuildsShimsVLANImpairmentAndIsolation(t *testing.T) {
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
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	m := New(fake, Options{Now: func() time.Time { return now }, GenerationID: func() (string, error) { return "generation-1", nil }})
	p, err := m.Plan(context.Background(), Config{Topology: "lab", Switches: []Switch{{Name: "lan", MTU: 1400, VLANFiltering: true}}, Links: []Link{{Name: "management", MTU: 1300, Endpoints: [2]Endpoint{
		{Node: "management_host", Interface: "management", Kind: EndpointManagementHost, MTU: 1500, Addresses: []string{"192.0.2.1/24"}, Egress: Impairment{LatencyMS: 5, LossPercent: 1, RateMbps: 100}},
		{Node: "lan", Interface: "host", Kind: EndpointSwitch, MTU: 1400, Port: Port{Mode: "access", VLAN: 10}},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := m.Apply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if state.Links["management"].GenerationID != "generation-1" {
		t.Fatalf("bad link state: %#v", state.Links)
	}
	if state.ManagementHostInterfaces["management"].DeviceName == "" {
		t.Fatal("management-host interface was not recorded")
	}
	for _, fragment := range []string{
		"type bridge",
		"type veth peer name",
		"ip netns exec " + state.ManagementHostNamespace + " sysctl -q -w net.ipv4.ip_forward=0",
		"tc qdisc replace",
		"htb rate 100mbit",
		"handle 10: netem loss 1% limit 4294967295",
		"parent 10:1 handle 20: netem delay 5ms",
		"bridge vlan add dev",
		"vid 10 pvid untagged",
		"nft add rule inet",
		"iifname " + state.ManagementHostInterfaces["management"].DeviceName + " counter drop",
		"oifname " + state.ManagementHostInterfaces["management"].DeviceName + " counter drop",
	} {
		if !fake.contains(fragment) {
			t.Errorf("missing command containing %q\n%s", fragment, strings.Join(fake.calls, "\n"))
		}
	}
	if len(state.Resources) < 7 {
		t.Fatalf("too few ownership records: %#v", state.Resources)
	}
	if state.DatapathVersion != DatapathVersion || state.FabricNamespace == "" || state.ManagementHostNamespace == "" || state.FabricNamespace == state.ManagementHostNamespace {
		t.Fatalf("namespace ownership state is incomplete: %#v", state)
	}
	for _, resource := range state.Resources {
		if resource.Kind == ResourceNamespace {
			continue
		}
		if resource.Kind == ResourceNFTTable && resource.Namespace == state.ManagementHostNamespace {
			continue
		}
		if resource.Namespace != state.FabricNamespace {
			t.Errorf("internal resource is not fabric scoped: %#v", resource)
		}
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "ip link add") || strings.HasPrefix(call, "nft add") {
			t.Errorf("internal datapath mutated root namespace: %s", call)
		}
	}
}

func TestPlanDoesNotInspectRootRoutesForManagementHost(t *testing.T) {
	topology := "lab"
	cfg := Config{Topology: topology, Switches: []Switch{{Name: "switch", MTU: 1500}}, Links: []Link{{Name: "management", MTU: 1500, Endpoints: [2]Endpoint{
		{Node: "management_host", Interface: "management", Kind: EndpointManagementHost, MTU: 1500, Addresses: []string{"192.0.2.1/24"}},
		{Node: "switch", Interface: "host", Kind: EndpointSwitch, MTU: 1500, Port: Port{Mode: "access", VLAN: 1}},
	}}}}
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		return nil, fmt.Errorf("unexpected root command %s %v", program, args)
	}}
	if _, err := New(fake, Options{}).Plan(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestPlanIgnoresOnlyExactPersistedExternalRoutes(t *testing.T) {
	device := DeviceName("tlh", "lab", "edge", "wan", "external")
	base := Config{Topology: "lab", Externals: []External{{Router: "edge", Interface: "wan", MTU: 1500, EgressInterface: "eth0", SourceIP: "auto", Sources: []string{"10.10.0.0/24"}, TransitPrefix: "198.18.1.0/30", RouterAddress: "198.18.1.1", HostAddress: "198.18.1.2"}}, ExistingOwnedRoutes: []OwnedRoute{{Prefix: "198.18.1.0/30", Device: device, Protocol: "kernel", Marker: "external-alias"}, {Prefix: "10.10.0.0/24", Device: device, Via: "198.18.1.1", Protocol: "static", Marker: "external-alias"}}}
	for _, tc := range []struct {
		name, via string
		wantErr   bool
	}{{"exact", "198.18.1.1", false}, {"wrong gateway", "198.18.1.9", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
				call := program + " " + strings.Join(args, " ")
				switch call {
				case "ip -j -4 route show table all type unicast":
					return []byte(fmt.Sprintf(`[{"dst":"198.18.1.0/30","dev":%q,"protocol":"kernel"},{"dst":"10.10.0.0/24","dev":%q,"gateway":%q,"protocol":"static"}]`, device, device, tc.via)), nil
				case "ip -j -4 route get 1.1.1.1":
					return []byte(`[{"dev":"eth0","prefsrc":"203.0.113.9"}]`), nil
				case "ip -j -d link show dev " + device:
					return []byte(`[{"ifalias":"external-alias"}]`), nil
				default:
					return nil, fmt.Errorf("unexpected %s", call)
				}
			}}
			_, err := New(fake, Options{}).Plan(context.Background(), base)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%t", err, tc.wantErr)
			}
		})
	}
}

func TestExternalRouterExplicitlyEnablesForwarding(t *testing.T) {
	fake := &fakeRunner{}
	m := New(fake, Options{})
	err := m.configureRouter(context.Background(), "lab", Router{Name: "edge", Forwarding: true, Interfaces: []RouterInterface{{Name: "lan", MTU: 1500, MAC: "02:00:00:00:00:01"}}}, "ns-edge")
	if err != nil {
		t.Fatal(err)
	}
	if !fake.contains("ip netns exec ns-edge sysctl -q -w net.ipv4.ip_forward=1") {
		t.Fatalf("forwarding not enabled:\n%s", strings.Join(fake.calls, "\n"))
	}
}

func TestExternalRouterAttachmentCreatesVethWithoutTAP(t *testing.T) {
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		if program == "ip" && strings.Contains(strings.Join(args, " "), "-j -d link show dev") {
			return nil, &CommandError{Program: "ip", Output: "Device does not exist", Err: errors.New("exit status 1")}
		}
		return nil, nil
	}}
	m := New(fake, Options{})
	ext := ResolvedExternal{External: External{
		Router: "edge", Interface: "wan", MAC: "02:00:00:00:00:01", MTU: 1500,
		Sources: []string{"10.10.0.0/24"}, TransitPrefix: "198.18.1.0/30",
		RouterAddress: "198.18.1.1", HostAddress: "198.18.1.2",
	}}
	state := &State{Namespaces: map[string]string{"edge": "ns-edge"}, TAPs: map[string]TAPState{}}
	if err := m.applyExternal(context.Background(), "lab", ext, state); err != nil {
		t.Fatal(err)
	}
	if !fake.contains("type veth peer name") {
		t.Fatalf("external router did not create a veth:\n%s", strings.Join(fake.calls, "\n"))
	}
	if !fake.contains("ip -n ns-edge link set dev wan address 02:00:00:00:00:01") {
		t.Fatalf("external router MAC was not configured:\n%s", strings.Join(fake.calls, "\n"))
	}
	if fake.contains("tuntap add") || fake.contains("type bridge") || len(state.TAPs) != 0 {
		t.Fatalf("external router created QEMU datapath state: calls=%v taps=%v", fake.calls, state.TAPs)
	}
}

func TestHostNFTScopesExternalForwardingAndReturnTraffic(t *testing.T) {
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		if program == "nft" && len(args) > 2 && args[0] == "-j" && args[1] == "list" && args[2] == "table" {
			return nil, &CommandError{Program: "nft", Output: "Error: No such file or directory", Err: errors.New("exit status 1")}
		}
		if program == "nft" && slices.Equal(args, []string{"-j", "list", "ruleset"}) {
			return []byte(`{"nftables":[]}`), nil
		}
		return nil, nil
	}}
	m := New(fake, Options{})
	ext := ResolvedExternal{External: External{Router: "edge", Interface: "wan", EgressInterface: "eth0", Sources: []string{"10.10.0.0/24"}}, Masquerade: true}
	if err := m.applyHostNFT(context.Background(), &State{Topology: "lab", External: []ResolvedExternal{ext}}); err != nil {
		t.Fatal(err)
	}
	peer := externalForwardDevice("lab", ext.External)
	for _, fragment := range []string{"iifname " + peer + " oifname eth0 ip saddr 10.10.0.0/24 ct state new,established,related counter accept", "iifname eth0 oifname " + peer + " ip daddr 10.10.0.0/24 ct state established,related counter accept", "iifname " + peer + " oifname eth0 counter drop", "iifname eth0 oifname " + peer + " counter drop"} {
		if !fake.contains(fragment) {
			t.Errorf("missing %q\n%s", fragment, strings.Join(fake.calls, "\n"))
		}
	}
}

func TestHostNFTInsertsScopedAcceptsIntoExistingForwardHook(t *testing.T) {
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		call := program + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(call, "nft -j list table inet"):
			return nil, &CommandError{Program: "nft", Output: "Error: No such file or directory", Err: errors.New("exit status 1")}
		case call == "nft -j list ruleset":
			return []byte(`{"nftables":[{"chain":{"family":"ip","table":"filter","name":"FORWARD","hook":"forward","prio":0,"policy":"drop"}}]}`), nil
		case call == "nft -j list chain ip filter FORWARD":
			return []byte(`{"nftables":[{"chain":{"family":"ip","table":"filter","name":"FORWARD","hook":"forward"}}]}`), nil
		default:
			return nil, nil
		}
	}}
	state := &State{Topology: "lab", External: []ResolvedExternal{{External: External{Router: "edge", Interface: "wan", EgressInterface: "eth0", Sources: []string{"10.10.0.0/24"}}, Masquerade: true}}}
	if err := New(fake, Options{}).applyHostNFT(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if len(state.FirewallRules) != 2 {
		t.Fatalf("firewall rules = %#v", state.FirewallRules)
	}
	for _, fragment := range []string{
		"nft insert rule ip filter FORWARD iifname " + externalForwardDevice("lab", state.External[0].External) + " oifname eth0 ip saddr 10.10.0.0/24",
		"nft insert rule ip filter FORWARD iifname eth0 oifname " + externalForwardDevice("lab", state.External[0].External) + " ip daddr 10.10.0.0/24",
	} {
		if !fake.contains(fragment) {
			t.Errorf("missing %q\n%s", fragment, strings.Join(fake.calls, "\n"))
		}
	}
}

func TestRemoveRejectsWrongOwnershipBeforeDeleting(t *testing.T) {
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		name := args[len(args)-1]
		alias := "owner-a"
		if name == "second" {
			alias = "someone-else"
		}
		return []byte(fmt.Sprintf(`[{"ifalias":%q}]`, alias)), nil
	}}
	m := New(fake, Options{})
	err := m.Remove(context.Background(), &State{Resources: []Resource{{Kind: ResourceBridge, Name: "first", Alias: "owner-a"}, {Kind: ResourceBridge, Name: "second", Alias: "owner-b"}}})
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected ownership error, got %v", err)
	}
	if fake.contains("link del") {
		t.Fatalf("cleanup mutated before complete verification: %v", fake.calls)
	}
}

func TestCountersExposeDirectionalAndTCStatistics(t *testing.T) {
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		call := program + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(call, "ip -j -s link show dev ingress"):
			return []byte(`[{"stats64":{"rx":{"bytes":1000,"packets":10,"errors":1,"dropped":2},"tx":{"bytes":0,"packets":0,"errors":0,"dropped":0}}}]`), nil
		case strings.HasPrefix(call, "ip -j -s link show dev egress"):
			return []byte(`[{"stats64":{"rx":{"bytes":800,"packets":8,"errors":0,"dropped":0},"tx":{"bytes":0,"packets":0,"errors":0,"dropped":0}}}]`), nil
		case strings.HasPrefix(call, "ip -j -s link show dev impair"):
			return []byte(`[{"stats64":{"rx":{"bytes":3,"packets":1,"errors":4,"dropped":5},"tx":{"bytes":1000,"packets":10,"errors":6,"dropped":7}}}]`), nil
		case strings.HasPrefix(call, "tc -j -s qdisc"):
			return []byte(`[{"kind":"htb","drops":9,"overlimits":3,"requeues":4},{"kind":"netem","handle":"10:","drops":3,"dropped":2,"options":{"loss":100}},{"kind":"netem","handle":"20:","drops":1,"backlog":12,"qlen":1,"options":{"delay":5000}}]`), nil
		case strings.HasPrefix(call, "tc -j -s class"):
			return []byte(`[{"kind":"htb","drops":2,"overlimits":3,"requeues":4,"backlog":12,"qlen":1}]`), nil
		case strings.HasPrefix(call, "tc -j -s filter"):
			return []byte(`[{"kind":"matchall","options":{"actions":[{"kind":"gact","stats":{"bytes":1000,"packets":10}}]}}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command %s", call)
		}
	}}
	m := New(fake, Options{Now: func() time.Time { return time.Unix(100, 0) }})
	state := &State{Topology: "lab", Links: map[string]LinkState{"l": {Name: "l", GenerationID: "g", GenerationStartedAt: time.Unix(1, 0), Endpoints: []EndpointState{{Node: "a", Interface: "eth0", IngressDevice: "ingress", EgressDevice: "egress", ImpairmentDevice: "impair"}}}}}
	snapshot, err := m.Counters(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	ep := snapshot.Links[0].Endpoints[0]
	if *ep.Ingress.Packets != 10 || *ep.Egress.Packets != 8 || *ep.Impairment.OfferedPackets != 10 || *ep.Impairment.DeliveredPackets != 8 {
		t.Fatalf("bad directional counters: %#v", ep)
	}
	if *ep.Impairment.ConfiguredLossDrops != 2 || *ep.Impairment.NetemQueueDrops != 1 || *ep.Impairment.RateLimiterOverlimits != 3 {
		t.Fatalf("bad tc counters: %#v", ep.Impairment)
	}
	if ep.Impairment.RateLimiterDrops != nil {
		t.Fatal("ambiguous HTB generic drops must remain unavailable")
	}
	if ep.Device.MTUDrops != nil {
		t.Fatal("unavailable MTU drops must remain null")
	}
}

func TestFillTCFallsBackToGenericLossDropsAndSubtractsChildQueueDrops(t *testing.T) {
	loss, queue := uint64(11), uint64(3)
	var got ImpairmentCounters
	fillTC(&got, []tcStats{
		{Kind: "netem", Handle: "10:", Drops: &loss},
		{Kind: "netem", Handle: "20:", Drops: &queue},
	}, nil)
	if got.ConfiguredLossDrops == nil || *got.ConfiguredLossDrops != 8 {
		t.Fatalf("configured loss drops = %v, want 8", got.ConfiguredLossDrops)
	}
	if got.NetemQueueDrops == nil || *got.NetemQueueDrops != 3 {
		t.Fatalf("queue drops = %v, want 3", got.NetemQueueDrops)
	}
}

func TestForwardingOwnershipRestoresOnlyForLastOwner(t *testing.T) {
	guardExists := false
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		if program == "sysctl" && slices.Equal(args, []string{"-n", "net.ipv4.ip_forward"}) {
			return []byte("0\n"), nil
		}
		if program == "nft" && slices.Equal(args, []string{"-j", "list", "table", "inet", forwardingGuardTable}) {
			if !guardExists {
				return nil, &CommandError{Program: "nft", Output: "Error: No such file or directory", Err: errors.New("exit status 1")}
			}
			return []byte(`{"nftables":[{"table":{"family":"inet","name":"topolab_forward_guard","comment":"topolab:shared:forwarding-guard"}}]}`), nil
		}
		if program == "nft" && len(args) == 2 && args[0] == "-f" {
			guardExists = true
		}
		if program == "nft" && slices.Equal(args, []string{"delete", "table", "inet", forwardingGuardTable}) {
			guardExists = false
		}
		return nil, nil
	}}
	m := New(fake, Options{})
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := m.acquireForwarding(ctx, dir, "a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.acquireForwarding(ctx, dir, "b", nil); err != nil {
		t.Fatal(err)
	}
	if !fake.contains("nft -f") {
		t.Fatal("original forwarding=0 did not install the shared default-drop guard")
	}
	if err := m.releaseForwarding(ctx, dir, "a"); err != nil {
		t.Fatal(err)
	}
	if fake.contains("net.ipv4.ip_forward=0") {
		t.Fatal("forwarding restored while owner b remained")
	}
	if err := m.releaseForwarding(ctx, dir, "b"); err != nil {
		t.Fatal(err)
	}
	if !fake.contains("net.ipv4.ip_forward=0") {
		t.Fatal("original forwarding value was not restored")
	}
	if guardExists {
		t.Fatal("shared forwarding guard remained after the final owner")
	}
}

func TestForwardingGuardRulesetIsQuotedAndTransactional(t *testing.T) {
	st := forwardingState{Owners: []string{"lab"}, Guards: map[string][]forwardingGuard{"lab": {{HostDevice: "tl:bad", EgressInterface: "eth0", Sources: []string{"10.0.0.0/24"}}}}}
	rules := forwardingGuardRuleset(st, true)
	for _, want := range []string{"delete table inet " + forwardingGuardTable, `comment "topolab:shared:forwarding-guard"`, "policy drop", `iifname "tl:bad"`, `comment "topolab:v1:lab:global-forward-guard:tl:bad:10.0.0.0/24:out"`} {
		if !strings.Contains(rules, want) {
			t.Errorf("ruleset missing %q:\n%s", want, rules)
		}
	}
}
