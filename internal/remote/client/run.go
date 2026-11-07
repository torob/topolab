package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/torob/topolab/internal/remote/jobs"
	"github.com/torob/topolab/internal/remote/wire"
)

// pumpResult is how a session ended.
type pumpResult struct {
	exited   bool
	code     int
	job      string
	rerr     *RemoteError
	err      error
	timedOut bool
	aborted  bool
}

// pumpConfig tailors the frame loop to a command.
type pumpConfig struct {
	stdout, stderr io.Writer
	// onSignal handles an interrupt signal. count is 1 for the first one. It
	// returns true to abandon the session at once.
	onSignal func(s *session, sig os.Signal, count int) bool
	// onFrame sees every frame that the loop does not handle itself.
	onFrame func(s *session, f wire.Frame) error
	// offset, when set, advances by the size of every output record that
	// arrives after the job notice, so that a reconnect resumes at a record
	// boundary without repeating output.
	offset *int64
}

// pump relays frames until the agent sends exit, the stream fails, or the
// caller abandons the session.
func (r *Runner) pump(ctx context.Context, s *session, sigs <-chan os.Signal, pc pumpConfig) pumpResult {
	var res pumpResult
	signals := 0
	for {
		select {
		case msg := <-s.frames:
			if msg.err != nil {
				res.err = msg.err
				res.timedOut = s.timedOut.Load()
				return res
			}
			s.lastRecv.Store(nowNano())
			f := msg.f
			switch f.Type {
			case wire.TypeStdout:
				if pc.stdout != nil {
					if _, err := pc.stdout.Write(f.Payload); err != nil {
						res.err = err
						return res
					}
				}
				if pc.offset != nil && res.job != "" {
					*pc.offset += jobs.RecordSize(len(f.Payload))
				}
			case wire.TypeStderr:
				if pc.stderr != nil {
					_, _ = pc.stderr.Write(f.Payload)
				}
				if pc.offset != nil && res.job != "" {
					*pc.offset += jobs.RecordSize(len(f.Payload))
				}
			case wire.TypeError:
				var m wire.ErrorMsg
				if err := f.JSON(&m); err != nil {
					res.err = err
					return res
				}
				res.rerr = &RemoteError{Code: m.Code, Message: m.Message}
			case wire.TypeExit:
				var e wire.Exit
				if err := f.JSON(&e); err != nil {
					res.err = err
					return res
				}
				res.exited, res.code = true, e.Code
				if e.Job != "" {
					res.job = e.Job
				}
				return res
			case wire.TypeJobNotice:
				var n wire.JobNotice
				if err := f.JSON(&n); err != nil {
					res.err = err
					return res
				}
				res.job = n.ID
				if pc.onFrame != nil {
					if err := pc.onFrame(s, f); err != nil {
						res.err = err
						return res
					}
				}
			case wire.TypePing:
				_ = s.send(wire.TypePong, nil)
			case wire.TypePong:
			default:
				if pc.onFrame != nil {
					if err := pc.onFrame(s, f); err != nil {
						res.err = err
						return res
					}
				}
			}
		case sig := <-sigs:
			signals++
			if pc.onSignal != nil && pc.onSignal(s, sig, signals) {
				res.aborted = true
				return res
			}
		case <-s.done:
			// The monitor gave up on a silent agent. The reader may have
			// dropped its error because the session was already closed.
			res.err = errors.New("session closed")
			res.timedOut = s.timedOut.Load()
			return res
		case <-ctx.Done():
			res.aborted = true
			return res
		}
	}
}

// cancelThenAbandon is the interrupt policy of commands that are not jobs:
// the first interrupt asks the agent to cancel, the second gives up.
func cancelThenAbandon(s *session, _ os.Signal, count int) bool {
	if count == 1 {
		_ = s.send(wire.TypeCancel, nil)
		return false
	}
	return true
}

// lockedWriter serializes writes from several goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Run executes one invocation and returns the exit status.
func (r *Runner) Run(ctx context.Context, inv Invocation) int {
	r.defaults()
	if inv.Stdout == nil {
		inv.Stdout = io.Discard
	}
	if inv.Stderr == nil {
		inv.Stderr = io.Discard
	}
	// The ssh process and this client both write diagnostics here.
	inv.Stderr = &lockedWriter{w: inv.Stderr}
	switch {
	case lifecycleCommands[inv.Command] || inv.Command == "jobs-attach":
		return r.runJob(ctx, inv)
	case inv.Command == "exec" || inv.Command == "router-exec":
		return r.runExec(ctx, inv)
	case inv.Command == "capture":
		return r.runCapture(ctx, inv)
	}
	return r.runSimple(ctx, inv)
}

// lifecycleCommands run as jobs on the host.
var lifecycleCommands = map[string]bool{"up": true, "start": true, "stop": true, "restart": true, "down": true}

// runSimple runs a command that lives and dies with its connection.
func (r *Runner) runSimple(ctx context.Context, inv Invocation) int {
	req, err := BuildRequest(inv)
	if err != nil {
		Print(inv.Stderr, err)
		return ExitFailure
	}
	sigs, stop := r.Notify()
	defer stop()
	s, err := r.open(ctx, &inv)
	if err != nil {
		Print(inv.Stderr, err)
		return ExitFailure
	}
	defer s.close()
	if err := s.sendJSON(wire.TypeRequest, req); err != nil {
		Print(inv.Stderr, fmt.Errorf("send request: %w", err))
		return ExitFailure
	}
	res := r.pump(ctx, s, sigs, pumpConfig{stdout: inv.Stdout, stderr: inv.Stderr, onSignal: cancelThenAbandon})
	return r.report(inv, res)
}

// report prints what went wrong and chooses the exit status.
func (r *Runner) report(inv Invocation, res pumpResult) int {
	if res.rerr != nil {
		Print(inv.Stderr, res.rerr)
	}
	if res.exited {
		return res.code
	}
	switch {
	case res.aborted:
		return ExitFailure
	case res.timedOut:
		Print(inv.Stderr, errors.New("the agent stopped answering"))
	case res.err != nil:
		Print(inv.Stderr, fmt.Errorf("connection to the agent was lost: %w", res.err))
	}
	return ExitFailure
}
