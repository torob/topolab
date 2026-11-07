package client

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/torob/topolab/internal/remote/wire"
)

// fatalStart reports errors that retrying cannot fix.
func fatalStart(err error) bool {
	if re, ok := err.(*RemoteError); ok {
		switch re.Code {
		case wire.CodeAgentMissing, wire.CodeAgentMismatch, wire.CodePrivilegeDenied:
			return true
		}
	}
	return false
}

// runJob runs a lifecycle command or attaches to a job. The job outlives the
// connection: a lost connection is retried with backoff until the reconnect
// timeout, and the controller then exits with status 3 and leaves the job
// running (spec.md Section 12.6). A corrupted template transfer is retried once
// from a fresh hash.
func (r *Runner) runJob(ctx context.Context, inv Invocation) int {
	for attempt := 0; ; attempt++ {
		code, retry := r.runJobOnce(ctx, inv, attempt)
		if !retry {
			return code
		}
	}
}

func (r *Runner) runJobOnce(ctx context.Context, inv Invocation, attempt int) (int, bool) {
	var files map[string]string
	if inv.Command == "up" {
		var err error
		files, err = r.prepareTemplates(&inv)
		if err != nil {
			Print(inv.Stderr, err)
			return ExitFailure, false
		}
	}
	if lifecycleCommands[inv.Command] && inv.JobID == "" {
		id, err := wire.NewNonce()
		if err != nil {
			Print(inv.Stderr, err)
			return ExitFailure, false
		}
		inv.JobID = id
	}
	req, err := BuildRequest(inv)
	if err != nil {
		Print(inv.Stderr, err)
		return ExitFailure, false
	}
	sigs, stop := r.Notify()
	defer stop()

	var (
		offset    int64
		jobID     = inv.JobID
		connected bool
		lostSince time.Time
		backoff   = r.BackoffInitial
	)
	for {
		s, err := r.open(ctx, &inv)
		switch {
		case err != nil && (!connected || fatalStart(err)):
			Print(inv.Stderr, err)
			return ExitFailure, false
		case err == nil:
			connected = true
			lostSince = time.Time{}
			backoff = r.BackoffInitial
			res := r.attach(ctx, s, sigs, inv, req, files, &offset)
			s.close()
			if res.job != "" {
				jobID = res.job
			}
			if res.exited {
				if res.rerr != nil {
					Print(inv.Stderr, res.rerr)
					if res.rerr.Code == wire.CodeBlobDigest && attempt == 0 {
						fmt.Fprintln(inv.Stderr, "topolab: the template changed or was damaged during the upload; hashing it again")
						r.forgetTemplates(inv, files)
						return ExitFailure, true
					}
				}
				return res.code, false
			}
			if res.aborted {
				r.reportDetached(inv, jobID)
				return ExitDetached, false
			}
			if res.rerr != nil {
				Print(inv.Stderr, res.rerr)
			}
			if res.timedOut {
				fmt.Fprintln(inv.Stderr, "topolab: the agent stopped answering")
			} else if res.err != nil {
				fmt.Fprintf(inv.Stderr, "topolab: connection to the agent was lost: %v\n", res.err)
			}
			lostSince = time.Now()
		default: // a reconnection attempt failed
			if lostSince.IsZero() {
				lostSince = time.Now()
			}
		}
		if inv.ReconnectTimeout <= 0 || time.Since(lostSince) >= inv.ReconnectTimeout {
			r.reportDetached(inv, jobID)
			return ExitDetached, false
		}
		fmt.Fprintf(inv.Stderr, "topolab: reconnecting in %s\n", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-sigs:
			timer.Stop()
			r.reportDetached(inv, jobID)
			return ExitDetached, false
		case <-ctx.Done():
			timer.Stop()
			r.reportDetached(inv, jobID)
			return ExitDetached, false
		}
		if backoff *= 2; backoff > r.BackoffMax {
			backoff = r.BackoffMax
		}
	}
}

// attach sends the request on an open session and relays the job's output.
func (r *Runner) attach(ctx context.Context, s *session, sigs <-chan os.Signal, inv Invocation, req wire.Request, files map[string]string, offset *int64) pumpResult {
	req.Offset = *offset
	if err := s.sendJSON(wire.TypeRequest, req); err != nil {
		return pumpResult{err: fmt.Errorf("send request: %w", err)}
	}
	var up *uploader
	if len(files) > 0 {
		up = newUploader(req.Templates, files, inv.Stderr)
		defer up.close()
	}
	interrupts := 0
	return r.pump(ctx, s, sigs, pumpConfig{
		stdout: inv.Stdout,
		stderr: inv.Stderr,
		offset: offset,
		onFrame: func(s *session, f wire.Frame) error {
			if up != nil {
				return up.handle(s, f)
			}
			return nil
		},
		// The first SIGINT cancels the job and keeps the controller attached;
		// a second one, and SIGTERM or SIGHUP, detach without cancelling.
		onSignal: func(s *session, sig os.Signal, _ int) bool {
			if sig == syscall.SIGINT || sig == os.Interrupt {
				interrupts++
				if interrupts == 1 {
					_ = s.send(wire.TypeCancel, nil)
					fmt.Fprintln(inv.Stderr, "topolab: cancelling the job; press Ctrl-C again to detach")
					return false
				}
			}
			return true
		},
	})
}

// reportDetached tells the operator how to find the job again.
func (r *Runner) reportDetached(inv Invocation, jobID string) {
	if jobID != "" {
		fmt.Fprintf(inv.Stderr, "topolab: detached; job %s keeps running on %s; follow it with: topolab jobs attach %s\n", jobID, inv.Destination, jobID)
		return
	}
	fmt.Fprintf(inv.Stderr, "topolab: detached before the job was reported; list the jobs on %s with: topolab jobs ls\n", inv.Destination)
}
