package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeProcs struct {
	boot     string
	starts   map[int]uint64
	cmdlines map[int][]string
}

func (f *fakeProcs) BootID() (string, error) { return f.boot, nil }
func (f *fakeProcs) StartTime(pid int) (uint64, error) {
	if v, ok := f.starts[pid]; ok {
		return v, nil
	}
	return 0, os.ErrNotExist
}
func (f *fakeProcs) Cmdline(pid int) ([]string, error) {
	if v, ok := f.cmdlines[pid]; ok {
		return v, nil
	}
	return nil, os.ErrNotExist
}

func newTestStore(t *testing.T) (*Store, *fakeProcs, *time.Time) {
	t.Helper()
	s := NewStore(t.TempDir())
	procs := &fakeProcs{boot: "boot-1", starts: map[int]uint64{}, cmdlines: map[int][]string{}}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s.Procs = procs
	s.Now = func() time.Time { return now }
	return s, procs, &now
}

func id(n int) string { return strings.Repeat(string(rune('a'+n%6)), 31) + string(rune('0'+n%10)) }

func newJob(t *testing.T, s *Store, topology string, n int, state string) Status {
	t.Helper()
	st := Status{ID: id(n), Topology: topology, Command: "up", State: state, Created: s.Now().Add(time.Duration(n) * time.Second)}
	if err := s.Create(st, []byte(`{"secret":true}`)); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCreateWritesPrivateRecordsAndLoadReadsThemBack(t *testing.T) {
	s, _, _ := newTestStore(t)
	st := newJob(t, s, "lab", 1, "")
	dir := s.Dir("lab", st.ID)
	for _, d := range []string{s.Root(), s.TopologyDir("lab"), dir} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi, err)
		}
	}
	for _, f := range []string{"secrets.json", "status.json", "output.log"} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, fi, err)
		}
	}
	got, err := s.Load("lab", st.ID)
	if err != nil || got.State != StatePending || got.Version != RecordVersion || got.Command != "up" {
		t.Fatalf("loaded = %+v %v", got, err)
	}
	if _, err := s.Load("lab", "not-an-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid id err = %v", err)
	}
	if _, err := s.Load("lab", id(9)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id err = %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "status.json"))
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("status.json holds secrets: %s", raw)
	}
}

func TestLoadRejectsANewerRecordVersion(t *testing.T) {
	s, _, _ := newTestStore(t)
	st := newJob(t, s, "lab", 1, "")
	path := filepath.Join(s.Dir("lab", st.ID), "status.json")
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"version": 1`, `"version": 99`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("lab", st.ID); err == nil || !strings.Contains(err.Error(), "record version 99") {
		t.Fatalf("err = %v", err)
	}
}

func TestFinishIsTerminalAndDeletesSecrets(t *testing.T) {
	s, _, _ := newTestStore(t)
	st := newJob(t, s, "lab", 1, StateRunning)
	got, err := s.Finish(context.Background(), "lab", st.ID, StateSucceeded, 0, "")
	if err != nil || got.State != StateSucceeded || got.Finished == nil {
		t.Fatalf("finish = %+v %v", got, err)
	}
	if _, err := s.Secrets("lab", st.ID); !os.IsNotExist(err) {
		t.Fatalf("secrets remain: %v", err)
	}
	// A terminal state is never overwritten, not even by a later `lost`.
	got, err = s.Finish(context.Background(), "lab", st.ID, StateLost, 1, "late")
	if err != nil || got.State != StateSucceeded {
		t.Fatalf("terminal state changed: %+v %v", got, err)
	}
	reloaded, _ := s.Load("lab", st.ID)
	if reloaded.State != StateSucceeded || reloaded.Error != "" {
		t.Fatalf("reloaded = %+v", reloaded)
	}
}

func TestUpdateAppliesChangeOnlyBeforeTerminal(t *testing.T) {
	s, _, _ := newTestStore(t)
	st := newJob(t, s, "lab", 1, "")
	got, err := s.Update(context.Background(), "lab", st.ID, func(st *Status) bool {
		st.State = StateRunning
		st.Runner = RunnerIdentity{PID: 5, StartTime: 77, BootID: "boot-1"}
		return true
	})
	if err != nil || got.State != StateRunning || got.Runner.PID != 5 {
		t.Fatalf("update = %+v %v", got, err)
	}
	called := false
	if _, err := s.Update(context.Background(), "lab", st.ID, func(*Status) bool { called = true; return false }); err != nil || !called {
		t.Fatalf("no-op update: %v %v", called, err)
	}
}

func TestLivenessChecksPidStartTimeBootAndCommand(t *testing.T) {
	s, procs, now := newTestStore(t)
	runner := RunnerIdentity{PID: 4242, StartTime: 1000, BootID: "boot-1"}
	st := &Status{State: StateRunning, Runner: runner, Created: *now}
	procs.starts[4242] = 1000
	procs.cmdlines[4242] = []string{"/usr/local/lib/topolab/x/topolab", "__job", "/dir"}
	if !s.Alive(st) {
		t.Fatal("a live runner was reported dead")
	}
	procs.starts[4242] = 1001 // PID reused by another process
	if s.Alive(st) {
		t.Fatal("a reused PID was reported alive")
	}
	procs.starts[4242] = 1000
	procs.boot = "boot-2" // host rebooted
	if s.Alive(st) {
		t.Fatal("a runner from before a reboot was reported alive")
	}
	procs.boot = "boot-1"
	procs.cmdlines[4242] = []string{"/bin/sleep", "60"}
	if s.Alive(st) {
		t.Fatal("another program under the same PID and start time was reported alive")
	}
	delete(procs.starts, 4242)
	procs.cmdlines[4242] = []string{"x", "__job"}
	if s.Alive(st) {
		t.Fatal("a vanished PID was reported alive")
	}
	if s.Alive(&Status{State: StateSucceeded, Runner: runner}) {
		t.Fatal("a finished job was reported alive")
	}
}

func TestPendingJobIsAliveOnlyForAGracePeriod(t *testing.T) {
	s, _, now := newTestStore(t)
	pending := &Status{State: StatePending, Created: *now}
	if !s.Alive(pending) {
		t.Fatal("a fresh pending job was reported dead")
	}
	*now = now.Add(31 * time.Second)
	if s.Alive(pending) {
		t.Fatal("an old pending job without a runner was reported alive")
	}
}

func TestActiveMarksDeadJobsLostAndFindsLiveOnes(t *testing.T) {
	s, procs, now := newTestStore(t)
	dead := newJob(t, s, "lab", 1, StateRunning)
	if _, err := s.Update(context.Background(), "lab", dead.ID, func(st *Status) bool {
		st.Runner = RunnerIdentity{PID: 10, StartTime: 5, BootID: "boot-1"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	active, err := s.Active(context.Background(), "lab")
	if err != nil || active != nil {
		t.Fatalf("active = %+v %v", active, err)
	}
	got, _ := s.Load("lab", dead.ID)
	if got.State != StateLost || got.Finished == nil || !strings.Contains(got.Error, "runner is gone") {
		t.Fatalf("dead job = %+v", got)
	}
	if _, err := s.Secrets("lab", dead.ID); !os.IsNotExist(err) {
		t.Fatalf("secrets of a lost job remain: %v", err)
	}

	live := newJob(t, s, "lab", 2, StateRunning)
	procs.starts[20], procs.cmdlines[20] = 9, []string{"topolab", "__job", "x"}
	if _, err := s.Update(context.Background(), "lab", live.ID, func(st *Status) bool {
		st.Runner = RunnerIdentity{PID: 20, StartTime: 9, BootID: "boot-1"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	active, err = s.Active(context.Background(), "lab")
	if err != nil || active == nil || active.ID != live.ID {
		t.Fatalf("active = %+v %v", active, err)
	}
	// Another topology is unaffected.
	if other, err := s.Active(context.Background(), "other"); err != nil || other != nil {
		t.Fatalf("other topology active = %+v %v", other, err)
	}
	_ = now
}

func TestListIsNewestFirstAndScopedByTopology(t *testing.T) {
	s, _, _ := newTestStore(t)
	newJob(t, s, "lab", 1, StateSucceeded)
	newJob(t, s, "lab", 3, StateSucceeded)
	newJob(t, s, "other", 2, StateSucceeded)
	lab, err := s.List("lab")
	if err != nil || len(lab) != 2 || lab[0].ID != id(3) || lab[1].ID != id(1) {
		t.Fatalf("lab = %+v %v", lab, err)
	}
	all, err := s.List("")
	if err != nil || len(all) != 3 || all[0].ID != id(3) || all[1].ID != id(2) {
		t.Fatalf("all = %+v %v", all, err)
	}
	if none, err := NewStore(t.TempDir()).List(""); err != nil || len(none) != 0 {
		t.Fatalf("empty = %v %v", none, err)
	}
}

func TestRetentionKeepsNewestTwentyAndAnythingYoungerThanSevenDays(t *testing.T) {
	s, _, now := newTestStore(t)
	// 25 old finished jobs, created and finished long ago.
	for i := 0; i < 25; i++ {
		st := Status{ID: strings.Repeat("a", 24) + string(rune('a'+i/10)) + string(rune('0'+i%10)) + "000000", Topology: "lab", Command: "up", State: StateSucceeded,
			Created: now.Add(-time.Duration(40-i) * 24 * time.Hour)}
		if err := s.Create(st, nil); err != nil {
			t.Fatal(err)
		}
		fin := st.Created.Add(time.Minute)
		if _, err := s.Update(context.Background(), "lab", st.ID, func(x *Status) bool { x.State = StateRunning; return true }); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Update(context.Background(), "lab", st.ID, func(x *Status) bool { return false }); err != nil {
			t.Fatal(err)
		}
		_ = fin
		if _, err := s.Finish(context.Background(), "lab", st.ID, StateSucceeded, 0, ""); err != nil {
			t.Fatal(err)
		}
		// Finish stamps "now"; backdate it to the job's own age.
		st2, _ := s.Load("lab", st.ID)
		old := st.Created.Add(time.Minute)
		st2.Finished = &old
		if err := s.write(*st2); err != nil {
			t.Fatal(err)
		}
	}
	// A running job and a young finished job must never be removed.
	running := newJob(t, s, "lab", 4, StateRunning)
	removed, err := s.Prune("lab")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 5 {
		t.Fatalf("removed %d jobs, want 5 (25 finished minus the newest 20): %v", len(removed), removed)
	}
	left, _ := s.List("lab")
	if len(left) != 21 {
		t.Fatalf("%d jobs left, want 21", len(left))
	}
	if _, err := s.Load("lab", running.ID); err != nil {
		t.Fatalf("running job removed: %v", err)
	}
	// Within seven days nothing goes, whatever the count.
	s2, _, now2 := newTestStore(t)
	for i := 0; i < 30; i++ {
		st := Status{ID: strings.Repeat("b", 24) + string(rune('a'+i/10)) + string(rune('0'+i%10)) + "000000", Topology: "lab", Command: "up", State: StateSucceeded, Created: now2.Add(-time.Duration(i) * time.Minute)}
		if err := s2.Create(st, nil); err != nil {
			t.Fatal(err)
		}
	}
	removed, err = s2.Prune("lab")
	if err != nil || len(removed) != 0 {
		t.Fatalf("young jobs pruned: %v %v", removed, err)
	}
}

func TestActiveTemplatesCoverOnlyNonTerminalJobs(t *testing.T) {
	s, _, _ := newTestStore(t)
	run := Status{ID: id(1), Topology: "lab", Command: "up", State: StateRunning, Created: s.Now(), Templates: []string{"d1", "d2"}}
	done := Status{ID: id(2), Topology: "lab", Command: "up", State: StateSucceeded, Created: s.Now(), Templates: []string{"d3"}}
	for _, st := range []Status{run, done} {
		if err := s.Create(st, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Create writes the state as given; make the second one terminal for real.
	if _, err := s.Finish(context.Background(), "lab", done.ID, StateSucceeded, 0, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActiveTemplates()
	if err != nil || len(got) != 2 || !got["d1"] || !got["d2"] || got["d3"] {
		t.Fatalf("active templates = %v %v", got, err)
	}
}

func TestTopologyLockSerializesCreation(t *testing.T) {
	s, _, _ := newTestStore(t)
	first, err := s.TopologyLock(context.Background(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := s.TopologyLock(ctx, "lab"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if other, err := s.TopologyLock(context.Background(), "other"); err != nil {
		t.Fatalf("another topology blocked: %v", err)
	} else {
		_ = other.Close()
	}
	_ = first.Close()
}

func TestOSProcsReadsTheCurrentProcess(t *testing.T) {
	p := OSProcs{}
	if boot, err := p.BootID(); err != nil || boot == "" {
		t.Fatalf("boot id = %q %v", boot, err)
	}
	start, err := p.StartTime(os.Getpid())
	if err != nil || start == 0 {
		t.Fatalf("start time = %d %v", start, err)
	}
	again, _ := p.StartTime(os.Getpid())
	if again != start {
		t.Fatalf("start time is not stable: %d vs %d", start, again)
	}
	cmd, err := p.Cmdline(os.Getpid())
	if err != nil || len(cmd) == 0 {
		t.Fatalf("cmdline = %v %v", cmd, err)
	}
	if _, err := p.StartTime(1 << 30); err == nil {
		t.Fatal("a nonexistent PID has a start time")
	}
}
