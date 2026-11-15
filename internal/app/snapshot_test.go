package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
	storagepkg "github.com/torob/topolab/internal/storage"
)

// snapshotEvents records the order of fake calls across goroutines.
type snapshotEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *snapshotEvents) add(format string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, fmt.Sprintf(format, args...))
}

func (e *snapshotEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

func (e *snapshotEvents) index(event string) int {
	return slices.Index(e.list(), event)
}

// snapshotImages is an image manager fake with qcow2 internal snapshots.
type snapshotImages struct {
	mu     sync.Mutex
	sizes  map[string]uint64
	tags   map[string]map[string]uint64
	events *snapshotEvents
}

func (f *snapshotImages) ValidateTemplate(context.Context, string) (qemu.TemplateInfo, error) {
	return qemu.TemplateInfo{}, nil
}
func (f *snapshotImages) Info(_ context.Context, path string) (qemu.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	size, ok := f.sizes[path]
	if !ok {
		return qemu.ImageInfo{}, os.ErrNotExist
	}
	info := qemu.ImageInfo{Format: "qcow2", VirtualSize: size}
	var tags []string
	for tag := range f.tags[path] {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		info.Snapshots = append(info.Snapshots, qemu.ImageSnapshot{Name: tag})
	}
	return info, nil
}
func (f *snapshotImages) CreateOverlay(context.Context, string, qemu.TemplateInfo, uint64) error {
	return errors.New("unexpected overlay creation")
}
func (f *snapshotImages) CreateDataDisk(context.Context, string, uint64) error {
	return errors.New("unexpected disk creation")
}
func (f *snapshotImages) ResizeOffline(_ context.Context, g qemu.Growth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events.add("resize %s %d", filepath.Base(g.Path), g.Desired)
	f.sizes[g.Path] = g.Desired
	return nil
}
func (f *snapshotImages) ResizeOnline(context.Context, qemu.BlockResizer, qemu.Growth) error {
	return errors.New("unexpected online resize")
}
func (f *snapshotImages) addTag(path, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tags[path] == nil {
		f.tags[path] = map[string]uint64{}
	}
	if _, exists := f.tags[path][tag]; exists {
		return fmt.Errorf("snapshot %s exists in %s", tag, path)
	}
	f.tags[path][tag] = f.sizes[path]
	return nil
}
func (f *snapshotImages) SnapshotCreate(_ context.Context, path, tag string) error {
	f.events.add("qemu-img create %s %s", filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), tag)
	return f.addTag(path, tag)
}
func (f *snapshotImages) SnapshotApply(_ context.Context, path, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events.add("qemu-img apply %s %s", filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), tag)
	size, ok := f.tags[path][tag]
	if !ok {
		return fmt.Errorf("no snapshot %s in %s", tag, path)
	}
	f.sizes[path] = size
	return nil
}
func (f *snapshotImages) SnapshotDelete(_ context.Context, path, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events.add("qemu-img delete %s %s", filepath.Base(filepath.Dir(path))+"/"+filepath.Base(path), tag)
	delete(f.tags[path], tag)
	return nil
}

// fakeSnapshotQMP is the QMP of one VM in a snapshot fixture.
type fakeSnapshotQMP struct {
	f    *snapshotFixture
	node string
}

func (q *fakeSnapshotQMP) QueryStatus(context.Context) (string, error) {
	q.f.mu.Lock()
	defer q.f.mu.Unlock()
	if status, ok := q.f.status[q.node]; ok {
		return status, nil
	}
	return "running", nil
}
func (q *fakeSnapshotQMP) Pause(context.Context) error {
	q.f.events.add("pause %s", q.node)
	if err := q.f.failPause[q.node]; err != nil {
		return err
	}
	q.f.mu.Lock()
	q.f.status[q.node] = "paused"
	q.f.mu.Unlock()
	return nil
}
func (q *fakeSnapshotQMP) Resume(context.Context) error {
	q.f.events.add("resume %s", q.node)
	q.f.mu.Lock()
	q.f.status[q.node] = "running"
	q.f.mu.Unlock()
	return nil
}
func (q *fakeSnapshotQMP) SnapshotInternal(_ context.Context, name string, nodes []string) error {
	q.f.events.add("qmp snapshot %s %s %s", q.node, name, strings.Join(nodes, ","))
	if err := q.f.failSnapshot[q.node]; err != nil {
		return err
	}
	for _, block := range nodes {
		for _, d := range nodeDisks(q.f.m.Nodes[q.node]) {
			if d.BlockNode == block {
				if err := q.f.images.addTag(d.Path, name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (q *fakeSnapshotQMP) DeleteSnapshotInternal(_ context.Context, block, name string) error {
	q.f.events.add("qmp delete %s %s %s", q.node, block, name)
	for _, d := range nodeDisks(q.f.m.Nodes[q.node]) {
		if d.BlockNode == block {
			q.f.images.mu.Lock()
			delete(q.f.images.tags[d.Path], name)
			q.f.images.mu.Unlock()
		}
	}
	return nil
}
func (q *fakeSnapshotQMP) Close() error { return nil }

// fakeLVM is a stateful LVM command fake for one volume group.
type fakeLVM struct {
	mu     sync.Mutex
	vg     string
	lvs    map[string]*fakeLV
	next   int
	events *snapshotEvents
	// failOnce fails the next call of an LVM subcommand, such as lvrename.
	failOnce map[string]bool
}

type fakeLV struct {
	uuid, pool, origin, attr string
	size                     uint64
	tags                     []string
}

func (f *fakeLVM) add(name string, lv *fakeLV) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lvs[name] = lv
}

func (f *fakeLVM) get(name string) (fakeLV, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lv, ok := f.lvs[name]
	if !ok {
		return fakeLV{}, false
	}
	return *lv, true
}

func (f *fakeLVM) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnce[args[0]] {
		delete(f.failOnce, args[0])
		return nil, fmt.Errorf("injected %s failure", args[0])
	}
	if args[0] != "vgs" && args[0] != "lvs" {
		f.events.add("lvm %s", strings.Join(args, " "))
	}
	value := func(flag string) string {
		for i := range args[:len(args)-1] {
			if args[i] == flag {
				return args[i+1]
			}
		}
		return ""
	}
	lvName := func(path string) string { return strings.TrimPrefix(path, f.vg+"/") }
	switch args[0] {
	case "vgs":
		return []byte(fmt.Sprintf(`{"report":[{"vg":[{"vg_name":%q,"vg_uuid":"vg-id","vg_size":"107374182400","vg_free":"53687091200","pv_count":"1"}]}]}`, f.vg)), nil
	case "lvs":
		var rows []string
		var names []string
		for name := range f.lvs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			lv := f.lvs[name]
			rows = append(rows, fmt.Sprintf(`{"lv_name":%q,"lv_uuid":%q,"vg_name":%q,"vg_uuid":"vg-id","lv_size":"%d","pool_lv":%q,"origin":%q,"lv_attr":%q,"lv_tags":%q,"data_percent":"","metadata_percent":""}`, name, lv.uuid, f.vg, lv.size, lv.pool, lv.origin, lv.attr, strings.Join(lv.tags, ",")))
		}
		return []byte(`{"report":[{"lv":[` + strings.Join(rows, ",") + `]}]}`), nil
	case "lvcreate":
		if !slices.Contains(args, "--snapshot") {
			return nil, errors.New("fake lvcreate supports only --snapshot")
		}
		origin := f.lvs[lvName(args[len(args)-1])]
		name := value("--name")
		if origin == nil || f.lvs[name] != nil {
			return nil, fmt.Errorf("lvcreate %s: bad origin or existing name", name)
		}
		var tags []string
		for i, arg := range args {
			if arg == "--addtag" {
				tags = append(tags, args[i+1])
			}
		}
		f.next++
		f.lvs[name] = &fakeLV{uuid: fmt.Sprintf("snap-uuid-%d", f.next), pool: origin.pool, origin: lvName(args[len(args)-1]), attr: "Vwi---tz-k", size: origin.size, tags: tags}
	case "lvchange":
		target := f.lvs[lvName(args[len(args)-1])]
		if target == nil {
			return nil, errors.New("lvchange: no LV")
		}
		switch args[1] {
		case "--permission":
			target.attr = target.attr[:1] + "r" + target.attr[2:]
		case "--deltag":
			target.tags = slices.DeleteFunc(target.tags, func(tag string) bool { return tag == args[2] })
		}
	case "lvremove":
		name := lvName(args[len(args)-1])
		if f.lvs[name] == nil {
			return nil, errors.New("lvremove: no LV")
		}
		delete(f.lvs, name)
		for _, lv := range f.lvs {
			if lv.origin == name {
				lv.origin = ""
			}
		}
	case "lvrename":
		old, renamed := args[2], args[3]
		if f.lvs[old] == nil || f.lvs[renamed] != nil {
			return nil, errors.New("lvrename: bad names")
		}
		f.lvs[renamed] = f.lvs[old]
		delete(f.lvs, old)
		for _, lv := range f.lvs {
			if lv.origin == old {
				lv.origin = renamed
			}
		}
	case "lvextend":
		target := f.lvs[lvName(args[len(args)-1])]
		var size uint64
		fmt.Sscanf(strings.TrimSuffix(value("--size"), "B"), "%d", &size)
		target.size = size
	}
	return nil, nil
}

// snapshotFixture is a topology "lab" with VM "a" (running, guest agent,
// root and data file disks) and VM "b" (stopped, root file disk). Options add
// a running VM "t" with an lvm-thin root disk.
type snapshotFixture struct {
	t         *testing.T
	a         *App
	cfg       *config.Config
	layout    state.Layout
	m         *state.Manifest
	processes *lockedProcesses
	images    *snapshotImages
	lvm       *fakeLVM
	events    *snapshotEvents
	stdout    bytes.Buffer
	stderr    bytes.Buffer

	mu           sync.Mutex
	status       map[string]string
	failPause    map[string]error
	failSnapshot map[string]error
	failFreeze   map[string]error
	noAgent      map[string]bool
	nextPID      int
}

type snapshotVMSpec struct {
	name    string
	running bool
	agent   bool
	data    bool
	storage string
}

var snapshotFixtureTime = time.Date(2026, 11, 15, 10, 20, 31, 123_000_000, time.UTC)

func newSnapshotFixture(t *testing.T, specs ...snapshotVMSpec) *snapshotFixture {
	t.Helper()
	if len(specs) == 0 {
		specs = []snapshotVMSpec{{name: "a", running: true, agent: true, data: true, storage: "local"}, {name: "b", storage: "local"}}
	}
	events := &snapshotEvents{}
	f := &snapshotFixture{
		t:            t,
		processes:    &lockedProcesses{alive: map[int]bool{}, args: map[int][]string{}},
		images:       &snapshotImages{sizes: map[string]uint64{}, tags: map[string]map[string]uint64{}, events: events},
		lvm:          &fakeLVM{vg: "vg0", lvs: map[string]*fakeLV{}, events: events, failOnce: map[string]bool{}},
		events:       events,
		status:       map[string]string{},
		failPause:    map[string]error{},
		failSnapshot: map[string]error{},
		failFreeze:   map[string]error{},
		noAgent:      map[string]bool{},
		nextPID:      500,
	}
	f.layout, _ = shortLayout(t)
	local := &config.Storage{Name: "local", Type: "file", RootProvisioning: "snapshot"}
	thin := &config.Storage{Name: "thin", Type: "lvm-thin", VolumeGroup: "vg0", PoolSizeGiB: 4, RootProvisioning: "copy"}
	f.cfg = &config.Config{Name: "lab", Storages: []*config.Storage{local}}
	f.m = state.NewManifest("lab")
	f.m.Storages["local"] = &state.Storage{Name: "local", Type: "file", RootProvisioning: "snapshot"}
	pool := storagepkg.PoolName("lab", "thin")
	one := 1
	for i, spec := range specs {
		vm := &config.VM{Name: spec.name, Template: "ubuntu", CPU: &config.CPU{Model: "host", Sockets: &one, Cores: &one, Threads: &one}, MemoryMB: 128, MemoryBalloon: &config.MemoryBalloon{}, Boot: config.Boot{Order: []string{"disk"}}, RootDisk: config.RootDisk{Bus: "scsi", Storage: spec.storage}, GuestAgent: spec.agent}
		dir, _ := f.layout.NodeDir(spec.name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(bootstrapSnapshot{})
		n := &state.Node{Kind: "vm", StorageIdentityHash: "storage-" + spec.name, BootstrapHash: "bootstrap", BootstrapSnapshot: raw, TemplatePath: "/template.qcow2", SeedPath: filepath.Join(dir, "seed.iso"), OVMFVarsPath: filepath.Join(dir, "OVMF_VARS.fd"), PIDFile: filepath.Join(dir, "qemu.pid"), SerialLogPath: filepath.Join(f.layout.LogsDir, spec.name+".jsonl")}
		n.QMPPath, _ = f.layout.SocketPath(spec.name, "qmp")
		n.SerialSocketPath, _ = f.layout.SocketPath(spec.name, "serial")
		if spec.agent {
			n.GuestAgentPath, _ = f.layout.SocketPath(spec.name, "guest-agent")
		}
		if spec.storage == "thin" {
			if f.m.Storages["thin"] == nil {
				f.cfg.Storages = append(f.cfg.Storages, thin)
				f.m.Storages["thin"] = &state.Storage{Name: "thin", Type: "lvm-thin", RootProvisioning: "copy", VolumeGroup: "vg0", VGUUID: "vg-id", PoolName: pool, PoolUUID: "pool-id"}
				f.lvm.add(pool, &fakeLV{uuid: "pool-id", attr: "twi-a-t---", size: 4 << 30, tags: storagepkg.Tags("lab", "thin", "pool")})
			}
			lvName := storagepkg.DiskName("lab", spec.name, "root")
			n.RootDisk = diskRecord("root", storagepkg.DevicePath("vg0", lvName), "scsi", 2<<30, thin, true, "lab", spec.name)
			n.RootDisk.LVUUID, n.RootDisk.VGUUID, n.RootDisk.PoolUUID = "disk-uuid-"+spec.name, "vg-id", "pool-id"
			f.lvm.add(lvName, &fakeLV{uuid: n.RootDisk.LVUUID, pool: pool, attr: "Vwi-a-tz--", size: 2 << 30, tags: storagepkg.Tags("lab", "thin", "disk")})
		} else {
			n.RootDisk = diskRecord("root", filepath.Join(dir, "root.qcow2"), "scsi", 100, local, true, "lab", spec.name)
			f.addImage(n.RootDisk.Path, 100)
		}
		if spec.data {
			size := uint64(1)
			vm.AdditionalDisks = []*config.Disk{{Name: "data", Storage: "local", SizeGiB: size, Bus: "scsi"}}
			data := diskRecord("data", filepath.Join(dir, "disk-"+state.ShortID("data", 12)+".qcow2"), "scsi", 50, local, false, "lab", spec.name)
			n.AdditionalDisks = []state.Disk{*data}
			f.addImage(data.Path, 50)
		}
		if err := os.WriteFile(n.OVMFVarsPath, []byte("vars-"+spec.name), 0o600); err != nil {
			t.Fatal(err)
		}
		if spec.running {
			pid := 100 + i
			n.PID = pid
			f.processes.alive[pid] = true
			f.processes.args[pid] = []string{"qemu-system-x86_64", "-name", "guest=topolab:lab:" + spec.name + ",debug-threads=on"}
		}
		f.cfg.VMs = append(f.cfg.VMs, vm)
		f.m.Nodes[spec.name] = n
	}
	netState := &network.State{Topology: "lab", TAPs: map[string]network.TAPState{}}
	f.m.NetworkState, _ = json.Marshal(netState)
	if err := state.Save(f.layout.ManifestPath, f.m); err != nil {
		t.Fatal(err)
	}
	var qmpCalls []string
	a := New(&f.stdout, &f.stderr)
	a.Network = checkpointNetwork{netState}
	a.Namespaces = &recordingNamespaceOps{}
	a.Processes = f.processes
	a.Pidfds = &fakePidfds{events: &hardStopEvents{}, processes: f.processes, hang: map[int]bool{}, gone: map[int]bool{}, signals: map[int]int{}}
	a.Stopper = failingStopper{t: t}
	a.Images = f.images
	a.LVM = storagepkg.NewLVM(f.lvm, "lvm")
	a.Seeds = checkpointSeeds{}
	a.Now = func() time.Time { return snapshotFixtureTime }
	a.OpenSnapshotQMP = func(path string) snapshotQMP { return &fakeSnapshotQMP{f: f, node: f.nodeForQMP(path)} }
	a.GuestAgentProbe = func(_ context.Context, path string) bool { return !f.noAgent[f.nodeForAgent(path)] }
	a.GuestAgentExec = func(_ context.Context, path, command string) (json.RawMessage, error) {
		node := f.nodeForAgent(path)
		f.events.add("agent %s %s", node, command)
		if command == "guest-fsfreeze-freeze" {
			if err := f.failFreeze[node]; err != nil {
				return nil, err
			}
		}
		return json.RawMessage("1"), nil
	}
	a.SyncBlockDevice = func(path string) error {
		f.events.add("sync %s", path)
		return nil
	}
	a.Executable = func() (string, error) { return "/topolab", nil }
	a.StartProcess = func(argv []string, _ []*os.File) (*qemu.StartedProcess, error) {
		f.mu.Lock()
		f.nextPID++
		pid := f.nextPID
		f.mu.Unlock()
		f.processes.mu.Lock()
		f.processes.alive[pid] = true
		f.processes.args[pid] = argv
		f.processes.mu.Unlock()
		if slices.Contains(argv, "__serial-collector") {
			f.events.add("start collector %s", argv[len(argv)-1])
		} else {
			f.events.add("start qemu %d", pid)
		}
		return &qemu.StartedProcess{PID: pid}, nil
	}
	a.OpenQMP = func(string) qmpClient { return orderedQMP{calls: &qmpCalls} }
	a.VhostAvailable = func() bool { return false }
	a.defaults()
	f.a = a
	return f
}

func (f *snapshotFixture) addImage(path string, size uint64) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte("qcow2"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.images.sizes[path] = size
}

func (f *snapshotFixture) nodeForQMP(path string) string {
	for name, n := range f.m.Nodes {
		if n.QMPPath == path {
			return name
		}
	}
	return "unknown"
}

func (f *snapshotFixture) nodeForAgent(path string) string {
	for name, n := range f.m.Nodes {
		if n.GuestAgentPath == path {
			return name
		}
	}
	return "unknown"
}

func (f *snapshotFixture) create(args ...string) error {
	f.stdout.Reset()
	return f.a.snapshotCreate(context.Background(), f.cfg, f.layout, f.m, args)
}

func (f *snapshotFixture) restore(args ...string) error {
	f.stdout.Reset()
	return f.a.snapshotRestore(context.Background(), f.cfg, f.layout, f.m, args)
}

func (f *snapshotFixture) delete(args ...string) error {
	f.stdout.Reset()
	return f.a.snapshotDelete(context.Background(), f.cfg, f.layout, f.m, args)
}

// requireValid checks that the manifest on disk is the one in memory and
// passes the checks that every runtime command makes.
func (f *snapshotFixture) requireValid() {
	f.t.Helper()
	saved, err := state.Load(f.layout.ManifestPath)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := validateManifestPaths(f.layout, saved); err != nil {
		f.t.Fatalf("saved manifest is invalid: %v", err)
	}
	if err := f.a.verifyManifestStorage(context.Background(), saved, false); err != nil {
		f.t.Fatalf("saved manifest storage check: %v", err)
	}
}

func (f *snapshotFixture) tags(path string) []string {
	info, err := f.images.Info(context.Background(), path)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, s := range info.Snapshots {
		out = append(out, s.Name)
	}
	return out
}

func TestSnapshotCreateFreezesPausesAllRunningVMsAndSavesStoppedVMs(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := f.create("base"); err != nil {
		t.Fatalf("create: %v\nstderr: %s", err, f.stderr.String())
	}
	if got := f.stdout.String(); got != "NAME\tSTATE\tCONSISTENCY\na\trunning\tfrozen\nb\tstopped\tstopped\n" {
		t.Fatalf("output:\n%s", got)
	}
	record := f.m.Snapshots["base"]
	if record == nil || record.Pending != "" || !record.CreatedAt.Equal(snapshotFixtureTime) {
		t.Fatalf("record = %#v", record)
	}
	a, b := f.m.Nodes["a"], f.m.Nodes["b"]
	order := []string{
		"qemu-img create b/root.qcow2 base",
		"agent a guest-fsfreeze-freeze",
		"pause a",
		"qmp snapshot a base " + a.RootDisk.BlockNode + "," + a.AdditionalDisks[0].BlockNode,
		"resume a",
		"agent a guest-fsfreeze-thaw",
	}
	last := -1
	for _, event := range order {
		i := f.events.index(event)
		if i <= last {
			t.Fatalf("event %q is missing or out of order: %v", event, f.events.list())
		}
		last = i
	}
	for _, path := range []string{a.RootDisk.Path, a.AdditionalDisks[0].Path, b.RootDisk.Path} {
		if !slices.Equal(f.tags(path), []string{"base"}) {
			t.Errorf("%s tags = %v", path, f.tags(path))
		}
	}
	for _, name := range []string{"a", "b"} {
		vm := record.VMs[name]
		data, err := os.ReadFile(vm.OVMFVarsPath)
		if err != nil || string(data) != "vars-"+name || vm.OVMFVarsDigest == "" {
			t.Errorf("VM %s vars copy = %q, %v, digest %q", name, data, err, vm.OVMFVarsDigest)
		}
	}
	if !record.VMs["a"].Running || record.VMs["b"].Running {
		t.Fatalf("running flags = %#v", record.VMs)
	}
	f.requireValid()
	if err := f.create("base"); err == nil || !strings.Contains(err.Error(), `snapshot "base" already exists`) {
		t.Fatalf("second create error = %v", err)
	}
}

func TestSnapshotCreateWithFailedFreezeIsCrashConsistentAndStillThaws(t *testing.T) {
	f := newSnapshotFixture(t)
	f.failFreeze["a"] = errors.New("busy")
	if err := f.create("base", "a"); err != nil {
		t.Fatal(err)
	}
	if got := f.m.Snapshots["base"].VMs["a"].Consistency; got != state.SnapshotCrashConsistent {
		t.Fatalf("consistency = %q", got)
	}
	if !strings.Contains(f.stderr.String(), "warning: a: the guest agent did not freeze the file systems; the snapshot of a is crash-consistent") {
		t.Fatalf("stderr = %q", f.stderr.String())
	}
	if f.events.index("agent a guest-fsfreeze-thaw") < 0 {
		t.Fatalf("no thaw after a failed freeze: %v", f.events.list())
	}
	if f.m.Snapshots["base"].VMs["b"] != nil {
		t.Fatal("VM b is in a snapshot of only VM a")
	}
}

func TestSnapshotCreateWithoutResponsiveAgentSkipsFreeze(t *testing.T) {
	f := newSnapshotFixture(t)
	f.noAgent["a"] = true
	if err := f.create("base", "a"); err != nil {
		t.Fatal(err)
	}
	for _, event := range f.events.list() {
		if strings.HasPrefix(event, "agent ") {
			t.Fatalf("unexpected guest-agent call %q", event)
		}
	}
	if !strings.Contains(f.stderr.String(), "the guest agent does not answer") || f.m.Snapshots["base"].VMs["a"].Consistency != state.SnapshotCrashConsistent {
		t.Fatalf("stderr = %q, record = %#v", f.stderr.String(), f.m.Snapshots["base"].VMs["a"])
	}
}

func TestSnapshotCreateFailureResumesThawsAndRemovesEverything(t *testing.T) {
	f := newSnapshotFixture(t)
	f.failSnapshot["a"] = errors.New("injected QMP failure")
	err := f.create("base")
	if err == nil || !strings.Contains(err.Error(), "injected QMP failure") {
		t.Fatalf("err = %v", err)
	}
	for _, event := range []string{"resume a", "agent a guest-fsfreeze-thaw", "qemu-img delete b/root.qcow2 base"} {
		if f.events.index(event) < 0 {
			t.Errorf("missing event %q: %v", event, f.events.list())
		}
	}
	if f.m.Snapshots["base"] != nil {
		t.Fatalf("failed snapshot stays recorded: %#v", f.m.Snapshots["base"])
	}
	if tags := f.tags(f.m.Nodes["b"].RootDisk.Path); len(tags) != 0 {
		t.Fatalf("stopped VM keeps tags %v", tags)
	}
	dir, _ := f.layout.SnapshotDir("base")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("snapshot directory remains: %v", err)
	}
	f.requireValid()
}

func TestSnapshotCreatePauseFailureTakesNoSnapshot(t *testing.T) {
	f := newSnapshotFixture(t,
		snapshotVMSpec{name: "a", running: true, storage: "local"},
		snapshotVMSpec{name: "c", running: true, storage: "local"},
	)
	f.failPause["c"] = errors.New("injected pause failure")
	if err := f.create("base"); err == nil || !strings.Contains(err.Error(), "injected pause failure") {
		t.Fatalf("err = %v", err)
	}
	for _, event := range f.events.list() {
		if strings.HasPrefix(event, "qmp snapshot") {
			t.Fatalf("a snapshot was taken after a failed pause: %v", f.events.list())
		}
	}
	if f.events.index("resume a") < 0 || f.events.index("resume c") >= 0 {
		t.Fatalf("resume events: %v", f.events.list())
	}
}

func TestSnapshotCreateRejectsInvalidInputBeforeChanges(t *testing.T) {
	f := newSnapshotFixture(t)
	for _, test := range []struct {
		args []string
		want string
	}{
		{nil, "snapshot create requires NAME"},
		{[]string{"Base"}, `invalid snapshot name "Base"`},
		{[]string{"base", "missing"}, `unknown VM "missing"`},
		{[]string{"base", "--hard"}, `unknown option "--hard"`},
	} {
		if err := f.create(test.args...); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("create %q error = %v, want %q", test.args, err, test.want)
		}
	}
	if err := f.images.addTag(f.m.Nodes["b"].RootDisk.Path, "base"); err != nil {
		t.Fatal(err)
	}
	if err := f.create("base"); err == nil || !strings.Contains(err.Error(), "already has an internal snapshot") {
		t.Fatalf("existing tag error = %v", err)
	}
	if len(f.events.list()) != 0 || len(f.m.Snapshots) != 0 {
		t.Fatalf("a rejected create changed state: %v %#v", f.events.list(), f.m.Snapshots)
	}
}

func TestSnapshotCreateRejectsThickDisk(t *testing.T) {
	f := newSnapshotFixture(t, snapshotVMSpec{name: "a", storage: "local"})
	thick := &config.Storage{Name: "thick", Type: "lvm-thick", VolumeGroup: "vg0"}
	f.cfg.Storages = append(f.cfg.Storages, thick)
	f.m.Storages["thick"] = &state.Storage{Name: "thick", Type: "lvm-thick", VolumeGroup: "vg0", VGUUID: "vg-id"}
	n := f.m.Nodes["a"]
	lvName := storagepkg.DiskName("lab", "a", "data")
	data := diskRecord("data", storagepkg.DevicePath("vg0", lvName), "scsi", 1<<30, thick, false, "lab", "a")
	data.LVUUID, data.VGUUID = "thick-uuid", "vg-id"
	n.AdditionalDisks = []state.Disk{*data}
	f.lvm.add(lvName, &fakeLV{uuid: "thick-uuid", attr: "-wi-a-----", size: 1 << 30, tags: storagepkg.Tags("lab", "thick", "disk")})
	err := f.create("base")
	if err == nil || !strings.Contains(err.Error(), "disk data of VM a uses lvm-thick storage thick; snapshots support only file and lvm-thin storage") {
		t.Fatalf("err = %v", err)
	}
	if len(f.m.Snapshots) != 0 {
		t.Fatal("a rejected create was recorded")
	}
}

func TestSnapshotRestoreKeepsRunStateAndGrowsDisks(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	// The data disk of VM a grows after the snapshot.
	a := f.m.Nodes["a"]
	f.images.sizes[a.AdditionalDisks[0].Path] = 80
	a.AdditionalDisks[0].CapacityBytes = 80
	if err := os.WriteFile(a.OVMFVarsPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.restore("base"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := f.stdout.String(); got != "NAME\tBEFORE\tRESULT\tAFTER\na\trunning\trestored\trunning\nb\tstopped\trestored\tstopped\n" {
		t.Fatalf("output:\n%s", got)
	}
	if f.processes.Alive(100) {
		t.Fatal("the old QEMU process of VM a still runs")
	}
	if a.PID == 0 || !f.processes.Alive(a.PID) || f.m.Nodes["b"].PID != 0 {
		t.Fatalf("PIDs after restore: a=%d b=%d", a.PID, f.m.Nodes["b"].PID)
	}
	for _, event := range []string{"qemu-img apply a/root.qcow2 base", "qemu-img apply b/root.qcow2 base", "resize " + filepath.Base(a.AdditionalDisks[0].Path) + " 80"} {
		if f.events.index(event) < 0 {
			t.Errorf("missing event %q: %v", event, f.events.list())
		}
	}
	if data, _ := os.ReadFile(a.OVMFVarsPath); string(data) != "vars-a" {
		t.Fatalf("OVMF variables = %q", data)
	}
	record := f.m.Snapshots["base"]
	if record.Restore != nil || record.Pending != "" {
		t.Fatalf("restore stays pending: %#v", record)
	}
	f.requireValid()
}

func TestSnapshotRestoreUsesTopologyOrder(t *testing.T) {
	f := newSnapshotFixture(t,
		snapshotVMSpec{name: "zeta", storage: "local"},
		snapshotVMSpec{name: "alpha", storage: "local"},
	)
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	if got := f.stdout.String(); got != "NAME\tSTATE\tCONSISTENCY\nzeta\tstopped\tstopped\nalpha\tstopped\tstopped\n" {
		t.Fatalf("create output:\n%s", got)
	}
	if err := f.restore("base"); err != nil {
		t.Fatal(err)
	}
	if got := f.stdout.String(); got != "NAME\tBEFORE\tRESULT\tAFTER\nzeta\tstopped\trestored\tstopped\nalpha\tstopped\trestored\tstopped\n" {
		t.Fatalf("restore output:\n%s", got)
	}
}

func TestSnapshotRestoreOfOneVMLeavesOthersRunning(t *testing.T) {
	f := newSnapshotFixture(t,
		snapshotVMSpec{name: "a", running: true, storage: "local"},
		snapshotVMSpec{name: "c", running: true, storage: "local"},
	)
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	if err := f.restore("base", "c"); err != nil {
		t.Fatal(err)
	}
	if !f.processes.Alive(100) || f.m.Nodes["a"].PID != 100 {
		t.Fatal("VM a was stopped by a restore of VM c")
	}
	if f.events.index("qemu-img apply a/root.qcow2 base") >= 0 || f.events.index("qemu-img apply c/root.qcow2 base") < 0 {
		t.Fatalf("events: %v", f.events.list())
	}
	if err := f.restore("base", "missing"); err == nil || !strings.Contains(err.Error(), "VM missing is not in snapshot base") {
		t.Fatalf("err = %v", err)
	}
}

func TestSnapshotRestoreRejectsChangedVMBeforeStoppingIt(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	f.m.Nodes["a"].StorageIdentityHash = "replaced"
	if err := f.restore("base"); err == nil || !strings.Contains(err.Error(), "the root disk of VM a changed after snapshot base") {
		t.Fatalf("err = %v", err)
	}
	if !f.processes.Alive(100) || f.m.Snapshots["base"].Restore != nil {
		t.Fatal("a rejected restore stopped a VM or was recorded")
	}
	f.m.Nodes["a"].StorageIdentityHash = "storage-a"
	f.m.Nodes["a"].AdditionalDisks = nil
	if err := f.restore("base"); err == nil || !strings.Contains(err.Error(), "the disks of VM a changed") {
		t.Fatalf("err = %v", err)
	}
}

func TestSnapshotThinCreateRestoreAndDelete(t *testing.T) {
	f := newSnapshotFixture(t, snapshotVMSpec{name: "t", running: true, storage: "thin"})
	disk := f.m.Nodes["t"].RootDisk
	snapLV := storagepkg.SnapshotLVName("lab", "t", "root", "base")
	if err := f.create("base"); err != nil {
		t.Fatalf("create: %v", err)
	}
	pause, sync, lvcreate, resume := f.events.index("pause t"), f.events.index("sync "+disk.Path), -1, f.events.index("resume t")
	for i, event := range f.events.list() {
		if strings.HasPrefix(event, "lvm lvcreate --yes --snapshot --name "+snapLV) {
			lvcreate = i
		}
	}
	if !(pause >= 0 && pause < sync && sync < lvcreate && lvcreate < resume) {
		t.Fatalf("thin snapshot is not taken while paused after a flush: %v", f.events.list())
	}
	lv, ok := f.lvm.get(snapLV)
	sd := f.m.Snapshots["base"].VMs["t"].Disks[0]
	if !ok || lv.attr[1] != 'r' || lv.origin != disk.LVName || sd.LVUUID != lv.uuid || !storagepkg.HasTags(lv.tags, storagepkg.Tags("lab", "thin", "snapshot")) {
		t.Fatalf("snapshot LV = %#v, record = %#v", lv, sd)
	}
	f.requireValid()

	// The disk grows after the snapshot. The restore keeps the new size.
	disk.CapacityBytes = 3 << 30
	f.lvm.lvs[disk.LVName].size = 3 << 30
	if err := f.restore("base"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, ok := f.lvm.get(disk.LVName)
	if !ok || restored.uuid == "disk-uuid-t" || restored.uuid != disk.LVUUID || restored.origin != snapLV || restored.size != 3<<30 {
		t.Fatalf("restored LV = %#v, disk = %#v", restored, disk)
	}
	if storagepkg.HasTags(restored.tags, []string{"topolab.kind.staging"}) || !storagepkg.HasTags(restored.tags, storagepkg.Tags("lab", "thin", "disk")) {
		t.Fatalf("restored LV tags = %v", restored.tags)
	}
	if disk.OriginName != snapLV || disk.OriginUUID != sd.LVUUID || disk.Pending != "" || disk.StagingName != "" {
		t.Fatalf("disk record = %#v", disk)
	}
	if _, staged := f.lvm.get(storagepkg.StagingName("lab", "t", "root")); staged {
		t.Fatal("the staging LV remains")
	}
	if !f.processes.Alive(f.m.Nodes["t"].PID) {
		t.Fatal("VM t does not run after the restore")
	}
	f.requireValid()

	// A delete removes the snapshot LV, and the disk loses that origin.
	if err := f.delete("base"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := f.lvm.get(snapLV); ok || disk.OriginName != "" || disk.OriginUUID != "" || len(f.m.Snapshots) != 0 {
		t.Fatalf("after delete: LV present=%t disk=%#v snapshots=%#v", ok, disk, f.m.Snapshots)
	}
	if current, _ := f.lvm.get(disk.LVName); current.origin != "" {
		t.Fatalf("disk LV origin = %q", current.origin)
	}
	f.requireValid()
}

func TestSnapshotInterruptedThinRestoreBlocksStartAndCompletesOnRetry(t *testing.T) {
	f := newSnapshotFixture(t, snapshotVMSpec{name: "t", running: true, storage: "thin"})
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	f.lvm.failOnce["lvrename"] = true
	err := f.restore("base")
	if err == nil || !strings.Contains(err.Error(), "injected lvrename failure") || !strings.Contains(err.Error(), "run topolab snapshot restore base again") {
		t.Fatalf("err = %v", err)
	}
	r := f.m.Snapshots["base"].Restore
	if r == nil || r.Phase != state.SnapshotRestorePhaseRestore || !slices.Equal(r.Running, []string{"t"}) {
		t.Fatalf("restore record = %#v", r)
	}
	f.requireValid()
	for _, check := range []func() error{
		func() error { return f.a.start(context.Background(), f.cfg, f.layout, f.m, nil) },
		func() error { return f.a.restart(context.Background(), f.cfg, f.layout, f.m, nil) },
		func() error { return validateSnapshotsForUp(f.m, map[string]*state.Node{"t": f.m.Nodes["t"]}) },
		func() error { return f.create("other") },
		func() error { return f.delete("base") },
	} {
		if err := check(); err == nil || !strings.Contains(err.Error(), "the restore of snapshot base is incomplete") {
			t.Errorf("command during an incomplete restore: %v", err)
		}
	}
	if err := f.restore("base"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := f.stdout.String(); got != "NAME\tBEFORE\tRESULT\tAFTER\nt\trunning\trestored\trunning\n" {
		t.Fatalf("retry output:\n%s", got)
	}
	disk := f.m.Nodes["t"].RootDisk
	if lv, ok := f.lvm.get(disk.LVName); !ok || lv.uuid != disk.LVUUID || disk.Pending != "" {
		t.Fatalf("disk after retry = %#v, LV = %#v", disk, lv)
	}
	f.requireValid()
}

func TestSnapshotDeleteOneVMThenAll(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	a, b := f.m.Nodes["a"], f.m.Nodes["b"]
	if err := f.delete("base", "b"); err != nil {
		t.Fatal(err)
	}
	if f.m.Snapshots["base"].VMs["b"] != nil || f.m.Snapshots["base"].VMs["a"] == nil || len(f.tags(b.RootDisk.Path)) != 0 {
		t.Fatalf("after partial delete: %#v", f.m.Snapshots["base"])
	}
	if err := f.delete("base"); err != nil {
		t.Fatal(err)
	}
	if f.events.index("qmp delete a "+a.RootDisk.BlockNode+" base") < 0 || len(f.tags(a.RootDisk.Path)) != 0 {
		t.Fatalf("running VM tags were not deleted through QMP: %v", f.events.list())
	}
	if len(f.m.Snapshots) != 0 {
		t.Fatalf("snapshots = %#v", f.m.Snapshots)
	}
	dir, _ := f.layout.SnapshotDir("base")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("snapshot directory remains: %v", err)
	}
	if err := f.delete("base"); err == nil || !strings.Contains(err.Error(), `snapshot "base" does not exist`) {
		t.Fatalf("err = %v", err)
	}
	f.requireValid()
}

func TestSnapshotDeleteOfInterruptedCreateResumesPausedVM(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	f.m.Snapshots["base"].Pending = state.SnapshotPendingCreate
	f.status["a"] = "paused"
	if err := f.restore("base"); err == nil || !strings.Contains(err.Error(), "snapshot base is incomplete; run topolab snapshot delete base") {
		t.Fatalf("restore of incomplete snapshot: %v", err)
	}
	if err := f.delete("base"); err != nil {
		t.Fatal(err)
	}
	if f.status["a"] != "running" || !strings.Contains(f.stderr.String(), "VM a continues after the interrupted snapshot create") {
		t.Fatalf("status = %q, stderr = %q", f.status["a"], f.stderr.String())
	}
}

func TestSnapshotsBlockDestructiveUp(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	clone := func(name string) *state.Node {
		n := *f.m.Nodes[name]
		n.AdditionalDisks = slices.Clone(n.AdditionalDisks)
		return &n
	}
	desired := map[string]*state.Node{"a": clone("a"), "b": clone("b"), "new": {StorageIdentityHash: "x"}}
	if err := validateSnapshotsForUp(f.m, desired); err != nil {
		t.Fatalf("adding a VM was refused: %v", err)
	}
	for _, test := range []struct {
		change func(map[string]*state.Node)
		want   string
	}{
		{func(d map[string]*state.Node) { delete(d, "b") }, `up would remove VM b, which snapshot "base" contains; run "topolab snapshot delete base b" first`},
		{func(d map[string]*state.Node) { d["a"].StorageIdentityHash = "new-template" }, `up would replace the root disk of VM a`},
		{func(d map[string]*state.Node) { d["a"].AdditionalDisks = nil }, `up would remove disk data of VM a`},
	} {
		changed := map[string]*state.Node{"a": clone("a"), "b": clone("b")}
		test.change(changed)
		if err := validateSnapshotsForUp(f.m, changed); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("error = %v, want %q", err, test.want)
		}
	}
}

func TestSnapshotListTableAndJSON(t *testing.T) {
	f := newSnapshotFixture(t)
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	f.stdout.Reset()
	if err := f.a.snapshotList(f.m, nil); err != nil {
		t.Fatal(err)
	}
	want := "NAME\tSTATE\tCREATED\tVMS\tCONSISTENCY\nbase\tready\t2026-11-15T10:20:31.123Z\t2\t1 frozen, 1 stopped\n"
	if f.stdout.String() != want {
		t.Fatalf("table:\n%s", f.stdout.String())
	}
	f.stdout.Reset()
	if err := f.a.snapshotList(f.m, []string{"--format", "json"}); err != nil {
		t.Fatal(err)
	}
	var out snapshotListOutput
	if err := json.Unmarshal(f.stdout.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.SchemaVersion != 1 || out.Topology != "lab" || len(out.Snapshots) != 1 || out.Snapshots[0].State != "ready" || len(out.Snapshots[0].VMs) != 2 || len(out.Snapshots[0].VMs[0].Disks) != 2 {
		t.Fatalf("json = %#v", out)
	}
	for _, args := range [][]string{{"--format"}, {"--format", "yaml"}, {"--format", "json", "--format", "table"}, {"extra"}} {
		if err := f.a.snapshotList(f.m, args); err == nil {
			t.Errorf("list %q was accepted", args)
		}
	}
}

func TestSnapshotManifestValidationRejectsUnsafeRecords(t *testing.T) {
	f := newSnapshotFixture(t, snapshotVMSpec{name: "t", storage: "thin"})
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		change func(*state.Snapshot)
		want   string
	}{
		{func(s *state.Snapshot) { s.VMs["t"].OVMFVarsPath = "/etc/passwd" }, "unsafe OVMF variables path"},
		{func(s *state.Snapshot) { s.VMs["t"].Disks[0].LVName = "tld-other" }, "inconsistent LV identity"},
		{func(s *state.Snapshot) { s.VMs["t"].Disks[0].OwnershipTags = storagepkg.Tags("lab", "thin", "disk") }, "inconsistent LV identity"},
		{func(s *state.Snapshot) { s.Pending = "unknown" }, "invalid pending operation"},
		{func(s *state.Snapshot) {
			s.Restore = &state.SnapshotRestore{Phase: state.SnapshotRestorePhaseRestore, VMs: []string{"x"}}
		}, `restores unknown VM "x"`},
	} {
		saved, err := state.Load(f.layout.ManifestPath)
		if err != nil {
			t.Fatal(err)
		}
		test.change(saved.Snapshots["base"])
		if err := validateManifestPaths(f.layout, saved); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("error = %v, want %q", err, test.want)
		}
	}
}

func TestDownRemovesSnapshotLVsAndFiles(t *testing.T) {
	f := newSnapshotFixture(t, snapshotVMSpec{name: "t", storage: "thin"}, snapshotVMSpec{name: "b", storage: "local"})
	if err := f.create("base"); err != nil {
		t.Fatal(err)
	}
	vars := f.m.Snapshots["base"].VMs["t"].OVMFVarsPath
	if err := verifyTopologyTree(f.layout, f.m); err != nil {
		t.Fatalf("down refuses the snapshot files: %v", err)
	}
	if err := f.a.removeAllSnapshots(context.Background(), f.layout, f.m); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.lvm.get(storagepkg.SnapshotLVName("lab", "t", "root", "base")); ok {
		t.Fatal("snapshot LV remains")
	}
	if _, err := os.Stat(vars); !os.IsNotExist(err) || len(f.m.Snapshots) != 0 {
		t.Fatalf("vars copy: %v, snapshots: %#v", err, f.m.Snapshots)
	}
}
