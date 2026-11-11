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

func TestStorageOnlyCheckpointIsNotTreatedAsLegacyDatapath(t *testing.T) {
	partial := state.NewManifest("lab")
	partial.Storages["thin"] = &state.Storage{Name: "thin", Type: "lvm-thin", Pending: "create-pool"}
	if requiresLegacyDatapathDown(partial) {
		t.Fatal("storage-only write-ahead checkpoint was classified as an active legacy datapath")
	}
	partial.DatapathVersion = 1
	if !requiresLegacyDatapathDown(partial) {
		t.Fatal("active legacy datapath was not classified for down-first migration")
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
	o, err := ParseCaptureArgs([]string{"lan", "--endpoint", "vm:eth0", "--direction", "egress", "--duration", "250ms", "--count", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Link != "lan" || o.Endpoint != "vm:eth0" || o.Direction != "egress" || o.Duration != 250*time.Millisecond || o.Count != 3 {
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

func TestRootCACertificatesPreserveLegacyBootstrapAndAreWriteOnce(t *testing.T) {
	legacy := struct {
		AuthorizedKeys []string          `json:"authorized_keys"`
		Network        cloudinit.Network `json:"network"`
	}{AuthorizedKeys: []string{"key"}, Network: cloudinit.Network{}}
	current := bootstrapSnapshot{AuthorizedKeys: []string{"key"}, Network: cloudinit.Network{}}
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	currentJSON, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(currentJSON) != string(legacyJSON) {
		t.Fatalf("empty Root CA field changed legacy snapshot:\nlegacy=%s\ncurrent=%s", legacyJSON, currentJSON)
	}
	legacyHash, _ := state.Hash(legacy)
	currentHash, _ := state.Hash(current)
	if currentHash != legacyHash {
		t.Fatalf("empty Root CA field changed bootstrap hash: %s != %s", currentHash, legacyHash)
	}

	withCA := bootstrapSnapshot{AuthorizedKeys: []string{"key"}, Network: cloudinit.Network{}, RootCACertificates: []string{"canonical certificate"}}
	caHash, _ := state.Hash(withCA)
	manifest := state.NewManifest("lab")
	manifest.Nodes["vm"] = &state.Node{Kind: "vm", StorageIdentityHash: "storage", BootstrapHash: currentHash}
	desired := map[string]*state.Node{"vm": {Kind: "vm", StorageIdentityHash: "storage", BootstrapHash: caHash}}
	if err := New(nil, nil).validateExisting(context.Background(), manifest, desired); err == nil || !strings.Contains(err.Error(), "write-once") {
		t.Fatalf("Root CA bootstrap change error = %v", err)
	}
}

func TestSeedReconstructionUsesRecordedRootCACertificates(t *testing.T) {
	snapshot := bootstrapSnapshot{RootCACertificates: []string{"-----BEGIN CERTIFICATE-----\nrecorded\n-----END CERTIFICATE-----\n"}}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	node := &state.Node{StorageIdentityHash: "storage", BootstrapHash: "bootstrap", BootstrapSnapshot: raw}
	seed, err := seedFor(&config.Config{Name: "lab"}, "vm", node)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(seed.UserData), "recorded") {
		t.Fatalf("reconstructed seed omitted recorded Root CA:\n%s", seed.UserData)
	}

	first := vmRootCACertificates(&config.VM{RootCACertificates: []*config.RootCACertificate{{Path: "/first.pem", CanonicalPEM: "same"}}})
	second := vmRootCACertificates(&config.VM{RootCACertificates: []*config.RootCACertificate{{Path: "/second.pem", CanonicalPEM: "same"}}})
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("certificate source path affected bootstrap content: %v != %v", first, second)
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

func TestDiskIOPolicyChangesOnlyAffectedVMRestartIdentity(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "tlcache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	layout, err := state.NewLayout(base, "lab")
	if err != nil {
		t.Fatal(err)
	}
	storage := &config.Storage{Name: "local", Type: "file", RootProvisioning: "snapshot"}
	vm := func(name string) *config.VM {
		return &config.VM{
			Name: name, Template: "base", MemoryBalloon: &config.MemoryBalloon{},
			RootDisk:        config.RootDisk{Storage: "local", Bus: "scsi", Cache: "none", AIO: "auto"},
			AdditionalDisks: []*config.Disk{{Name: "data", SizeGiB: 1, Storage: "local", Bus: "scsi", Cache: "none", AIO: "auto"}},
		}
	}
	cfg := &config.Config{Name: "lab", Storages: []*config.Storage{storage}, VMs: []*config.VM{vm("vm1"), vm("vm2")}}
	templates := map[string]qemu.TemplateInfo{"base": {Path: "/base.qcow2", Digest: "digest", CapacityBytes: qemu.GiB}}
	before, err := New(nil, nil).desiredNodes(cfg, layout, templates)
	if err != nil {
		t.Fatal(err)
	}
	cfg.VMs[0].RootDisk.Cache = "writeback"
	afterRoot, err := New(nil, nil).desiredNodes(cfg, layout, templates)
	if err != nil {
		t.Fatal(err)
	}
	if before["vm1"].RestartHash == afterRoot["vm1"].RestartHash || before["vm2"].RestartHash != afterRoot["vm2"].RestartHash {
		t.Fatalf("root cache change affected wrong restart hashes: before=%#v after=%#v", before, afterRoot)
	}
	if before["vm1"].StorageIdentityHash != afterRoot["vm1"].StorageIdentityHash || before["vm1"].RootDisk.Path != afterRoot["vm1"].RootDisk.Path || before["vm1"].AdditionalDisks[0].Path != afterRoot["vm1"].AdditionalDisks[0].Path {
		t.Fatal("root cache change altered storage identity")
	}
	cfg.VMs[0].AdditionalDisks[0].Cache = "writeback"
	afterAdditional, err := New(nil, nil).desiredNodes(cfg, layout, templates)
	if err != nil {
		t.Fatal(err)
	}
	if afterRoot["vm1"].RestartHash == afterAdditional["vm1"].RestartHash || afterRoot["vm2"].RestartHash != afterAdditional["vm2"].RestartHash {
		t.Fatalf("additional cache change affected wrong restart hashes: root=%#v additional=%#v", afterRoot, afterAdditional)
	}
	if afterRoot["vm1"].StorageIdentityHash != afterAdditional["vm1"].StorageIdentityHash || afterRoot["vm1"].AdditionalDisks[0].Path != afterAdditional["vm1"].AdditionalDisks[0].Path {
		t.Fatal("additional cache change altered storage identity")
	}
	cfg.VMs[0].RootDisk.AIO = "threads"
	afterRootAIO, err := New(nil, nil).desiredNodes(cfg, layout, templates)
	if err != nil {
		t.Fatal(err)
	}
	if afterAdditional["vm1"].RestartHash == afterRootAIO["vm1"].RestartHash || afterAdditional["vm2"].RestartHash != afterRootAIO["vm2"].RestartHash {
		t.Fatalf("root AIO change affected wrong restart hashes: before=%#v after=%#v", afterAdditional, afterRootAIO)
	}
	cfg.VMs[0].AdditionalDisks[0].AIO = "io_uring"
	afterAdditionalAIO, err := New(nil, nil).desiredNodes(cfg, layout, templates)
	if err != nil {
		t.Fatal(err)
	}
	if afterRootAIO["vm1"].RestartHash == afterAdditionalAIO["vm1"].RestartHash || afterRootAIO["vm2"].RestartHash != afterAdditionalAIO["vm2"].RestartHash {
		t.Fatalf("additional AIO change affected wrong restart hashes: before=%#v after=%#v", afterRootAIO, afterAdditionalAIO)
	}
	if afterRootAIO["vm1"].StorageIdentityHash != afterAdditionalAIO["vm1"].StorageIdentityHash || afterRootAIO["vm1"].RootDisk.Path != afterAdditionalAIO["vm1"].RootDisk.Path || afterRootAIO["vm1"].AdditionalDisks[0].Path != afterAdditionalAIO["vm1"].AdditionalDisks[0].Path {
		t.Fatal("AIO changes altered storage identity")
	}
	for _, change := range []struct {
		name  string
		apply func()
	}{
		{"root writeback to unsafe", func() { cfg.VMs[0].RootDisk.Cache = "unsafe" }},
		{"additional writeback to unsafe", func() { cfg.VMs[0].AdditionalDisks[0].Cache = "unsafe" }},
		{"root unsafe to none", func() { cfg.VMs[0].RootDisk.Cache = "none" }},
	} {
		previous, err := New(nil, nil).desiredNodes(cfg, layout, templates)
		if err != nil {
			t.Fatal(err)
		}
		change.apply()
		next, err := New(nil, nil).desiredNodes(cfg, layout, templates)
		if err != nil {
			t.Fatal(err)
		}
		if previous["vm1"].RestartHash == next["vm1"].RestartHash || previous["vm2"].RestartHash != next["vm2"].RestartHash {
			t.Fatalf("%s affected wrong restart hashes: before=%#v after=%#v", change.name, previous, next)
		}
		if previous["vm1"].StorageIdentityHash != next["vm1"].StorageIdentityHash || previous["vm1"].RootDisk.Path != next["vm1"].RootDisk.Path || previous["vm1"].AdditionalDisks[0].Path != next["vm1"].AdditionalDisks[0].Path {
			t.Fatalf("%s altered storage identity", change.name)
		}
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
	if report.SchemaVersion != 2 || report.Topology.State != "active" || report.Network.State != "ready" {
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

func TestPSJSONIncludesExplicitStorageState(t *testing.T) {
	cfg := &config.Config{Name: "lab", Storages: []*config.Storage{{Name: "files", Type: "file", RootProvisioning: "copy"}}, Routers: []*config.Router{{Name: "edge"}}}
	m := state.NewManifest("lab")
	m.Storages["files"] = &state.Storage{Name: "files", Type: "file", RootProvisioning: "copy", Warnings: []string{"example warning"}}
	var out bytes.Buffer
	a := New(&out, nil)
	a.defaults()
	if err := a.ps(context.Background(), cfg, m, []string{"--format", "json"}); err != nil {
		t.Fatal(err)
	}
	var report psReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 2 || len(report.Storages) != 1 || report.Storages[0].State != "ready" || report.Storages[0].Provisioning != "copy" || len(report.Storages[0].Warnings) != 1 {
		t.Fatalf("storage status = %#v", report.Storages)
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
	var probed string
	a.ProbeIOUring = func(_ context.Context, executable string) error { probed = executable; return nil }
	if err := a.runtimePreflight(context.Background(), testVMConfig(), overrides); err != nil {
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
	if probed != overrides.QEMUSystem || a.autoAIO != "io_uring" {
		t.Fatalf("io_uring probe executable/effective AIO = %q/%q", probed, a.autoAIO)
	}
}

func TestAIOPreflightAutoFallbackAndExplicitSelection(t *testing.T) {
	overrides := testHelperOverrides(t)
	newApp := func(stderr *bytes.Buffer, probe func(context.Context, string) error) *App {
		a := New(nil, stderr)
		a.defaults()
		a.Preflight = func(preflight.Options) error { return nil }
		a.ProbeIOUring = probe
		return a
	}
	t.Run("auto fallback", func(t *testing.T) {
		var stderr bytes.Buffer
		calls := 0
		a := newApp(&stderr, func(context.Context, string) error { calls++; return errors.New("operation not supported") })
		if err := a.runtimePreflight(context.Background(), testVMConfig(), overrides); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || a.autoAIO != "threads" || strings.Count(stderr.String(), "aio auto is using threads") != 1 {
			t.Fatalf("calls=%d effective=%q stderr=%q", calls, a.autoAIO, stderr.String())
		}
	})
	t.Run("explicit io_uring is strict", func(t *testing.T) {
		cfg := testVMConfig()
		cfg.VMs[0].RootDisk.AIO = "io_uring"
		a := newApp(&bytes.Buffer{}, func(context.Context, string) error { return errors.New("operation not supported") })
		if err := a.runtimePreflight(context.Background(), cfg, overrides); err == nil || !strings.Contains(err.Error(), "explicitly requested") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("threads and native skip probe", func(t *testing.T) {
		cfg := testVMConfig()
		cfg.VMs[0].RootDisk.AIO = "threads"
		cfg.VMs[0].AdditionalDisks = []*config.Disk{{Name: "data", AIO: "native"}}
		a := newApp(&bytes.Buffer{}, func(context.Context, string) error { t.Fatal("unexpected io_uring probe"); return nil })
		if err := a.runtimePreflight(context.Background(), cfg, overrides); err != nil {
			t.Fatal(err)
		}
		if a.autoAIO != "threads" {
			t.Fatalf("unused auto AIO = %q", a.autoAIO)
		}
	})
}

func TestExplicitIOUringProbeFailurePrecedesStateMutation(t *testing.T) {
	overrides := testHelperOverrides(t)
	parent, err := os.MkdirTemp("/tmp", "tlaio-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	working := filepath.Join(parent, "not-created")
	cfg := testVMConfig()
	cfg.VMs[0].RootDisk.AIO = "io_uring"
	a := New(nil, nil)
	a.Preflight = func(preflight.Options) error { return nil }
	a.ProbeIOUring = func(context.Context, string) error { return errors.New("operation not supported") }
	if runErr := a.Run(context.Background(), Command{Name: "up", Topology: cfg, WorkingDir: working, Helpers: overrides}); runErr == nil || !strings.Contains(runErr.Error(), "explicitly requested") {
		t.Fatalf("preflight error = %v", runErr)
	}
	if _, err := os.Stat(working); !os.IsNotExist(err) {
		t.Fatalf("failed AIO preflight created state: %v", err)
	}
}

func TestQEMUSpecResolvesAIOAndExplicitSnapshotBacking(t *testing.T) {
	one := 1
	cfg := &config.Config{Name: "lab", VMs: []*config.VM{{
		Name: "vm", CPU: &config.CPU{Model: "host", Sockets: &one, Cores: &one, Threads: &one}, MemoryBalloon: &config.MemoryBalloon{},
		RootDisk:        config.RootDisk{Cache: "none", AIO: "auto"},
		AdditionalDisks: []*config.Disk{{Name: "data", Cache: "none", AIO: "native"}},
	}}}
	n := &state.Node{TemplatePath: "/template.qcow2", RootDisk: &state.Disk{Name: "root", Path: "/root.qcow2", Format: "qcow2", Provisioning: "snapshot"}, AdditionalDisks: []state.Disk{{Name: "data", Path: "/dev/vg/data", Format: "raw"}}}
	a := New(nil, nil)
	a.autoAIO = "io_uring"
	spec, err := a.qemuSpec(cfg, "vm", n, &network.State{TAPs: map[string]network.TAPState{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Disks) != 2 || spec.Disks[0].AIO != "io_uring" || spec.Disks[0].BackingPath != "/template.qcow2" || spec.Disks[1].AIO != "native" || spec.Disks[1].BackingPath != "" {
		t.Fatalf("QEMU disks = %#v", spec.Disks)
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

// digestImages reports the digest it is told to for every template path.
type digestImages struct {
	checkpointImages
	digest string
}

func (d *digestImages) ValidateTemplate(_ context.Context, path string) (qemu.TemplateInfo, error) {
	return qemu.TemplateInfo{Path: path, Digest: d.digest}, nil
}

func TestValidateTemplatesChecksExpectedDigests(t *testing.T) {
	doc := `name: lab
templates:
  - {name: base, path: /templates/base.qcow2}
storages:
  - {name: local, type: file, root_provisioning: snapshot}
vms:
  - name: vm
    template: base
    root_disk: {storage: local}
    ssh_authorized_keys: ["` + "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test" + `"]
    interfaces: [{name: eth0}]
switches: [{name: lan}]
links:
  - name: vm-lan
    endpoints:
      - {node: vm, interface: eth0}
      - {node: lan, interface: vm}
`
	fsys := fakeFiles{"/templates/base.qcow2": []byte("x")}
	cfg, _, err := config.ParseWith([]byte(doc), config.ParseOptions{BaseDir: "/", FS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	a := New(nil, nil)
	a.Images = &digestImages{digest: "aaaa"}
	if _, err := a.validateTemplates(context.Background(), cfg); err != nil {
		t.Fatalf("without expectations: %v", err)
	}
	a.TemplateDigests = map[string]string{"/templates/base.qcow2": "aaaa"}
	if _, err := a.validateTemplates(context.Background(), cfg); err != nil {
		t.Fatalf("matching digest: %v", err)
	}
	a.TemplateDigests = map[string]string{"/templates/base.qcow2": "bbbb"}
	_, err = a.validateTemplates(context.Background(), cfg)
	if !errors.Is(err, ErrTemplateDigest) || !strings.Contains(err.Error(), "has digest aaaa, want bbbb") {
		t.Fatalf("mismatch err = %v", err)
	}
	a.TemplateDigests = map[string]string{"/other/path.qcow2": "bbbb"}
	if _, err := a.validateTemplates(context.Background(), cfg); err != nil {
		t.Fatalf("expectation for another path: %v", err)
	}
}

// fakeFiles serves regular files from memory for configuration parsing.
type fakeFiles map[string][]byte

type fakeInfo struct{ size int64 }

func (fakeInfo) Name() string       { return "f" }
func (i fakeInfo) Size() int64      { return i.size }
func (fakeInfo) Mode() os.FileMode  { return 0o444 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return false }
func (fakeInfo) Sys() any           { return nil }

func (f fakeFiles) Stat(path string) (os.FileInfo, error) {
	data, ok := f[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return fakeInfo{size: int64(len(data))}, nil
}

func (f fakeFiles) ReadFile(path string) ([]byte, error) {
	data, ok := f[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func TestLockNoticeRunsOnceWhileTheTopologyLockIsHeld(t *testing.T) {
	layoutDir, err := os.MkdirTemp("/tmp", "tlnote-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(layoutDir)
	layout, err := state.NewLayout(layoutDir, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	holder, err := state.AcquireLock(context.Background(), layout.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	notices := 0
	a := New(nil, nil)
	a.LockNotice = func() { notices++ }
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := state.AcquireLockNotify(ctx, layout.LockPath, a.LockNotice); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if notices != 1 {
		t.Fatalf("notice ran %d times, want 1", notices)
	}
}

func TestRunWritesLockWaitNoticeToStderrWhenNoCallbackIsSet(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "tlwait-")
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
	holder, err := state.AcquireSharedLock(context.Background(), layout.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	command := Command{Name: "down", WorkingDir: base, Topology: &config.Config{Name: "lab"}}

	var stderr bytes.Buffer
	a := New(nil, &stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx, command); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if got, want := stderr.String(), LockWaitNotice+"\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}

	custom := 0
	var quiet bytes.Buffer
	b := New(nil, &quiet)
	b.LockNotice = func() { custom++ }
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if err := b.Run(ctx2, command); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if custom != 1 || quiet.Len() != 0 {
		t.Fatalf("custom notice ran %d times and stderr = %q; want 1 and empty", custom, quiet.String())
	}
}
