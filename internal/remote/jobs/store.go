// Package jobs stores the records of remote lifecycle jobs
// (spec.md Section 12.6): status, secrets, and the output log.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/torob/topolab/internal/state"
)

// Job states.
const (
	StatePending   = "pending"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateLost      = "lost"
)

// RecordVersion is the version of status.json.
const RecordVersion = 1

// Retention (spec.md Section 12.6).
const (
	RetainCount = 20
	RetainAge   = 7 * 24 * time.Hour
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidID reports whether s is a job identifier.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// Terminal reports whether a state is final.
func Terminal(s string) bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateLost:
		return true
	}
	return false
}

// RunnerIdentity identifies a runner process so that a reused PID is not taken
// for it.
type RunnerIdentity struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"`
	BootID    string `json:"boot_id"`
	Exe       string `json:"exe,omitempty"`
}

// Status is the content of status.json. It never holds secrets.
type Status struct {
	Version   int            `json:"version"`
	ID        string         `json:"id"`
	Topology  string         `json:"topology"`
	Command   string         `json:"command"`
	Args      []string       `json:"args,omitempty"`
	State     string         `json:"state"`
	Created   time.Time      `json:"created"`
	Started   *time.Time     `json:"started,omitempty"`
	Finished  *time.Time     `json:"finished,omitempty"`
	ExitCode  int            `json:"exit_code"`
	Error     string         `json:"error,omitempty"`
	Runner    RunnerIdentity `json:"runner"`
	Templates []string       `json:"templates,omitempty"`
	// RequestDigest identifies the request, so that a reconnect resends the
	// same one and a conflicting reuse of an identifier is detected.
	RequestDigest string `json:"request_digest"`
}

// Procs reads process facts; tests replace it.
type Procs interface {
	BootID() (string, error)
	StartTime(pid int) (uint64, error)
	Cmdline(pid int) ([]string, error)
}

// OSProcs reads /proc.
type OSProcs struct{}

// BootID implements Procs.
func (OSProcs) BootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(raw)), err
}

// StartTime implements Procs. It returns field 22 of /proc/<pid>/stat.
func (OSProcs) StartTime(pid int) (uint64, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	text := string(raw)
	// The command name is in parentheses and may contain spaces.
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(text[end+1:])
	// fields[0] is field 3 (state); start time is field 22.
	if len(fields) < 20 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

// Cmdline implements Procs.
func (OSProcs) Cmdline(pid int) ([]string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"), nil
}

// Store is the job store of one working directory.
type Store struct {
	root  string
	Procs Procs
	Now   func() time.Time
}

// NewStore returns the store under <workingDir>/.topolab/jobs.
func NewStore(workingDir string) *Store {
	return &Store{root: filepath.Join(workingDir, ".topolab", "jobs"), Procs: OSProcs{}, Now: time.Now}
}

// Root is the store directory.
func (s *Store) Root() string { return s.root }

// TopologyDir is the directory of one topology's jobs.
func (s *Store) TopologyDir(topology string) string { return filepath.Join(s.root, topology) }

// Dir is the directory of one job.
func (s *Store) Dir(topology, id string) string { return filepath.Join(s.root, topology, id) }

// ensure creates the store directories down to dir with root-only permissions.
func (s *Store) ensure(dir string) error {
	rel, err := filepath.Rel(s.root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside the job store", dir)
	}
	cur := s.root
	if err := os.MkdirAll(cur, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(cur, 0o700); err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if err := os.MkdirAll(cur, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(cur, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// TopologyLock takes jobs/<topology>/.lock, which serializes job creation and
// liveness decisions for one topology.
func (s *Store) TopologyLock(ctx context.Context, topology string) (*state.Lock, error) {
	if err := s.ensure(s.TopologyDir(topology)); err != nil {
		return nil, err
	}
	return state.AcquireLock(ctx, filepath.Join(s.TopologyDir(topology), ".lock"))
}

// Create writes the records of a new job in the pending state. The caller holds
// the topology lock.
func (s *Store) Create(st Status, secrets []byte) error {
	dir := s.Dir(st.Topology, st.ID)
	if err := s.ensure(dir); err != nil {
		return err
	}
	st.Version = RecordVersion
	if st.State == "" {
		st.State = StatePending
	}
	if st.Created.IsZero() {
		st.Created = s.Now().UTC()
	}
	if err := state.AtomicWrite(filepath.Join(dir, "secrets.json"), secrets, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "output.log"), nil, 0o600); err != nil {
		return err
	}
	return s.write(st)
}

func (s *Store) write(st Status) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return state.AtomicWrite(filepath.Join(s.Dir(st.Topology, st.ID), "status.json"), append(raw, '\n'), 0o600)
}

// ErrNotFound means that a job does not exist.
var ErrNotFound = errors.New("job not found")

// Load reads status.json.
func (s *Store) Load(topology, id string) (*Status, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir(topology, id), "status.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("read job %s: %w", id, err)
	}
	if st.Version > RecordVersion {
		return nil, fmt.Errorf("job %s has record version %d; this agent understands up to %d", id, st.Version, RecordVersion)
	}
	return &st, nil
}

// Update changes a job's status atomically under a per-job lock. fn runs only
// while the job is not in a terminal state; fn returning false makes no change.
func (s *Store) Update(ctx context.Context, topology, id string, fn func(*Status) bool) (*Status, error) {
	dir := s.Dir(topology, id)
	lock, err := state.AcquireLock(ctx, filepath.Join(dir, ".lock"))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	st, err := s.Load(topology, id)
	if err != nil {
		return nil, err
	}
	if Terminal(st.State) {
		return st, nil
	}
	if fn(st) {
		if err := s.write(*st); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// Finish records the terminal state. It deletes secrets.json afterwards, so a
// reader that sees a terminal state may rely on the secrets being gone soon.
func (s *Store) Finish(ctx context.Context, topology, id, stateName string, exitCode int, message string) (*Status, error) {
	st, err := s.Update(ctx, topology, id, func(st *Status) bool {
		now := s.Now().UTC()
		st.State, st.ExitCode, st.Error, st.Finished = stateName, exitCode, message, &now
		return true
	})
	if err != nil {
		return nil, err
	}
	_ = os.Remove(filepath.Join(s.Dir(topology, id), "secrets.json"))
	return st, nil
}

// Secrets returns secrets.json, or an error when it was already deleted.
func (s *Store) Secrets(topology, id string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.Dir(topology, id), "secrets.json"))
}

// Alive reports whether a running or pending job's runner still exists. A
// pending job has no runner identity yet and counts as alive for a short grace
// period while the agent is still starting it.
func (s *Store) Alive(st *Status) bool {
	if Terminal(st.State) {
		return false
	}
	if st.Runner.PID == 0 {
		return st.State == StatePending && s.Now().Sub(st.Created) < 30*time.Second
	}
	boot, err := s.Procs.BootID()
	if err != nil || boot != st.Runner.BootID {
		return false
	}
	start, err := s.Procs.StartTime(st.Runner.PID)
	if err != nil || start != st.Runner.StartTime {
		return false
	}
	cmd, err := s.Procs.Cmdline(st.Runner.PID)
	if err != nil {
		return false
	}
	for _, arg := range cmd {
		if arg == "__job" {
			return true
		}
	}
	return false
}

// MarkLostIfDead records `lost` for a job whose runner is gone and returns the
// possibly updated status. The caller should hold the topology lock.
func (s *Store) MarkLostIfDead(ctx context.Context, st *Status) (*Status, error) {
	if Terminal(st.State) || s.Alive(st) {
		return st, nil
	}
	updated, err := s.Finish(ctx, st.Topology, st.ID, StateLost, 1, "the job's runner is gone (host restart or crash)")
	if err != nil {
		return st, err
	}
	return updated, nil
}

// List returns the jobs of one topology, or of all topologies when topology is
// empty, newest first.
func (s *Store) List(topology string) ([]Status, error) {
	var topologies []string
	if topology != "" {
		topologies = []string{topology}
	} else {
		entries, err := os.ReadDir(s.root)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				topologies = append(topologies, e.Name())
			}
		}
	}
	var out []Status
	for _, t := range topologies {
		entries, err := os.ReadDir(s.TopologyDir(t))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() || !ValidID(e.Name()) {
				continue
			}
			st, err := s.Load(t, e.Name())
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, *st)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Active returns the topology's job that is not in a terminal state and whose
// runner is alive, marking dead ones lost on the way. The caller holds the
// topology lock.
func (s *Store) Active(ctx context.Context, topology string) (*Status, error) {
	list, err := s.List(topology)
	if err != nil {
		return nil, err
	}
	for i := range list {
		st := &list[i]
		if Terminal(st.State) {
			continue
		}
		st, err := s.MarkLostIfDead(ctx, st)
		if err != nil {
			return nil, err
		}
		if !Terminal(st.State) {
			return st, nil
		}
	}
	return nil, nil
}

// ActiveTemplates returns the digests that non-terminal jobs name, for blob
// prune protection.
func (s *Store) ActiveTemplates() (map[string]bool, error) {
	list, err := s.List("")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, st := range list {
		if Terminal(st.State) {
			continue
		}
		for _, d := range st.Templates {
			out[d] = true
		}
	}
	return out, nil
}

// Prune removes finished jobs of a topology that are both older than RetainAge
// and outside the RetainCount most recent finished jobs.
func (s *Store) Prune(topology string) ([]string, error) {
	list, err := s.List(topology)
	if err != nil {
		return nil, err
	}
	var removed []string
	finished := 0
	now := s.Now()
	for _, st := range list { // newest first
		if !Terminal(st.State) {
			continue
		}
		finished++
		if finished <= RetainCount {
			continue
		}
		end := st.Created
		if st.Finished != nil {
			end = *st.Finished
		}
		if now.Sub(end) <= RetainAge {
			continue
		}
		if err := os.RemoveAll(s.Dir(st.Topology, st.ID)); err != nil {
			return removed, err
		}
		removed = append(removed, st.ID)
	}
	return removed, nil
}
