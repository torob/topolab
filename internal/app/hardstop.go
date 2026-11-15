package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
)

// HardStopTimeout is the fixed time that stop --hard waits for each QEMU
// process to exit after SIGTERM.
const HardStopTimeout = 30 * time.Second

// Results that stop --hard prints for each selected VM.
const (
	hardStopStopped    = "stopped"
	hardStopNotExited  = "not-exited"
	hardStopNotRunning = "not-running"
)

// hardStopTimeLayout is UTC with milliseconds.
const hardStopTimeLayout = "2006-01-02T15:04:05.000Z"

// splitStopArgs separates the --hard option from the VM names. Every other
// argument stays a VM name, so the normal stop keeps its exact errors. A VM
// name cannot start with "-".
func splitStopArgs(args []string) (bool, []string, error) {
	hard := false
	names := make([]string, 0, len(args))
	for _, arg := range args {
		if arg != "--hard" {
			names = append(names, arg)
			continue
		}
		if hard {
			return false, nil, errors.New("stop --hard may be specified only once")
		}
		hard = true
	}
	return hard, names, nil
}

// acquireLifecycleLocks takes the network lock and then the VM lock, and
// returns the function that releases both. A command that changes only VMs
// (start and stop --hard) takes the network lock shared, so it runs beside
// exec and router exec. Every other lifecycle command takes the network lock
// exclusive, as before. The VM lock is always exclusive, so only one
// lifecycle command changes VMs or writes the manifest at a time. The lock
// order is always the same, so two commands cannot deadlock.
func (a *App) acquireLifecycleLocks(ctx context.Context, layout state.Layout, vmOnly bool) (func(), error) {
	notice := sync.OnceFunc(func() { fmt.Fprintln(a.Stderr, LockWaitNotice) })
	acquireNetwork := state.AcquireLockNotify
	if vmOnly {
		acquireNetwork = state.AcquireSharedLockNotify
	}
	networkLock, err := acquireNetwork(ctx, layout.LockPath, notice)
	if err != nil {
		return nil, err
	}
	vmLock, err := state.AcquireLockNotify(ctx, layout.VMLockPath, notice)
	if err != nil {
		_ = networkLock.Close()
		return nil, err
	}
	return func() {
		_ = vmLock.Close()
		_ = networkLock.Close()
	}, nil
}

type hardStopTarget struct {
	name     string
	node     *state.Node
	pid      int
	handle   qemu.ExitHandle
	signaled bool
	result   string
	stopTime time.Time
	exitTime time.Time
	err      error
}

// hardStop simulates a simultaneous power loss of the named VMs. It checks
// every running VM before any VM gets a signal. Then it sends SIGTERM to all
// QEMU processes in one loop, so QEMU exits through its normal path and
// writes its disk data to the host first. It never sends an ACPI event or
// SIGKILL. A process that does not exit within HardStopTimeout stays
// running, keeps its PID in the manifest, and makes the command fail.
func (a *App) hardStop(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, names []string) error {
	targets, err := a.hardStopVMs(ctx, cfg, layout, m, names)
	if targets != nil {
		a.printHardStop(targets)
	}
	return err
}

// hardStopVMs does the work of stop --hard without output. It returns nil
// targets when a check failed and no VM got a signal.
func (a *App) hardStopVMs(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, names []string) ([]*hardStopTarget, error) {
	targets := make([]*hardStopTarget, 0, len(names))
	defer func() {
		for _, t := range targets {
			if t.handle != nil {
				_ = t.handle.Close()
			}
		}
	}()

	// Prepare. No VM gets a signal until every running VM passes its checks.
	for _, name := range names {
		t := &hardStopTarget{name: name, node: m.Nodes[name], result: hardStopNotRunning}
		targets = append(targets, t)
		if err := a.prepareHardStop(cfg.Name, t); err != nil {
			return nil, fmt.Errorf("stop --hard: no VM was stopped: VM %s: %w", name, err)
		}
	}

	// Fire. One loop sends SIGTERM to every prepared VM.
	for _, t := range targets {
		if t.handle == nil {
			continue
		}
		err := t.handle.Terminate()
		at := a.Now()
		switch {
		case err == nil:
			t.signaled, t.stopTime = true, at
		case errors.Is(err, qemu.ErrProcessExited):
			t.result, t.pid = hardStopNotRunning, 0
		default:
			t.result = hardStopNotExited
			t.err = fmt.Errorf("stop --hard: send SIGTERM to VM %s (PID %d): %w; the process is left running", t.name, t.pid, err)
		}
	}

	// Wait. Each VM has HardStopTimeout from its own SIGTERM.
	var wg sync.WaitGroup
	for _, t := range targets {
		if !t.signaled {
			continue
		}
		wg.Go(func() {
			exited, err := t.handle.WaitExit(ctx, a.HardStopTimeout)
			at := a.Now()
			switch {
			case exited:
				t.result, t.exitTime = hardStopStopped, at
			case err != nil:
				t.result = hardStopNotExited
				t.err = fmt.Errorf("stop --hard: wait for VM %s (PID %d) to exit: %w; the process is left running", t.name, t.pid, err)
			default:
				t.result = hardStopNotExited
				t.err = fmt.Errorf("stop --hard: VM %s (PID %d) did not exit within %s after SIGTERM; the process is left running", t.name, t.pid, a.HardStopTimeout)
			}
		})
	}
	wg.Wait()

	// Finish. A VM that ended gets the same state as after a normal stop. A
	// VM that did not exit keeps its PID for the operator.
	var errs []error
	for _, t := range targets {
		if t.node != nil && t.result != hardStopNotExited {
			if err := a.finishNodeStop(t.node); err != nil {
				errs = append(errs, fmt.Errorf("stop --hard: record the stop of VM %s: %w", t.name, err))
			}
		}
		if t.err != nil {
			errs = append(errs, t.err)
		}
	}
	if err := state.Save(layout.ManifestPath, m); err != nil {
		errs = append(errs, err)
	}
	return targets, errors.Join(errs...)
}

// prepareHardStop opens an exit handle for a running VM and makes the same
// ownership checks as the normal stop. The handle is open before the checks,
// so the checked process is the one that gets the signal.
func (a *App) prepareHardStop(topology string, t *hardStopTarget) error {
	n := t.node
	if n == nil || n.PID <= 0 || !a.Processes.Alive(n.PID) {
		return nil
	}
	handle, err := a.Pidfds.Open(n.PID)
	if err != nil {
		if errors.Is(err, qemu.ErrProcessExited) || !a.Processes.Alive(n.PID) {
			return nil
		}
		return err
	}
	t.handle = handle
	err = a.verifyNodeNamespace(n)
	if err == nil {
		err = qemu.VerifyProcessIdentity(a.Processes, n.PID, "topolab:"+topology+":"+t.name)
	}
	// A process that ended during the checks is not running. Its PID can now
	// name another process, so a failed check does not count then.
	if handle.Exited() {
		_ = handle.Close()
		t.handle = nil
		return nil
	}
	if err != nil {
		return err
	}
	t.pid = n.PID
	return nil
}

func (a *App) printHardStop(targets []*hardStopTarget) {
	fmt.Fprintln(a.Stdout, "NAME\tRESULT\tPID\tSTOP_TIME\tEXIT_TIME")
	for _, t := range targets {
		pid := "-"
		if t.pid > 0 {
			pid = fmt.Sprint(t.pid)
		}
		fmt.Fprintf(a.Stdout, "%s\t%s\t%s\t%s\t%s\n", t.name, t.result, pid, formatHardStopTime(t.stopTime), formatHardStopTime(t.exitTime))
	}
}

func formatHardStopTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(hardStopTimeLayout)
}
