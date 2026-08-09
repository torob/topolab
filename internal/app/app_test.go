package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torob/topolab/internal/cloudinit"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/preflight"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
)

func TestDownWithoutStateDoesNotCreateWorkingDirectory(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "tlapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	working := filepath.Join(base, "absent")
	a := New(nil, nil)
	if err := a.Run(context.Background(), Command{Name: "down", WorkingDir: working, Topology: &config.Config{Name: "lab"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(working); !os.IsNotExist(err) {
		t.Fatalf("down created state: %v", err)
	}
}

type fakeProcesses struct {
	alive   map[int]bool
	args    map[int][]string
	signals []os.Signal
}

func (f *fakeProcesses) Alive(pid int) bool                    { return f.alive[pid] }
func (f *fakeProcesses) CommandLine(pid int) ([]string, error) { return f.args[pid], nil }
func (f *fakeProcesses) Signal(pid int, signal os.Signal) error {
	f.signals = append(f.signals, signal)
	f.alive[pid] = false
	return nil
}

func TestNamespaceDNSMasqIsSupervisedAndOwned(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "tlapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	layout, err := state.NewLayout(base, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	dhcp := &config.DHCPServer{RangeStart: "10.0.0.10", RangeEnd: "10.0.0.20", Lease: "12h", DNSServers: []string{"1.1.1.1"}}
	cfg := &config.Config{Name: "lab", Routers: []*config.Router{{Name: "r", Interfaces: []*config.RouterInterface{{Name: "lan", MTU: 1500, IPv4: &config.RouterIPv4{DHCPServer: dhcp}}}}}}
	processes := &fakeProcesses{alive: map[int]bool{}, args: map[int][]string{}}
	a := New(nil, nil)
	a.Processes = processes
	a.StartProcess = func(argv []string, files []*os.File) (*qemu.StartedProcess, error) {
		processes.alive[42] = true
		processes.args[42] = append([]string(nil), argv...)
		return &qemu.StartedProcess{PID: 42}, nil
	}
	a.defaults()
	m := state.NewManifest("lab")
	ns := &network.State{Namespaces: map[string]string{"r": "tlr-r"}}
	if err := a.reconcileNamespaceHelpers(context.Background(), cfg, layout, m, ns); err != nil {
		t.Fatal(err)
	}
	h := m.Helpers["r:lan"]
	if h == nil || h.Process == nil || h.Process.PID != 42 {
		t.Fatalf("helper not persisted: %#v", h)
	}
	data, err := os.ReadFile(h.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "port=0\n") || !strings.Contains(string(data), "dhcp-range=10.0.0.10,10.0.0.20,12h") {
		t.Fatalf("bad config: %s", data)
	}
	if err := a.stopNamespaceHelpers(m); err != nil {
		t.Fatal(err)
	}
	if len(processes.signals) != 1 || processes.alive[42] {
		t.Fatal("helper was not stopped")
	}
}

func TestVhostSelectionOnlyAcceleratesVirtio(t *testing.T) {
	nics := []qemu.NICSpec{{Name: "fast", Model: "virtio"}, {Name: "legacy", Model: "e1000e"}}
	node := &state.Node{TAPs: map[string]state.TAP{"fast": {}, "legacy": {}}}
	applyVhostSelection(nics, node, true)
	if !nics[0].Vhost || nics[1].Vhost {
		t.Fatalf("wrong vhost selection: %#v", nics)
	}
	if node.TAPs["fast"].Datapath != "vhost-net" || node.TAPs["legacy"].Datapath != "userspace" {
		t.Fatalf("wrong reported datapaths: %#v", node.TAPs)
	}
}

func TestDownRemovesTopologyStateButRetainsCapture(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "tlapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	layout, err := state.NewLayout(base, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	m := state.NewManifest("lab")
	if err := state.Save(layout.ManifestPath, m); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.CaptureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(layout.CaptureDir, "keep.pcapng")
	if err := os.WriteFile(capture, []byte("capture"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(nil, nil)
	if err := a.Run(context.Background(), Command{Name: "down", WorkingDir: base, Topology: &config.Config{Name: "lab"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.TopologyDir); !os.IsNotExist(err) {
		t.Fatalf("topology state remains: %v", err)
	}
	if _, err := os.Stat(capture); err != nil {
		t.Fatalf("capture removed: %v", err)
	}
}

func TestDownRejectsUnrecordedTopologyState(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "tlapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	layout, err := state.NewLayout(base, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(layout.ManifestPath, state.NewManifest("lab")); err != nil {
		t.Fatal(err)
	}
	unexpected := filepath.Join(layout.TopologyDir, "operator-note")
	if err := os.WriteFile(unexpected, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(nil, nil)
	err = a.Run(context.Background(), Command{Name: "down", WorkingDir: base, Topology: &config.Config{Name: "lab"}})
	if err == nil || !strings.Contains(err.Error(), "unrecorded topology state") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(unexpected); err != nil {
		t.Fatalf("unrecorded file was removed: %v", err)
	}
}

func TestCaptureParserAllowsDocumentedLinkBeforeOptions(t *testing.T) {
	o, err := parseCapture([]string{"lan", "--endpoint", "vm:eth0", "--direction", "egress", "--duration", "250ms", "--count", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if o.link != "lan" || o.endpoint != "vm:eth0" || o.direction != "egress" || o.duration != 250*time.Millisecond || o.count != 3 {
		t.Fatalf("unexpected options: %#v", o)
	}
}

func TestWriteOnceBootstrapRejectedBeforeImageInspection(t *testing.T) {
	a := New(nil, nil)
	a.defaults()
	old := state.NewManifest("lab")
	old.Nodes["vm"] = &state.Node{Kind: "vm", StorageIdentityHash: "same", BootstrapHash: "old"}
	desired := map[string]*state.Node{"vm": {Kind: "vm", StorageIdentityHash: "same", BootstrapHash: "new"}}
	if err := a.validateExisting(context.Background(), old, desired); err == nil {
		t.Fatal("expected write-once rejection")
	}
}

func TestTranslateNetworkPreservesTransitAllocation(t *testing.T) {
	cfg := &config.Config{Name: "lab", Routers: []*config.Router{{Name: "r", Interfaces: []*config.RouterInterface{{Name: "wan", MTU: 1500, External: &config.External{Sources: []string{"10.0.0.0/24"}, EgressInterface: "eth0", SourceIP: "192.0.2.2"}}}}}}
	prior := &network.State{External: []network.ResolvedExternal{{External: network.External{Router: "r", Interface: "wan", TransitPrefix: "198.18.1.0/30", RouterAddress: "198.18.1.1", HostAddress: "198.18.1.2"}}}}
	got := translateNetwork(cfg, "/work/shared", prior)
	if len(got.Externals) != 1 || got.Externals[0].TransitPrefix != "198.18.1.0/30" {
		t.Fatalf("transit allocation not reused: %#v", got.Externals)
	}
	if len(got.Routers) != 1 || !got.Routers[0].Forwarding {
		t.Fatalf("external router did not request forwarding: %#v", got.Routers)
	}
}

func TestCanonicalNetworkHashStableAcrossPersistedTransit(t *testing.T) {
	cfg := &config.Config{Name: "lab", Routers: []*config.Router{{Name: "r", Interfaces: []*config.RouterInterface{{Name: "wan", MTU: 1500, External: &config.External{Sources: []string{"10.0.0.0/24"}, EgressInterface: "auto", SourceIP: "auto"}}}}}}
	resolved := network.ResolvedExternal{External: network.External{Router: "r", Interface: "wan", MTU: 1500, EgressInterface: "eth0", SourceIP: "192.0.2.2", Sources: []string{"10.0.0.0/24"}, TransitPrefix: "198.18.1.0/30", RouterAddress: "198.18.1.1", HostAddress: "198.18.1.2"}, Masquerade: true}
	first := canonicalNetworkConfig(translateNetwork(cfg, "/work/shared", nil), []network.ResolvedExternal{resolved})
	device := externalHostDevice("lab", resolved.External)
	prior := &network.State{Topology: "lab", External: []network.ResolvedExternal{resolved}, Resources: []network.Resource{{Name: device, Alias: "owned-external"}}}
	secondTranslated := translateNetwork(cfg, "/work/shared", prior)
	if len(secondTranslated.ExistingOwnedRoutes) != 2 {
		t.Fatalf("expected exact persisted transit/source ownership, got %#v", secondTranslated.ExistingOwnedRoutes)
	}
	second := canonicalNetworkConfig(secondTranslated, []network.ResolvedExternal{resolved})
	a, _ := state.Hash(first)
	b, _ := state.Hash(second)
	if a != b {
		t.Fatalf("canonical hashes differ across repeat up: %s != %s\nfirst=%#v\nsecond=%#v", a, b, first, second)
	}
	if second.External[0].TransitPrefix != resolved.TransitPrefix {
		t.Fatalf("transit allocation changed: %#v", second.External[0])
	}
	explicit := resolved
	explicit.Masquerade = false
	c, _ := state.Hash(canonicalNetworkConfig(translateNetwork(cfg, "/work/shared", nil), []network.ResolvedExternal{explicit}))
	if c == a {
		t.Fatal("canonical hash did not distinguish masquerade from fixed SNAT")
	}
}

func TestMissingRootOverlayIsRecreated(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "moved-root.qcow2")
	want := &state.Node{StorageIdentityHash: "same", RootDisk: &state.Disk{Path: missing}}
	old := &state.Node{StorageIdentityHash: "same", RootDisk: &state.Disk{Path: missing}}
	if !rootDiskNeedsCreate(old, want) {
		t.Fatal("missing recorded overlay must be recreated")
	}
	if err := os.WriteFile(missing, []byte("qcow2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rootDiskNeedsCreate(old, want) {
		t.Fatal("existing overlay with unchanged storage identity must be preserved")
	}
}

func TestMalformedNetworkStateIsRejected(t *testing.T) {
	m := state.NewManifest("lab")
	m.NetworkState = []byte(`{"topology":`)
	if _, err := manifestNetwork(m); err == nil {
		t.Fatal("expected malformed network state error")
	}
}

func TestManifestCannotRedirectManagedDeletion(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "tlapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	layout, err := state.NewLayout(base, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "keep.qcow2")
	if err := os.WriteFile(external, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := state.NewManifest("lab")
	m.Nodes["vm"] = &state.Node{RootDisk: &state.Disk{Path: external}}
	if err := validateManifestPaths(layout, m); err == nil {
		t.Fatal("expected unsafe manifest path rejection")
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("external file was changed: %v", err)
	}
}

func TestManifestRejectsSymlinkedManagedParent(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "tlapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	layout, err := state.NewLayout(base, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	nodeDir, _ := layout.NodeDir("vm")
	if err := os.Symlink(t.TempDir(), nodeDir); err != nil {
		t.Fatal(err)
	}
	m := state.NewManifest("lab")
	m.Nodes["vm"] = &state.Node{RootDisk: &state.Disk{Path: filepath.Join(nodeDir, "root.qcow2")}}
	if err := validateManifestPaths(layout, m); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestRootCapacityBelowTemplateFailsBeforeMaterialization(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "tlapp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	layout, err := state.NewLayout(base, "lab")
	if err != nil {
		t.Fatal(err)
	}
	one := uint64(1)
	cfg := &config.Config{Name: "lab", VMs: []*config.VM{
		{Name: "vm", Template: "base", RootDisk: config.RootDisk{SizeGiB: &one, Bus: "scsi"}, MemoryBalloon: &config.MemoryBalloon{}},
	}}
	_, err = New(nil, nil).desiredNodes(cfg, layout, map[string]qemu.TemplateInfo{"base": {CapacityBytes: 2 * qemu.GiB}})
	if err == nil || !strings.Contains(err.Error(), "smaller than template") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTranslateNetworkUsesRouterEndpoint(t *testing.T) {
	cfg := &config.Config{
		Name:     "lab",
		Routers:  []*config.Router{{Name: "r", Interfaces: []*config.RouterInterface{{Name: "lan", MTU: 1500, MAC: "02:00:00:00:00:01"}}}},
		Switches: []*config.Switch{{Name: "s", MTU: 1500}},
		Links: []*config.Link{{Name: "l", MTU: 1500, Endpoints: []*config.Endpoint{
			{Node: "r", Interface: "lan"},
			{Node: "s", Interface: "r", Port: &config.Port{Mode: "access"}},
		}}},
	}
	got := translateNetwork(cfg, "/work/shared", nil)
	if len(got.Routers) != 1 || len(got.Links) != 1 || got.Links[0].Endpoints[0].Kind != network.EndpointRouter {
		t.Fatalf("router was not translated to the managed router datapath: %#v", got)
	}
}

func TestVMSelectionRejectsRouter(t *testing.T) {
	cfg := &config.Config{Name: "lab", VMs: []*config.VM{{Name: "vm"}}, Routers: []*config.Router{{Name: "r"}}}
	if got, err := selectNodes(cfg, nil); err != nil || len(got) != 1 || got[0] != "vm" {
		t.Fatalf("default VM selection = %v, %v", got, err)
	}
	if _, err := selectNodes(cfg, []string{"r"}); err == nil || !strings.Contains(err.Error(), `unknown VM "r"`) {
		t.Fatalf("router selection error = %v", err)
	}
}

func TestPSListsVMsBeforeRoutersAndLabelsRouter(t *testing.T) {
	cfg := &config.Config{Name: "lab", VMs: []*config.VM{{Name: "vm"}}, Routers: []*config.Router{{Name: "r"}}}
	netState := &network.State{Topology: "lab"}
	m := state.NewManifest("lab")
	m.Nodes["vm"] = &state.Node{Kind: "vm"}
	m.NetworkState, _ = json.Marshal(netState)
	var out bytes.Buffer
	a := New(&out, nil)
	a.Network = checkpointNetwork{state: netState}
	a.defaults()
	if err := a.ps(context.Background(), cfg, m, nil); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	vmLine, routerLine := strings.Index(text, "vm\tvm\t"), strings.Index(text, "r\trouter\tready\t-\t")
	if vmLine < 0 || routerLine < 0 || vmLine > routerLine {
		t.Fatalf("unexpected ps output:\n%s", text)
	}
}

func TestPSJSONReportsStableTypedState(t *testing.T) {
	target := uint64(256)
	cfg := &config.Config{Name: "lab", ManagementHost: config.ManagementHost{Interfaces: []*config.ManagementHostInterface{{Name: "management"}}}, VMs: []*config.VM{{Name: "vm", GuestAgent: true}}, Routers: []*config.Router{{Name: "edge"}}}
	netState := &network.State{
		Topology:                "lab",
		FabricNamespace:         "fabric",
		ManagementHostNamespace: "management",
		Namespaces:              map[string]string{"edge": "router-edge"},
		ManagementHostInterfaces: map[string]network.ManagementHostInterface{
			"management": {LogicalName: "management", DeviceName: "tlm0", MTU: 1500, Addresses: []string{"192.0.2.1/24"}, Link: "lan"},
		},
		ManagementHostNFTTable: "tl-mgmt",
		External:               []network.ResolvedExternal{{External: network.External{Router: "edge", Interface: "wan", EgressInterface: "eth0", SourceIP: "192.0.2.10", Sources: []string{"10.0.0.0/24"}, TransitPrefix: "198.18.0.0/30", RouterAddress: "198.18.0.1", HostAddress: "198.18.0.2"}, Masquerade: true}},
	}
	m := state.NewManifest("lab")
	m.Nodes["vm"] = &state.Node{Kind: "vm", BalloonTargetMB: &target, TAPs: map[string]state.TAP{"eth0": {Datapath: "vhost-net"}}}
	m.NetworkState, _ = json.Marshal(netState)
	var out bytes.Buffer
	a := New(&out, nil)
	a.Network = checkpointNetwork{state: netState}
	a.Processes = &fakeProcesses{alive: map[int]bool{}}
	a.GuestAgentProbe = func(context.Context, string) bool { return false }
	a.defaults()
	if err := a.ps(context.Background(), cfg, m, []string{"--format", "json"}); err != nil {
		t.Fatal(err)
	}
	var report psReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, out.String())
	}
	if report.SchemaVersion != 1 || report.Topology.State != "active" || report.Network.State != "ready" {
		t.Fatalf("status = %#v", report)
	}
	if len(report.Nodes) != 2 || report.Nodes[0].QEMU == nil || report.Nodes[0].QEMU.State != "stopped" || len(report.Nodes[0].Datapaths) != 1 || report.Nodes[0].Balloon == nil || report.Nodes[0].GuestAgent == nil {
		t.Fatalf("VM status = %#v", report.Nodes)
	}
	if len(report.Nodes[1].ExternalInterfaces) != 1 || !report.Nodes[1].ExternalInterfaces[0].Masquerade {
		t.Fatalf("router status = %#v", report.Nodes[1])
	}
	if len(report.Namespaces) != 3 || len(report.ManagementHostInterfaces) != 1 || report.ManagementHostInterfaces[0].Device != "tlm0" || !report.ManagementHostInterfaces[0].Isolated {
		t.Fatalf("network details = %#v %#v", report.Namespaces, report.ManagementHostInterfaces)
	}
}

type driftNetwork struct {
	checkpointNetwork
	err error
}

func (f driftNetwork) Verify(context.Context, *network.State) error { return f.err }

func TestPSJSONAbsentAndDriftKeepErrorsOutOfState(t *testing.T) {
	cfg := &config.Config{Name: "lab", Routers: []*config.Router{{Name: "edge"}}}
	var out bytes.Buffer
	a := New(&out, nil)
	a.defaults()
	if err := a.ps(context.Background(), cfg, nil, []string{"--format", "json"}); err != nil {
		t.Fatal(err)
	}
	var absent psReport
	if err := json.Unmarshal(out.Bytes(), &absent); err != nil || absent.Topology.State != "absent" || absent.Network.State != "absent" {
		t.Fatalf("absent report = %#v, %v", absent, err)
	}
	out.Reset()
	netState := &network.State{Topology: "lab"}
	m := state.NewManifest("lab")
	m.NetworkState, _ = json.Marshal(netState)
	a.Network = driftNetwork{checkpointNetwork: checkpointNetwork{state: netState}, err: errors.New("wrong ownership")}
	if err := a.ps(context.Background(), cfg, m, []string{"--format", "json"}); err != nil {
		t.Fatal(err)
	}
	var drift psReport
	if err := json.Unmarshal(out.Bytes(), &drift); err != nil {
		t.Fatal(err)
	}
	if drift.Network.State != "drift" || drift.Network.DriftError != "wrong ownership" || strings.Contains(drift.Network.State, "wrong ownership") {
		t.Fatalf("drift report = %#v", drift)
	}
}

func testVMConfig() *config.Config {
	one := 1
	return &config.Config{Name: "lab", VMs: []*config.VM{{Name: "vm", CPU: &config.CPU{Model: "host", Sockets: &one, Cores: &one, Threads: &one}, MemoryBalloon: &config.MemoryBalloon{}}}}
}

func testHelperOverrides(t *testing.T) HelperOverrides {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return HelperOverrides{QEMUSystem: write("qemu", 0o755), QEMUImg: write("qemu-img", 0o755), Xorriso: write("xorriso", 0o755), OVMFCode: write("code.fd", 0o644), OVMFVars: write("vars.fd", 0o644)}
}

func TestVMHelperOverridesReachPreflightAndRuntimeConsumers(t *testing.T) {
	overrides := testHelperOverrides(t)
	a := New(nil, nil)
	a.defaults()
	var got preflight.Options
	a.Preflight = func(options preflight.Options) error { got = options; return nil }
	if err := a.runtimePreflight(testVMConfig(), overrides); err != nil {
		t.Fatal(err)
	}
	if got.Helpers.QEMUSystem != overrides.QEMUSystem || got.Helpers.QEMUImg != overrides.QEMUImg || got.Helpers.Xorriso != overrides.Xorriso || got.Helpers.OVMFCode != overrides.OVMFCode || got.Helpers.OVMFVars != overrides.OVMFVars {
		t.Fatalf("preflight helpers = %#v", got.Helpers)
	}
	images, ok := a.Images.(*qemu.ImageManager)
	if !ok || images.QEMUImg != overrides.QEMUImg {
		t.Fatalf("image manager = %#v", a.Images)
	}
	seeds, ok := a.Seeds.(*cloudinit.ISOBuilder)
	if !ok || seeds.Xorriso != overrides.Xorriso {
		t.Fatalf("seed builder = %#v", a.Seeds)
	}
	if a.QEMUBuilder.Executable != overrides.QEMUSystem || a.OVMFCode != overrides.OVMFCode || a.OVMFVars != overrides.OVMFVars {
		t.Fatalf("QEMU runtime helpers = %#v %q %q", a.QEMUBuilder, a.OVMFCode, a.OVMFVars)
	}
}

func TestFailedVMPreflightOccursBeforeStateOrNetworkMutation(t *testing.T) {
	overrides := testHelperOverrides(t)
	parent, err := os.MkdirTemp("/tmp", "tlpf-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	base := filepath.Join(parent, "not-created")
	a := New(nil, nil)
	a.Preflight = func(preflight.Options) error { return errors.New("KVM unavailable") }
	if runErr := a.Run(context.Background(), Command{Name: "up", Topology: testVMConfig(), WorkingDir: base, Helpers: overrides}); runErr == nil || !strings.Contains(runErr.Error(), "KVM unavailable") {
		t.Fatalf("preflight error = %v", runErr)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("failed preflight created state: %v", err)
	}
}
