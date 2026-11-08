package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/remote/blob"
	"github.com/torob/topolab/internal/remote/jobs"
	"github.com/torob/topolab/internal/remote/wire"
)

// lifecycleCommands run as jobs that outlive the session.
var lifecycleCommands = map[string]bool{"up": true, "start": true, "stop": true, "restart": true, "down": true}

func (s *session) jobStore(workingDir string) *jobs.Store {
	store := jobs.NewStore(workingDir)
	if s.opts.JobProcs != nil {
		store.Procs = s.opts.JobProcs
	}
	store.Now = s.opts.Now
	return store
}

// requestDigest identifies a request independently of the offset that a
// reconnect resumes from.
func requestDigest(req wire.Request) string {
	req.Offset = 0
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// runLifecycle creates the job (or attaches to the one with the request's
// identifier) and streams its output until it ends.
func (s *session) runLifecycle(req wire.Request) (int, error) {
	p, err := parseRequest(req)
	if err != nil {
		return s.reject(err)
	}
	store := s.jobStore(p.workingDir)
	id := req.JobID
	if id == "" {
		id, err = wire.NewNonce()
		if err != nil {
			return 1, err
		}
		req.JobID = id
	}
	topology := p.cfg.Name

	// A request with a known identifier is a reconnection: attach to that job.
	if st, err := store.Load(topology, id); err == nil {
		if st.RequestDigest != requestDigest(req) {
			return s.reject(reqErr(wire.CodeProtocolError, "job %s exists with a different request", id))
		}
		return s.streamAfterWarnings(p, store, st, req.Offset)
	} else if !errors.Is(err, jobs.ErrNotFound) {
		return s.reject(err)
	}

	// Until the job exists, a cancel frame stops the command before it starts.
	pre, cancelPre := context.WithCancel(s.ctx)
	defer cancelPre()
	s.setCancel(cancelPre)
	stopped := func() (int, error) {
		if s.ctx.Err() != nil {
			return 1, context.Cause(s.ctx)
		}
		return 1, nil // cancelled by the controller
	}

	release := func() {}
	var digests []string
	if req.Command == "up" {
		if err := checkHostTemplates(p); err != nil {
			return s.reject(err)
		}
		refs, err := usedTemplates(p, req)
		if err != nil {
			return s.reject(err)
		}
		if len(refs) > 0 {
			release, err = s.receiveTemplates(pre, p, req)
			if err != nil {
				if pre.Err() != nil {
					return stopped()
				}
				return s.reject(err)
			}
		}
		for _, r := range refs {
			digests = append(digests, r.SHA256)
		}
	}
	st, err := s.createJob(pre, store, p, req, id, digests)
	release()
	if err != nil {
		if pre.Err() != nil {
			return stopped()
		}
		return s.reject(err)
	}
	return s.streamAfterWarnings(p, store, st, req.Offset)
}

// streamAfterWarnings streams a job's output. The runner records parse warnings
// in the job log itself, so a reconnect shows them once and in order.
func (s *session) streamAfterWarnings(_ *parsed, store *jobs.Store, st *jobs.Status, offset int64) (int, error) {
	return s.streamJob(s.ctx, store, st.Topology, st.ID, offset)
}

// createJob registers a job and starts its runner. It holds the topology's job
// lock until the runner records `running`.
func (s *session) createJob(ctx context.Context, store *jobs.Store, p *parsed, req wire.Request, id string, templates []string) (*jobs.Status, error) {
	topology := p.cfg.Name
	lock, err := store.TopologyLock(ctx, topology)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	// Another session with the same identifier may have created it meanwhile.
	if st, err := store.Load(topology, id); err == nil {
		if st.RequestDigest != requestDigest(req) {
			return nil, reqErr(wire.CodeProtocolError, "job %s exists with a different request", id)
		}
		return st, nil
	}
	active, err := store.Active(ctx, topology)
	if err != nil {
		return nil, err
	}
	if active != nil {
		return nil, reqErr(wire.CodeJobActive, "job %s (%s) is %s for topology %s; attach with `jobs attach %s` or cancel it with `jobs cancel %s`", active.ID, active.Command, active.State, topology, active.ID, active.ID)
	}
	secrets, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	st := jobs.Status{ID: id, Topology: topology, Command: req.Command, Args: req.Args, State: jobs.StatePending, Created: s.opts.Now().UTC(), Templates: templates, RequestDigest: requestDigest(req)}
	if err := store.Create(st, secrets); err != nil {
		return nil, err
	}
	if err := s.opts.SpawnJob(store.Dir(topology, id)); err != nil {
		_, _ = store.Finish(ctx, topology, id, jobs.StateFailed, 1, fmt.Sprintf("start the job runner: %v", err))
		return nil, fmt.Errorf("start the job runner: %w", err)
	}
	deadline := time.NewTimer(s.opts.JobStartTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		cur, err := store.Load(topology, id)
		if err != nil {
			return nil, err
		}
		if cur.State != jobs.StatePending {
			return cur, nil
		}
		// Once the job exists, only a lost connection ends the wait; a cancel
		// frame is forwarded to the runner when the stream starts.
		select {
		case <-s.ctx.Done():
			return nil, context.Cause(s.ctx)
		case <-deadline.C:
			_, _ = store.Finish(context.Background(), topology, id, jobs.StateFailed, 1, "the job runner did not start")
			return nil, fmt.Errorf("the job runner did not start within %s", s.opts.JobStartTimeout)
		case <-tick.C:
		}
	}
}

// signalRunner asks a job's runner to stop after checking its identity.
func (s *session) signalRunner(store *jobs.Store, topology, id string) {
	st, err := store.Load(topology, id)
	if err != nil || !store.Alive(st) || st.Runner.PID <= 0 {
		return
	}
	s.opts.SignalRunner(st.Runner.PID)
}

// streamJob sends a job's output records from offset, then its exit status.
// A cancel frame asks the runner to stop, as an interrupt does in local mode;
// the stream goes on until the job ends. A lost connection stops the stream and
// leaves the job running.
func (s *session) streamJob(ctx context.Context, store *jobs.Store, topology, id string, offset int64) (int, error) {
	st, err := store.Load(topology, id)
	if err != nil {
		return s.reject(jobLookupError(err, id))
	}
	if err := s.conn.WriteJSON(wire.TypeJobNotice, wire.JobNotice{ID: id, State: st.State}); err != nil {
		return 1, err
	}
	s.setCancel(func() { s.signalRunner(store, topology, id) })
	path := store.LogPath(topology, id)
	stdout := streamWriter{conn: s.conn, typ: wire.TypeStdout}
	stderr := streamWriter{conn: s.conn, typ: wire.TypeStderr}
	sawExit := false
	exitCode := 1
	for {
		for {
			rec, ok, err := jobs.ReadRecord(path, offset)
			if err != nil {
				return 1, err
			}
			if !ok {
				break
			}
			offset = rec.End
			switch rec.Stream {
			case jobs.StreamStdout:
				if _, err := stdout.Write(rec.Payload); err != nil {
					return 1, err
				}
			case jobs.StreamStderr:
				if _, err := stderr.Write(rec.Payload); err != nil {
					return 1, err
				}
			case jobs.StreamExit:
				sawExit = true
				if code, err := strconv.Atoi(string(rec.Payload)); err == nil {
					exitCode = code
				}
			}
			if sawExit {
				break
			}
		}
		if sawExit {
			return exitCode, s.sendJobExit(id)
		}
		cur, err := store.Load(topology, id)
		if err != nil {
			return 1, err
		}
		if !jobs.Terminal(cur.State) && !store.Alive(cur) {
			cur = s.markLost(ctx, store, cur)
		}
		if jobs.Terminal(cur.State) {
			// The runner is gone: drain what it wrote, then report the end.
			if _, ok, _ := jobs.ReadRecord(path, offset); ok {
				continue
			}
			if cur.State == jobs.StateSucceeded {
				return 0, s.sendJobExit(id)
			}
			msg := cur.Error
			if msg == "" {
				msg = "the job ended without an exit record"
			}
			if _, err := fmt.Fprintf(stderr, "topolab: job %s is %s: %s\n", id, cur.State, msg); err != nil {
				return 1, err
			}
			return 1, s.sendJobExit(id)
		}
		select {
		case <-ctx.Done():
			return 1, context.Cause(ctx)
		case <-time.After(s.opts.PollInterval):
		}
	}
}

// sendJobExit lets the caller's exit frame carry the job identifier.
func (s *session) sendJobExit(id string) error {
	s.jobID = id
	return nil
}

// markLost records `lost` for a job whose runner is gone.
func (s *session) markLost(ctx context.Context, store *jobs.Store, st *jobs.Status) *jobs.Status {
	lock, err := store.TopologyLock(ctx, st.Topology)
	if err != nil {
		return st
	}
	defer lock.Close()
	updated, err := store.MarkLostIfDead(ctx, st)
	if err != nil {
		return st
	}
	return updated
}

func jobLookupError(err error, id string) error {
	if errors.Is(err, jobs.ErrNotFound) {
		return reqErr(wire.CodeJobNotFound, "job %s does not exist", id)
	}
	return err
}

// RunJob is the job runner (`topolab __job DIR`). It runs one lifecycle command
// from the job's records and writes its output, exit record, and final state.
func RunJob(ctx context.Context, dir string, runner Runner) int {
	dir = filepath.Clean(dir)
	id := filepath.Base(dir)
	topology := filepath.Base(filepath.Dir(dir))
	workingDir := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(dir))))
	store := jobs.NewStore(workingDir)
	if store.Dir(topology, id) != dir {
		fmt.Fprintf(os.Stderr, "topolab: %s is not a job directory\n", dir)
		return 1
	}
	raw, err := store.Secrets(topology, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "topolab: read job secrets: %v\n", err)
		return 1
	}
	var req wire.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		fmt.Fprintf(os.Stderr, "topolab: decode job request: %v\n", err)
		return 1
	}
	log, err := store.OpenLog(topology, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "topolab: open job log: %v\n", err)
		return 1
	}
	defer log.Close()
	stdout, stderr := log.StreamWriter(jobs.StreamStdout), log.StreamWriter(jobs.StreamStderr)

	finish := func(state string, code int, message string) int {
		if message != "" {
			fmt.Fprintf(stderr, "topolab: %s\n", message)
		}
		_ = log.WriteExit(code)
		if _, err := store.Finish(context.Background(), topology, id, state, code, message); err != nil {
			fmt.Fprintf(os.Stderr, "topolab: record job state: %v\n", err)
			return 1
		}
		return 0
	}

	identity, err := runnerIdentity(store)
	if err != nil {
		return finish(jobs.StateFailed, 1, fmt.Sprintf("identify the runner: %v", err))
	}
	if _, err := store.Update(ctx, topology, id, func(st *jobs.Status) bool {
		now := store.Now().UTC()
		st.State, st.Started, st.Runner = jobs.StateRunning, &now, identity
		return true
	}); err != nil {
		fmt.Fprintf(os.Stderr, "topolab: record job start: %v\n", err)
		return 1
	}

	p, err := parseRequest(req)
	if err != nil {
		return finish(jobs.StateFailed, 1, err.Error())
	}
	for _, w := range p.warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	digests, err := applyTemplates(p, req, blob.NewStore(p.workingDir))
	if err != nil {
		return finish(jobs.StateFailed, 1, err.Error())
	}
	command := app.Command{Name: req.Command, Args: req.Args, Topology: p.cfg, WorkingDir: p.workingDir, Helpers: helperOverrides(req.Helpers)}
	runErr := runner.Run(ctx, command, RunIO{
		Stdout:          stdout,
		Stderr:          stderr,
		TemplateDigests: digests,
		LockNotice:      func() { fmt.Fprintln(stderr, "waiting for the topology lock") },
	})
	switch {
	case runErr == nil:
		return finish(jobs.StateSucceeded, 0, "")
	case errors.Is(runErr, context.Canceled) || ctx.Err() != nil:
		return finish(jobs.StateCancelled, 1, "")
	case errors.Is(runErr, app.ErrTemplateDigest):
		return finish(jobs.StateFailed, 1, fmt.Sprintf("%s: %v", wire.CodeBlobCorrupt, runErr))
	default:
		return finish(jobs.StateFailed, 1, runErr.Error())
	}
}

// runnerIdentity captures what identifies this process.
func runnerIdentity(store *jobs.Store) (jobs.RunnerIdentity, error) {
	boot, err := store.Procs.BootID()
	if err != nil {
		return jobs.RunnerIdentity{}, err
	}
	start, err := store.Procs.StartTime(os.Getpid())
	if err != nil {
		return jobs.RunnerIdentity{}, err
	}
	exe, _ := os.Executable()
	return jobs.RunnerIdentity{PID: os.Getpid(), StartTime: start, BootID: boot, Exe: strings.TrimSuffix(exe, " (deleted)")}, nil
}
