package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
)

// startDelayFixture holds two VMs, "a" and "b", in that order. Each can have a start_delay.
// Every process start and every sleep appends to events, so tests see the order.
type startDelayFixture struct {
	a         *App
	cfg       *config.Config
	layout    state.Layout
	m         *state.Manifest
	processes *fakeProcesses
	events    []string
	stderr    bytes.Buffer
}

func newStartDelayFixture(t *testing.T, delayForA, delayForB string, sleep func(context.Context, time.Duration) error) *startDelayFixture {
	t.Helper()
	f := &startDelayFixture{processes: &fakeProcesses{alive: map[int]bool{}, args: map[int][]string{}}}
	f.layout, _ = shortLayout(t)
	one := 1
	newVM := func(name, delay string) *config.VM {
		vm := &config.VM{Name: name, CPU: &config.CPU{Model: "host", Sockets: &one, Cores: &one, Threads: &one}, MemoryMB: 128, MemoryBalloon: &config.MemoryBalloon{}, Boot: config.Boot{Order: []string{"disk"}}}
		if delay != "" {
			d, err := time.ParseDuration(delay)
			if err != nil {
				t.Fatal(err)
			}
			vm.StartDelay, vm.StartDelayDuration = delay, d
		}
		return vm
	}
	f.cfg = &config.Config{Name: "lab", VMs: []*config.VM{newVM("a", delayForA), newVM("b", delayForB)}}
	f.m = state.NewManifest("lab")
	for _, name := range []string{"a", "b"} {
		n := checkpointDesired(f.layout)
		n.AdditionalDisks = nil
		n.SeedPending = false
		n.QMPPath, _ = f.layout.SocketPath(name, "qmp")
		n.SerialSocketPath, _ = f.layout.SocketPath(name, "serial")
		f.m.Nodes[name] = n
	}
	netState := &network.State{Topology: "lab", TAPs: map[string]network.TAPState{}}
	f.m.NetworkState, _ = json.Marshal(netState)
	if err := state.Save(f.layout.ManifestPath, f.m); err != nil {
		t.Fatal(err)
	}
	var qmpCalls []string
	pid := 40
	f.a = New(nil, &f.stderr)
	f.a.Network = checkpointNetwork{netState}
	f.a.Namespaces = &recordingNamespaceOps{}
	f.a.Processes = f.processes
	f.a.Executable = func() (string, error) { return "/topolab", nil }
	f.a.StartProcess = func(argv []string, _ []*os.File) (*qemu.StartedProcess, error) {
		pid++
		f.processes.alive[pid] = true
		f.processes.args[pid] = argv
		f.events = append(f.events, "spawn")
		return &qemu.StartedProcess{PID: pid}, nil
	}
	f.a.OpenQMP = func(string) qmpClient { return orderedQMP{calls: &qmpCalls} }
	f.a.VhostAvailable = func() bool { return false }
	f.a.Sleep = func(ctx context.Context, d time.Duration) error {
		f.events = append(f.events, "sleep:"+d.String())
		if sleep != nil {
			return sleep(ctx, d)
		}
		return nil
	}
	f.a.defaults()
	return f
}

func (f *startDelayFixture) sleeps() []string {
	var out []string
	for _, event := range f.events {
		if strings.HasPrefix(event, "sleep:") {
			out = append(out, event)
		}
	}
	return out
}

func TestStartWaitsForStartDelayBeforeLaunchingThatVM(t *testing.T) {
	f := newStartDelayFixture(t, "", "30s", nil)
	if err := f.a.start(context.Background(), f.cfg, f.layout, f.m, nil); err != nil {
		t.Fatal(err)
	}
	at := -1
	for i, event := range f.events {
		if event == "sleep:30s" {
			if at >= 0 {
				t.Fatalf("slept twice: %v", f.events)
			}
			at = i
		}
	}
	// VM "a" spawns first; the wait comes next; VM "b" spawns after it.
	spawnsBefore, spawnsAfter := at, len(f.events)-at-1
	if at <= 0 || spawnsAfter != spawnsBefore {
		t.Fatalf("sleep is not between the VM launches: %v", f.events)
	}
	if want := "waiting 30s before starting VM b\n"; f.stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", f.stderr.String(), want)
	}
}

func TestStartWithoutStartDelayNeverSleeps(t *testing.T) {
	f := newStartDelayFixture(t, "", "", nil)
	if err := f.a.start(context.Background(), f.cfg, f.layout, f.m, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.sleeps(); len(got) != 0 {
		t.Fatalf("unexpected sleeps: %v", got)
	}
	if f.stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", f.stderr.String())
	}
}

func TestStartSkipsStartDelayForRunningVM(t *testing.T) {
	f := newStartDelayFixture(t, "", "30s", nil)
	const runningPID = 99
	f.processes.alive[runningPID] = true
	f.processes.args[runningPID] = []string{"guest=topolab:lab:b,debug-threads=on"}
	f.m.Nodes["b"].PID = runningPID
	if err := f.a.start(context.Background(), f.cfg, f.layout, f.m, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.sleeps(); len(got) != 0 {
		t.Fatalf("running VM waited: %v", got)
	}
	if f.stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", f.stderr.String())
	}
}

func TestStartSkipsStartDelayForFirstLaunchedVM(t *testing.T) {
	f := newStartDelayFixture(t, "30s", "", nil)
	if err := f.a.start(context.Background(), f.cfg, f.layout, f.m, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.sleeps(); len(got) != 0 {
		t.Fatalf("first VM waited: %v", got)
	}
	if f.stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", f.stderr.String())
	}
}

func TestStartWaitsOnlyForLaterVMsWhenEveryVMHasStartDelay(t *testing.T) {
	f := newStartDelayFixture(t, "10s", "30s", nil)
	if err := f.a.start(context.Background(), f.cfg, f.layout, f.m, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.sleeps(); len(got) != 1 || got[0] != "sleep:30s" {
		t.Fatalf("sleeps = %v, want only the 30s wait of VM b", got)
	}
	if want := "waiting 30s before starting VM b\n"; f.stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", f.stderr.String(), want)
	}
}

func TestRunningVMIsNotTheFirstLaunch(t *testing.T) {
	f := newStartDelayFixture(t, "", "30s", nil)
	const runningPID = 99
	f.processes.alive[runningPID] = true
	f.processes.args[runningPID] = []string{"guest=topolab:lab:a,debug-threads=on"}
	f.m.Nodes["a"].PID = runningPID
	if err := f.a.start(context.Background(), f.cfg, f.layout, f.m, nil); err != nil {
		t.Fatal(err)
	}
	// VM a already ran and was not launched, so VM b is the first launch and does not wait.
	if got := f.sleeps(); len(got) != 0 {
		t.Fatalf("VM b waited although no VM was launched before it: %v", got)
	}
}

func TestStartWithNamedVMsSkipsWaitOfTheFirstNamedVM(t *testing.T) {
	f := newStartDelayFixture(t, "", "30s", nil)
	if err := f.a.start(context.Background(), f.cfg, f.layout, f.m, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if got := f.sleeps(); len(got) != 0 {
		t.Fatalf("the only launched VM waited: %v", got)
	}
}

func TestStartStopsWhenStartDelayIsCancelled(t *testing.T) {
	f := newStartDelayFixture(t, "", "30s", func(context.Context, time.Duration) error { return context.Canceled })
	err := f.a.start(context.Background(), f.cfg, f.layout, f.m, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	if f.m.Nodes["b"].PID != 0 {
		t.Fatalf("VM b started after the wait was cancelled: PID %d", f.m.Nodes["b"].PID)
	}
	if last := f.events[len(f.events)-1]; last != "sleep:30s" {
		t.Fatalf("a process started after the cancelled wait: %v", f.events)
	}
}

func TestSleepContextWaitsAndHonorsCancel(t *testing.T) {
	if err := sleepContext(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("short sleep: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled sleep did not return at once")
	}
}
