package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/preflight"
	"github.com/torob/topolab/internal/state"
)

// lockTestLayout returns a saved empty manifest for topology "lab".
func lockTestLayout(t *testing.T) (string, state.Layout) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "tllock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	layout, err := state.NewLayout(base, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(layout.ManifestPath, state.NewManifest("lab")); err != nil {
		t.Fatal(err)
	}
	return base, layout
}

func holdLock(t *testing.T, path string, shared bool) *state.Lock {
	t.Helper()
	acquire := state.AcquireLock
	if shared {
		acquire = state.AcquireSharedLock
	}
	lock, err := acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return lock
}

// runLocked runs one lifecycle command with a short deadline.
func runLocked(t *testing.T, base, name string, args ...string) (string, error) {
	t.Helper()
	var stderr bytes.Buffer
	a := New(&bytes.Buffer{}, &stderr)
	a.Preflight = func(preflight.Options) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := a.Run(ctx, Command{Name: name, Args: args, WorkingDir: base, Topology: &config.Config{Name: "lab"}})
	return stderr.String(), err
}

func TestVMOnlyCommandsRunBesideExec(t *testing.T) {
	base, layout := lockTestLayout(t)
	holdLock(t, layout.LockPath, true) // as exec and router exec do
	stderr, err := runLocked(t, base, "stop", "--hard")
	if err != nil || stderr != "" {
		t.Fatalf("stop --hard: err = %v, stderr = %q; want no wait", err, stderr)
	}
	// start passes the locks and then stops at its own network check.
	stderr, err = runLocked(t, base, "start")
	if err == nil || !strings.Contains(err.Error(), "managed network is absent") || stderr != "" {
		t.Fatalf("start: err = %v, stderr = %q; want no wait", err, stderr)
	}
}

func TestOtherLifecycleCommandsStillWaitForExec(t *testing.T) {
	base, layout := lockTestLayout(t)
	holdLock(t, layout.LockPath, true)
	for _, name := range []string{"stop", "restart", "down"} {
		stderr, err := runLocked(t, base, name)
		if !errors.Is(err, context.DeadlineExceeded) || stderr != LockWaitNotice+"\n" {
			t.Errorf("%s: err = %v, stderr = %q; want a wait with one notice", name, err, stderr)
		}
	}
}

func TestVMCommandsWaitForTheVMLock(t *testing.T) {
	base, layout := lockTestLayout(t)
	holdLock(t, layout.VMLockPath, false) // as a running start does
	for _, args := range [][]string{{"stop", "--hard"}, {"start"}, {"stop"}, {"down"}} {
		stderr, err := runLocked(t, base, args[0], args[1:]...)
		if !errors.Is(err, context.DeadlineExceeded) || stderr != LockWaitNotice+"\n" {
			t.Errorf("%q: err = %v, stderr = %q; want a wait with one notice", args, err, stderr)
		}
	}
}

func TestLockWaitNoticeAppearsOnceForBothLocks(t *testing.T) {
	base, layout := lockTestLayout(t)
	network := holdLock(t, layout.LockPath, false)
	holdLock(t, layout.VMLockPath, false)
	// The command first waits for the network lock, then for the VM lock.
	released := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() {
		_ = network.Close()
		close(released)
	})
	stderr, err := runLocked(t, base, "stop", "--hard")
	<-released
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if stderr != LockWaitNotice+"\n" {
		t.Fatalf("stderr = %q, want one notice", stderr)
	}
}

func TestHardStopOptionMayAppearOnlyOnce(t *testing.T) {
	base, _ := lockTestLayout(t)
	_, err := runLocked(t, base, "stop", "--hard", "--hard")
	if err == nil || !strings.Contains(err.Error(), "only once") {
		t.Fatalf("err = %v", err)
	}
}
