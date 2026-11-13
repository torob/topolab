package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/state"
)

// lockedProcesses is a thread-safe fake for qemu.ProcessOps.
type lockedProcesses struct {
	mu    sync.Mutex
	alive map[int]bool
	args  map[int][]string
}

func (f *lockedProcesses) Alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[pid]
}

func (f *lockedProcesses) CommandLine(pid int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.args[pid], nil
}

func (f *lockedProcesses) Signal(pid int, _ os.Signal) error {
	f.kill(pid)
	return nil
}

func (f *lockedProcesses) kill(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[pid] = false
}

// barrierStopper makes every Stop call wait until all expected calls started.
// A sequential caller never reaches the barrier, so its first call times out.
type barrierStopper struct {
	processes *lockedProcesses
	want      int
	fail      map[string]error

	mu      sync.Mutex
	started int
	ready   chan struct{}
}

func newBarrierStopper(processes *lockedProcesses, want int) *barrierStopper {
	return &barrierStopper{processes: processes, want: want, fail: map[string]error{}, ready: make(chan struct{})}
}

func (s *barrierStopper) Stop(_ context.Context, pid int, _, node, _ string) error {
	s.mu.Lock()
	s.started++
	if s.started == s.want {
		close(s.ready)
	}
	s.mu.Unlock()
	select {
	case <-s.ready:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("barrier timeout: Stop calls did not run at the same time (node %s)", node)
	}
	if err := s.fail[node]; err != nil {
		return err
	}
	s.processes.kill(pid)
	return nil
}

// countingNetwork counts Remove calls.
type countingNetwork struct {
	checkpointNetwork
	removed atomic.Int32
}

func (n *countingNetwork) Remove(context.Context, *network.State) error {
	n.removed.Add(1)
	return nil
}

// downManifest builds a saved manifest with one running VM per name.
func downManifest(t *testing.T, layout state.Layout, names ...string) (*state.Manifest, *lockedProcesses) {
	t.Helper()
	processes := &lockedProcesses{alive: map[int]bool{}, args: map[int][]string{}}
	m := state.NewManifest("lab")
	for i, name := range names {
		dir, err := layout.NodeDir(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		qmp, err := layout.SocketPath(name, "qmp")
		if err != nil {
			t.Fatal(err)
		}
		pid := 100 + i
		n := &state.Node{
			Kind:     "vm",
			RootDisk: diskRecord("root", filepath.Join(dir, "root.qcow2"), "scsi", 100),
			SeedPath: filepath.Join(dir, "seed.iso"),
			PIDFile:  filepath.Join(dir, "qemu.pid"),
			QMPPath:  qmp,
			PID:      pid,
			Process:  &state.Process{PID: pid},
		}
		for _, p := range []string{n.RootDisk.Path, n.SeedPath, n.PIDFile} {
			if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		processes.alive[pid] = true
		m.Nodes[name] = n
	}
	raw, err := json.Marshal(&network.State{Topology: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	m.NetworkState = raw
	if err := state.Save(layout.ManifestPath, m); err != nil {
		t.Fatal(err)
	}
	return m, processes
}

func TestDownStopsNodesInParallel(t *testing.T) {
	layout, _ := shortLayout(t)
	m, processes := downManifest(t, layout, "vm1", "vm2", "vm3", "vm4")
	net := &countingNetwork{}
	a := New(nil, nil)
	a.Processes = processes
	a.Stopper = newBarrierStopper(processes, 4)
	a.Network = net
	a.defaults()
	if err := a.down(context.Background(), &config.Config{Name: "lab"}, layout, m); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := os.Stat(layout.ManifestPath); !os.IsNotExist(err) {
		t.Fatalf("manifest remains after down: %v", err)
	}
	if got := net.removed.Load(); got != 1 {
		t.Fatalf("network Remove calls = %d, want 1", got)
	}
}

func TestDownStopFailureStopsOthersAndSavesManifest(t *testing.T) {
	layout, _ := shortLayout(t)
	m, processes := downManifest(t, layout, "vm1", "vm2", "vm3")
	stopper := newBarrierStopper(processes, 3)
	stopper.fail["vm2"] = errors.New("boom")
	net := &countingNetwork{}
	a := New(nil, nil)
	a.Processes = processes
	a.Stopper = stopper
	a.Network = net
	a.defaults()
	err := a.down(context.Background(), &config.Config{Name: "lab"}, layout, m)
	if err == nil {
		t.Fatal("down succeeded, want stop failure")
	}
	if !strings.Contains(err.Error(), "stop vm2: boom") {
		t.Fatalf("error does not name the failed node: %v", err)
	}
	if strings.Contains(err.Error(), "vm1") || strings.Contains(err.Error(), "vm3") || strings.Contains(err.Error(), "barrier") {
		t.Fatalf("error names a node that stopped: %v", err)
	}
	saved, err := state.Load(layout.ManifestPath)
	if err != nil {
		t.Fatalf("manifest was not saved: %v", err)
	}
	for _, name := range []string{"vm1", "vm3"} {
		got := saved.Nodes[name]
		if got == nil || got.PID != 0 || got.Process != nil {
			t.Fatalf("node %s stop was not recorded: %#v", name, got)
		}
	}
	if got := saved.Nodes["vm2"]; got == nil || got.PID != 101 || got.Process == nil {
		t.Fatalf("failed node %s lost its PID: %#v", "vm2", got)
	}
	if !processes.Alive(101) || processes.Alive(100) || processes.Alive(102) {
		t.Fatal("wrong set of processes stopped")
	}
	for _, name := range []string{"vm1", "vm2", "vm3"} {
		if _, err := os.Stat(saved.Nodes[name].RootDisk.Path); err != nil {
			t.Fatalf("down removed the disk of %s: %v", name, err)
		}
	}
	if got := net.removed.Load(); got != 0 {
		t.Fatalf("network Remove calls = %d, want 0", got)
	}
}

func TestStopNodesErrorOrderIsStable(t *testing.T) {
	layout, _ := shortLayout(t)
	m, processes := downManifest(t, layout, "vm1", "vm2", "vm3")
	stopper := newBarrierStopper(processes, 3)
	stopper.fail["vm3"] = errors.New("e3")
	stopper.fail["vm1"] = errors.New("e1")
	a := New(nil, nil)
	a.Processes = processes
	a.Stopper = stopper
	a.defaults()
	err := a.stopNodes(context.Background(), "lab", m.Nodes)
	if err == nil {
		t.Fatal("stopNodes succeeded, want errors")
	}
	if got, want := err.Error(), "stop vm1: e1\nstop vm3: e3"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
