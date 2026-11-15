package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
)

// hardStopEvents records the order of exit-handle calls across goroutines.
type hardStopEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *hardStopEvents) add(event string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
}

func (e *hardStopEvents) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

// fakePidfds is a qemu.PidfdOps fake. Terminate ends the process unless the
// PID hangs. A gone PID ends before its Terminate call.
type fakePidfds struct {
	events    *hardStopEvents
	processes *lockedProcesses
	hang      map[int]bool
	gone      map[int]bool

	mu      sync.Mutex
	signals map[int]int
}

func (f *fakePidfds) Open(pid int) (qemu.ExitHandle, error) {
	f.events.add(fmt.Sprintf("open %d", pid))
	return &fakeExitHandle{pid: pid, f: f}, nil
}

func (f *fakePidfds) signalCount(pid int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signals[pid]
}

type fakeExitHandle struct {
	pid int
	f   *fakePidfds
}

func (h *fakeExitHandle) Terminate() error {
	h.f.events.add(fmt.Sprintf("terminate %d", h.pid))
	h.f.mu.Lock()
	h.f.signals[h.pid]++
	h.f.mu.Unlock()
	if h.f.gone[h.pid] {
		h.f.processes.kill(h.pid)
		return qemu.ErrProcessExited
	}
	if !h.f.hang[h.pid] {
		h.f.processes.kill(h.pid)
	}
	return nil
}

func (h *fakeExitHandle) Exited() bool { return !h.f.processes.Alive(h.pid) }

func (h *fakeExitHandle) WaitExit(context.Context, time.Duration) (bool, error) {
	h.f.events.add(fmt.Sprintf("wait %d", h.pid))
	return !h.f.processes.Alive(h.pid), nil
}

func (h *fakeExitHandle) Close() error { return nil }

// qemuSignalGuard fails the test when anything signals a QEMU PID through
// qemu.ProcessOps. The serial collector may still get its interrupt.
type qemuSignalGuard struct {
	*lockedProcesses
	t    *testing.T
	qemu map[int]bool
}

func (g qemuSignalGuard) Signal(pid int, signal os.Signal) error {
	if g.qemu[pid] {
		g.t.Errorf("Processes.Signal(%d, %v) was called for a QEMU process", pid, signal)
		return errors.New("unexpected QEMU signal")
	}
	return g.lockedProcesses.Signal(pid, signal)
}

// failingStopper fails the test when the normal graceful stop runs.
type failingStopper struct{ t *testing.T }

func (s failingStopper) Stop(_ context.Context, pid int, _, node, _ string) error {
	s.t.Errorf("normal Stopper.Stop(%d, %s) was called", pid, node)
	return errors.New("unexpected graceful stop")
}

// stepClock returns start, start+step, start+2*step, ...
type stepClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	c.now = c.now.Add(c.step)
	return now
}

var hardStopStart = time.Date(2026, 11, 14, 10, 0, 0, 123_000_000, time.UTC)

type hardStopFixture struct {
	app       *App
	layout    state.Layout
	manifest  *state.Manifest
	config    *config.Config
	processes *lockedProcesses
	pidfds    *fakePidfds
	events    *hardStopEvents
	stdout    *bytes.Buffer
}

// newHardStopFixture builds a topology "lab" with one running VM per name.
// The PIDs are 100, 101, ... in name order.
func newHardStopFixture(t *testing.T, names ...string) *hardStopFixture {
	t.Helper()
	layout, _ := shortLayout(t)
	m, processes := downManifest(t, layout, names...)
	cfg := &config.Config{Name: "lab"}
	qemuPIDs := map[int]bool{}
	for i, name := range names {
		pid := 100 + i
		processes.args[pid] = []string{"qemu-system-x86_64", "-name", "guest=topolab:lab:" + name + ",debug-threads=on"}
		qemuPIDs[pid] = true
		cfg.VMs = append(cfg.VMs, &config.VM{Name: name})
	}
	events := &hardStopEvents{}
	pidfds := &fakePidfds{events: events, processes: processes, hang: map[int]bool{}, gone: map[int]bool{}, signals: map[int]int{}}
	var stdout bytes.Buffer
	a := New(&stdout, nil)
	a.Processes = qemuSignalGuard{lockedProcesses: processes, t: t, qemu: qemuPIDs}
	a.Stopper = failingStopper{t: t}
	a.Pidfds = pidfds
	a.Now = (&stepClock{now: hardStopStart, step: time.Millisecond}).Now
	a.defaults()
	return &hardStopFixture{app: a, layout: layout, manifest: m, config: cfg, processes: processes, pidfds: pidfds, events: events, stdout: &stdout}
}

func (f *hardStopFixture) run(args ...string) error {
	hard, names, err := splitStopArgs(args)
	if err != nil {
		return err
	}
	return f.app.stop(context.Background(), f.config, f.layout, f.manifest, hard, names)
}

// rows returns the output rows after the header, split into columns.
func (f *hardStopFixture) rows(t *testing.T) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(f.stdout.String(), "\n"), "\n")
	if len(lines) == 0 || lines[0] != "NAME\tRESULT\tPID\tSTOP_TIME\tEXIT_TIME" {
		t.Fatalf("output has no header:\n%s", f.stdout.String())
	}
	rows := [][]string{}
	for _, line := range lines[1:] {
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows
}

func TestSplitStopArgs(t *testing.T) {
	for _, test := range []struct {
		args  []string
		hard  bool
		names []string
		err   bool
	}{
		{nil, false, []string{}, false},
		{[]string{"vm1", "vm2"}, false, []string{"vm1", "vm2"}, false},
		{[]string{"--hard"}, true, []string{}, false},
		{[]string{"vm1", "--hard", "vm2"}, true, []string{"vm1", "vm2"}, false},
		{[]string{"vm1", "--hard"}, true, []string{"vm1"}, false},
		{[]string{"--hard", "--hard"}, false, nil, true},
		// Other arguments stay VM names, so the normal stop keeps its errors.
		{[]string{"--soft"}, false, []string{"--soft"}, false},
	} {
		hard, names, err := splitStopArgs(test.args)
		if (err != nil) != test.err || hard != test.hard || !slices.Equal(names, test.names) {
			t.Errorf("splitStopArgs(%q) = %t, %q, %v", test.args, hard, names, err)
		}
	}
}

func TestHardStopWithoutNamesStopsAllVMs(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2", "vm3")
	if err := f.run("--hard"); err != nil {
		t.Fatal(err)
	}
	for pid := 100; pid <= 102; pid++ {
		if got := f.pidfds.signalCount(pid); got != 1 {
			t.Errorf("PID %d got %d SIGTERM calls, want 1", pid, got)
		}
		if f.processes.Alive(pid) {
			t.Errorf("PID %d is still alive", pid)
		}
	}
	var names []string
	for _, row := range f.rows(t) {
		names = append(names, row[0])
		if row[1] != hardStopStopped {
			t.Errorf("row %q, want result stopped", row)
		}
	}
	if !slices.Equal(names, []string{"vm1", "vm2", "vm3"}) {
		t.Fatalf("rows = %q, want the YAML order", names)
	}
}

func TestHardStopWithNamesStopsOnlyThoseVMs(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2", "vm3")
	if err := f.run("vm3", "--hard", "vm1", "vm3"); err != nil {
		t.Fatal(err)
	}
	if !f.processes.Alive(101) || f.pidfds.signalCount(101) != 0 {
		t.Fatal("stop --hard touched vm2, which was not selected")
	}
	if f.manifest.Nodes["vm2"].PID != 101 {
		t.Fatal("stop --hard changed the state of vm2")
	}
	if f.pidfds.signalCount(100) != 1 || f.pidfds.signalCount(102) != 1 {
		t.Fatal("a selected VM did not get exactly one SIGTERM")
	}
	var names []string
	for _, row := range f.rows(t) {
		names = append(names, row[0])
	}
	if !slices.Equal(names, []string{"vm3", "vm1"}) {
		t.Fatalf("rows = %q, want the given order without duplicates", names)
	}
}

func TestHardStopRejectsUnknownVMBeforeAnySignal(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2")
	err := f.run("--hard", "vm1", "vm9")
	if err == nil || !strings.Contains(err.Error(), `unknown VM "vm9"`) {
		t.Fatalf("err = %v, want unknown VM", err)
	}
	if events := f.events.list(); len(events) != 0 {
		t.Fatalf("events = %q, want none", events)
	}
	if f.stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", f.stdout.String())
	}
}

func TestHardStopSignalsAllVMsBeforeItWaits(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2", "vm3", "vm4")
	if err := f.run("--hard"); err != nil {
		t.Fatal(err)
	}
	events := f.events.list()
	lastOpen, firstTerminate, lastTerminate, firstWait := -1, len(events), -1, len(events)
	for i, event := range events {
		switch {
		case strings.HasPrefix(event, "open "):
			lastOpen = i
		case strings.HasPrefix(event, "terminate "):
			firstTerminate = min(firstTerminate, i)
			lastTerminate = i
		case strings.HasPrefix(event, "wait "):
			firstWait = min(firstWait, i)
		}
	}
	if lastOpen > firstTerminate || lastTerminate > firstWait || firstWait == len(events) {
		t.Fatalf("events = %q, want all opens, then all SIGTERMs, then the waits", events)
	}
	if want := []string{"terminate 100", "terminate 101", "terminate 102", "terminate 103"}; !slices.Equal(events[firstTerminate:lastTerminate+1], want) {
		t.Fatalf("SIGTERM events = %q, want one loop %q", events[firstTerminate:lastTerminate+1], want)
	}
	// The fire loop reads the clock once per VM, so the stop times are 1 ms
	// apart with the step clock.
	var stopTimes []string
	for _, row := range f.rows(t) {
		stopTimes = append(stopTimes, row[3])
	}
	want := []string{"2026-11-14T10:00:00.123Z", "2026-11-14T10:00:00.124Z", "2026-11-14T10:00:00.125Z", "2026-11-14T10:00:00.126Z"}
	if !slices.Equal(stopTimes, want) {
		t.Fatalf("stop times = %q, want %q", stopTimes, want)
	}
}

func TestHardStopNeverUsesTheGracefulStopOrProcessSignals(t *testing.T) {
	// The fixture fails the test if Stopper.Stop or Processes.Signal runs for
	// a QEMU PID. qemu.ExitHandle has no method that sends SIGKILL.
	f := newHardStopFixture(t, "vm1", "vm2")
	f.pidfds.hang[100] = true
	err := f.run("--hard")
	if err == nil {
		t.Fatal("want an error for the process that did not exit")
	}
	if f.pidfds.signalCount(100) != 1 || f.pidfds.signalCount(101) != 1 {
		t.Fatal("each VM must get exactly one SIGTERM")
	}
}

func TestHardStopReportsProcessThatDoesNotExit(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2", "vm3")
	f.pidfds.hang[101] = true
	err := f.run("--hard")
	want := "stop --hard: VM vm2 (PID 101) did not exit within 30s after SIGTERM; the process is left running"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if !f.processes.Alive(101) || f.pidfds.signalCount(101) != 1 {
		t.Fatal("the process that did not exit must stay running after exactly one SIGTERM")
	}
	saved, err := state.Load(f.layout.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Nodes["vm2"]; got.PID != 101 || got.Process == nil {
		t.Fatalf("vm2 lost its PID: %#v", got)
	}
	for _, name := range []string{"vm1", "vm3"} {
		if got := saved.Nodes[name]; got.PID != 0 || got.Process != nil {
			t.Fatalf("%s stop was not recorded: %#v", name, got)
		}
	}
	rows := f.rows(t)
	if row := rows[1]; row[0] != "vm2" || row[1] != hardStopNotExited || row[2] != "101" || row[3] == "-" || row[4] != "-" {
		t.Fatalf("vm2 row = %q", row)
	}
}

func TestHardStopChecksIdentityBeforeAnySignal(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2", "vm3")
	f.processes.args[101] = []string{"qemu-system-x86_64", "-name", "guest=topolab:other:vm2,debug-threads=on"}
	err := f.run("--hard")
	if err == nil || !strings.Contains(err.Error(), "no VM was stopped: VM vm2") || !strings.Contains(err.Error(), "does not identify expected node") {
		t.Fatalf("err = %v", err)
	}
	for _, event := range f.events.list() {
		if strings.HasPrefix(event, "terminate ") {
			t.Fatalf("a VM got SIGTERM before all checks passed: %q", f.events.list())
		}
	}
	for pid := 100; pid <= 102; pid++ {
		if !f.processes.Alive(pid) {
			t.Fatalf("PID %d stopped", pid)
		}
	}
}

type namespaceMismatch struct{ recordingNamespaceOps }

func (namespaceMismatch) SameProcess(int, string) (bool, error) { return false, nil }

func TestHardStopChecksNamespaceBeforeAnySignal(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2", "vm3")
	f.manifest.Nodes["vm3"].NetworkNamespace = "tl-fabric"
	f.app.Namespaces = &namespaceMismatch{}
	err := f.run("--hard")
	if err == nil || !strings.Contains(err.Error(), "VM vm3") || !strings.Contains(err.Error(), "outside expected fabric namespace tl-fabric") {
		t.Fatalf("err = %v", err)
	}
	for _, event := range f.events.list() {
		if strings.HasPrefix(event, "terminate ") {
			t.Fatalf("a VM got SIGTERM before all checks passed: %q", f.events.list())
		}
	}
}

func TestHardStopTreatsProcessThatAlreadyExitedAsNotRunning(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2")
	f.pidfds.gone[100] = true
	if err := f.run("--hard"); err != nil {
		t.Fatal(err)
	}
	rows := f.rows(t)
	if want := []string{"vm1", hardStopNotRunning, "-", "-", "-"}; !slices.Equal(rows[0], want) {
		t.Fatalf("vm1 row = %q, want %q", rows[0], want)
	}
	if got := f.manifest.Nodes["vm1"]; got.PID != 0 || got.Process != nil {
		t.Fatalf("vm1 stop was not recorded: %#v", got)
	}
}

func TestHardStopOutputFormat(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2", "vm3")
	f.processes.kill(102)
	// A local time zone must still print as UTC.
	local := hardStopStart.In(time.FixedZone("IRST", 3*3600+1800))
	f.app.Now = func() time.Time { return local }
	if err := f.run("--hard"); err != nil {
		t.Fatal(err)
	}
	want := "NAME\tRESULT\tPID\tSTOP_TIME\tEXIT_TIME\n" +
		"vm1\tstopped\t100\t2026-11-14T10:00:00.123Z\t2026-11-14T10:00:00.123Z\n" +
		"vm2\tstopped\t101\t2026-11-14T10:00:00.123Z\t2026-11-14T10:00:00.123Z\n" +
		"vm3\tnot-running\t-\t-\t-\n"
	if got := f.stdout.String(); got != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", got, want)
	}
}

func TestHardStopLeavesTheStateOfANormalStop(t *testing.T) {
	f := newHardStopFixture(t, "vm1")
	n := f.manifest.Nodes["vm1"]
	n.CollectorProcess = &state.Process{PID: 900, IdentityArg: "collector-vm1"}
	f.processes.alive[900] = true
	f.processes.args[900] = []string{"topolab", "__serial-collector", "collector-vm1"}
	for _, p := range []string{n.QMPPath} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.run("--hard"); err != nil {
		t.Fatal(err)
	}
	saved, err := state.Load(f.layout.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	got := saved.Nodes["vm1"]
	if got.PID != 0 || got.Process != nil || got.CollectorProcess != nil {
		t.Fatalf("saved node = %#v, want no PID, process, or collector", got)
	}
	if f.processes.Alive(900) {
		t.Fatal("the serial collector still runs")
	}
	for _, p := range []string{n.PIDFile, n.QMPPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", p, err)
		}
	}
	if _, err := os.Stat(n.RootDisk.Path); err != nil {
		t.Fatalf("stop --hard removed the root disk: %v", err)
	}
}

// orderStopper records the graceful-stop order and ends each process.
type orderStopper struct {
	processes *lockedProcesses
	mu        sync.Mutex
	nodes     []string
}

func (s *orderStopper) Stop(_ context.Context, pid int, _, node, _ string) error {
	s.mu.Lock()
	s.nodes = append(s.nodes, node)
	s.mu.Unlock()
	s.processes.kill(pid)
	return nil
}

func TestNormalStopStillStopsGracefullyInOrder(t *testing.T) {
	f := newHardStopFixture(t, "vm1", "vm2", "vm3")
	stopper := &orderStopper{processes: f.processes}
	f.app.Stopper = stopper
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stopper.nodes, []string{"vm1", "vm2", "vm3"}) {
		t.Fatalf("graceful stop order = %q", stopper.nodes)
	}
	if events := f.events.list(); len(events) != 0 {
		t.Fatalf("the normal stop used exit handles: %q", events)
	}
	if f.stdout.Len() != 0 {
		t.Fatalf("the normal stop printed %q", f.stdout.String())
	}
}
