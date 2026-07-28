package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/torob/topolab/internal/cloudinit"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
)

type checkpointImages struct {
	sizes    map[string]uint64
	failDisk string
}

func (f *checkpointImages) ValidateTemplate(context.Context, string) (qemu.TemplateInfo, error) {
	return qemu.TemplateInfo{}, nil
}
func (f *checkpointImages) Info(_ context.Context, path string) (qemu.ImageInfo, error) {
	size, ok := f.sizes[path]
	if !ok {
		return qemu.ImageInfo{}, os.ErrNotExist
	}
	return qemu.ImageInfo{Format: "qcow2", VirtualSize: size}, nil
}
func (f *checkpointImages) CreateOverlay(_ context.Context, path string, _ qemu.TemplateInfo, size uint64) error {
	return f.create(path, size)
}
func (f *checkpointImages) CreateDataDisk(_ context.Context, path string, size uint64) error {
	if filepath.Base(path) == f.failDisk {
		return errors.New("injected disk failure")
	}
	return f.create(path, size)
}
func (f *checkpointImages) create(path string, size uint64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte("qcow2"), 0o600); err != nil {
		return err
	}
	f.sizes[path] = size
	return nil
}
func (f *checkpointImages) ResizeOffline(_ context.Context, g qemu.Growth) error {
	f.sizes[g.Path] = g.Desired
	return nil
}
func (f *checkpointImages) ResizeOnline(_ context.Context, _ qemu.BlockResizer, g qemu.Growth) error {
	f.sizes[g.Path] = g.Desired
	return nil
}

type checkpointSeeds struct{}

func (checkpointSeeds) Build(_ context.Context, path string, _ cloudinit.Seed) error {
	return os.WriteFile(path, []byte("seed"), 0o600)
}

func shortLayout(t *testing.T) (state.Layout, string) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "tlcp-")
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
	return layout, base
}

func checkpointDesired(layout state.Layout) *state.Node {
	dir, _ := layout.NodeDir("vm")
	raw, _ := json.Marshal(bootstrapSnapshot{})
	return &state.Node{Kind: "vm", StorageIdentityHash: "storage", BootstrapHash: "bootstrap", BootstrapSnapshot: raw, TemplatePath: "/template.qcow2", RootDisk: diskRecord("root", filepath.Join(dir, "root.qcow2"), "scsi", 100), AdditionalDisks: []state.Disk{*diskRecord("a", filepath.Join(dir, "a.qcow2"), "scsi", 20), *diskRecord("b", filepath.Join(dir, "b.qcow2"), "scsi", 30)}, SeedPath: filepath.Join(dir, "seed.iso"), OVMFVarsPath: filepath.Join(dir, "vars.fd"), PIDFile: filepath.Join(dir, "qemu.pid"), SerialLogPath: filepath.Join(layout.LogsDir, "vm.jsonl")}
}

func TestMaterializeCheckpointsDiskIntentAndRetriesPartialCreation(t *testing.T) {
	layout, _ := shortLayout(t)
	want := checkpointDesired(layout)
	if err := os.MkdirAll(filepath.Dir(want.RootDisk.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want.RootDisk.Path, []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want.OVMFVarsPath, []byte("vars"), 0o600); err != nil {
		t.Fatal(err)
	}
	images := &checkpointImages{sizes: map[string]uint64{want.RootDisk.Path: 100}, failDisk: "b.qcow2"}
	a := New(nil, nil)
	a.Images = images
	a.Seeds = checkpointSeeds{}
	a.defaults()
	cfg := &config.Config{Name: "lab", VMs: []*config.VM{{Name: "vm"}}}
	m := state.NewManifest("lab")
	err := a.materializeNodes(context.Background(), cfg, layout, m, map[string]*state.Node{"vm": want}, &network.State{TAPs: map[string]network.TAPState{}}, map[string]qemu.TemplateInfo{"": {CapacityBytes: 100}})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	checkpoint, err := state.Load(layout.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["vm"].RootDisk == nil || len(checkpoint.Nodes["vm"].AdditionalDisks) != 2 {
		t.Fatalf("disk intent was not checkpointed: %#v", checkpoint.Nodes["vm"])
	}
	if _, err := os.Stat(checkpoint.Nodes["vm"].AdditionalDisks[0].Path); err != nil {
		t.Fatalf("first completed disk missing: %v", err)
	}
	images.failDisk = ""
	retry := checkpointDesired(layout)
	if err := a.validateExisting(context.Background(), checkpoint, map[string]*state.Node{"vm": retry}); err != nil {
		t.Fatalf("write-ahead state blocked retry preflight: %v", err)
	}
	if err := a.materializeNodes(context.Background(), cfg, layout, checkpoint, map[string]*state.Node{"vm": retry}, &network.State{TAPs: map[string]network.TAPState{}}, map[string]qemu.TemplateInfo{"": {CapacityBytes: 100}}); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if _, err := os.Stat(retry.AdditionalDisks[1].Path); err != nil {
		t.Fatalf("retry did not create second disk: %v", err)
	}
}

func TestHelperArtifactsAreWriteAheadAndRetryable(t *testing.T) {
	layout, _ := shortLayout(t)
	dhcp := &config.DHCPServer{RangeStart: "10.0.0.10", RangeEnd: "10.0.0.20", Lease: "1h"}
	cfg := &config.Config{Name: "lab", Routers: []*config.Router{{Name: "r", Interfaces: []*config.RouterInterface{{Name: "lan", IPv4: &config.RouterIPv4{DHCPServer: dhcp}}}}}}
	m := state.NewManifest("lab")
	if err := state.Save(layout.ManifestPath, m); err != nil {
		t.Fatal(err)
	}
	a := New(nil, nil)
	a.StartProcess = func([]string, []*os.File) (*qemu.StartedProcess, error) {
		return nil, errors.New("injected start failure")
	}
	a.defaults()
	ns := &network.State{Namespaces: map[string]string{"r": "ns-r"}}
	if err := a.reconcileNamespaceHelpers(context.Background(), cfg, layout, m, ns); err == nil {
		t.Fatal("expected helper start failure")
	}
	checkpoint, err := state.Load(layout.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	h := checkpoint.Helpers["r:lan"]
	if h == nil || h.Process != nil {
		t.Fatalf("helper ownership not checkpointed: %#v", h)
	}
	for _, p := range []string{h.ConfigPath, h.PIDPath, h.LeasePath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("owned helper artifact %s missing: %v", p, err)
		}
	}
	processes := &fakeProcesses{alive: map[int]bool{}, args: map[int][]string{}}
	a.Processes = processes
	a.StartProcess = func(argv []string, _ []*os.File) (*qemu.StartedProcess, error) {
		processes.alive[77] = true
		processes.args[77] = argv
		return &qemu.StartedProcess{PID: 77}, nil
	}
	if err := a.reconcileNamespaceHelpers(context.Background(), cfg, layout, checkpoint, ns); err != nil {
		t.Fatalf("helper retry failed: %v", err)
	}
	if checkpoint.Helpers["r:lan"].Process.PID != 77 {
		t.Fatal("retry process was not recorded")
	}
}

type checkpointNetwork struct{ state *network.State }

func (f checkpointNetwork) Plan(context.Context, network.Config) (*network.Plan, error) {
	return nil, nil
}
func (f checkpointNetwork) Apply(context.Context, *network.Plan) (*network.State, error) {
	return f.state, nil
}
func (f checkpointNetwork) Verify(context.Context, *network.State) error { return nil }
func (f checkpointNetwork) Remove(context.Context, *network.State) error { return nil }
func (f checkpointNetwork) Counters(context.Context, *network.State) (*network.CounterSnapshot, error) {
	return nil, nil
}

type failingQMP struct{}

func (failingQMP) BlockResize(context.Context, string, uint64) error { return errors.New("qmp") }
func (failingQMP) Balloon(context.Context, uint64) error             { return errors.New("qmp") }
func (failingQMP) QueryBalloon(context.Context) (uint64, error)      { return 0, errors.New("qmp") }
func (failingQMP) QueryStatus(context.Context) (string, error)       { return "", errors.New("not ready") }
func (failingQMP) Close() error                                      { return nil }

type checkpointStopper struct{ processes *fakeProcesses }

func (s checkpointStopper) Stop(_ context.Context, pid int, _, _, _ string) error {
	s.processes.alive[pid] = false
	return nil
}

func TestStartCheckpointsPIDWhenReadinessFails(t *testing.T) {
	layout, _ := shortLayout(t)
	one := 1
	cfg := &config.Config{Name: "lab", VMs: []*config.VM{{Name: "vm", CPU: &config.CPU{Model: "host", Sockets: &one, Cores: &one, Threads: &one}, MemoryMB: 128, MemoryBalloon: &config.MemoryBalloon{}, Boot: config.Boot{Order: []string{"disk"}}}}}
	n := checkpointDesired(layout)
	n.AdditionalDisks = nil
	n.SeedPending = false
	n.QMPPath, _ = layout.SocketPath("vm", "qmp")
	n.SerialSocketPath, _ = layout.SocketPath("vm", "serial")
	m := state.NewManifest("lab")
	m.Nodes["vm"] = n
	netState := &network.State{Topology: "lab", TAPs: map[string]network.TAPState{}}
	m.NetworkState, _ = json.Marshal(netState)
	if err := state.Save(layout.ManifestPath, m); err != nil {
		t.Fatal(err)
	}
	processes := &fakeProcesses{alive: map[int]bool{}, args: map[int][]string{}}
	calls := 0
	a := New(nil, nil)
	a.Network = checkpointNetwork{netState}
	a.Processes = processes
	a.Executable = func() (string, error) { return "/topolab", nil }
	a.StartProcess = func(argv []string, _ []*os.File) (*qemu.StartedProcess, error) {
		calls++
		pid := 40 + calls
		processes.alive[pid] = true
		processes.args[pid] = argv
		return &qemu.StartedProcess{PID: pid}, nil
	}
	a.OpenQMP = func(string) qmpClient { return failingQMP{} }
	a.VhostAvailable = func() bool { return false }
	a.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.start(ctx, cfg, layout, m, nil); err == nil {
		t.Fatal("expected readiness failure")
	}
	checkpoint, err := state.Load(layout.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["vm"].PID != 42 || checkpoint.Nodes["vm"].Process == nil || checkpoint.Nodes["vm"].CollectorProcess == nil {
		t.Fatalf("started process state not checkpointed: %#v", checkpoint.Nodes["vm"])
	}
}

func TestRestartCheckpointsSuccessfulStopBeforeLaterHelperError(t *testing.T) {
	layout, _ := shortLayout(t)
	cfg := &config.Config{Name: "lab", VMs: []*config.VM{{Name: "vm"}}}
	n := checkpointDesired(layout)
	n.PID = 10
	n.Process = &state.Process{PID: 10}
	n.CollectorProcess = &state.Process{PID: 11, IdentityArg: "expected-collector"}
	m := state.NewManifest("lab")
	m.Nodes["vm"] = n
	netState := &network.State{Topology: "lab"}
	m.NetworkState, _ = json.Marshal(netState)
	if err := state.Save(layout.ManifestPath, m); err != nil {
		t.Fatal(err)
	}
	processes := &fakeProcesses{alive: map[int]bool{10: true, 11: true}, args: map[int][]string{11: {"not-the-owned-helper"}}}
	a := New(nil, nil)
	a.Network = checkpointNetwork{netState}
	a.Processes = processes
	a.Stopper = checkpointStopper{processes}
	a.defaults()
	if err := a.restart(context.Background(), cfg, layout, m, nil); err == nil {
		t.Fatal("expected collector identity failure")
	}
	checkpoint, err := state.Load(layout.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	got := checkpoint.Nodes["vm"]
	if got.PID != 0 || got.Process != nil {
		t.Fatalf("successful QEMU stop was not checkpointed: %#v", got)
	}
	if got.CollectorProcess == nil {
		t.Fatal("failed collector stop should remain recorded")
	}
}
