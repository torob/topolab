package qemu

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func startChild(t *testing.T, script string) (*exec.Cmd, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // test cleanup of the test's own child only
		_ = cmd.Wait()
	})
	return cmd, bufio.NewReader(stdout)
}

func TestPidfdTerminateEndsProcessWithSIGTERM(t *testing.T) {
	cmd, out := startChild(t, "echo ready; exec sleep 60")
	if _, err := out.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	handle, err := OSPidfdOps{}.Open(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if handle.Exited() {
		t.Fatal("a running process reports that it exited")
	}
	if err := handle.Terminate(); err != nil {
		t.Fatal(err)
	}
	exited, err := handle.WaitExit(context.Background(), 5*time.Second)
	if err != nil || !exited {
		t.Fatalf("WaitExit = %t, %v; want the exit", exited, err)
	}
	if !handle.Exited() {
		t.Fatal("Exited is false after the exit")
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("wait = %v", err)
	}
	if status := exitErr.Sys().(syscall.WaitStatus); !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("child status = %v, want the SIGTERM signal", status)
	}
}

func TestPidfdWaitExitTimesOutAndLeavesProcessRunning(t *testing.T) {
	// The exec'd sleep keeps the ignored SIGTERM disposition.
	cmd, out := startChild(t, `trap "" TERM; echo ready; exec sleep 60`)
	if _, err := out.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	handle, err := OSPidfdOps{}.Open(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := handle.Terminate(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	exited, err := handle.WaitExit(context.Background(), 300*time.Millisecond)
	if err != nil || exited {
		t.Fatalf("WaitExit = %t, %v; want a timeout", exited, err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("WaitExit returned after %s, before its timeout", elapsed)
	}
	if handle.Exited() || syscall.Kill(cmd.Process.Pid, 0) != nil {
		t.Fatal("the process does not run after the timeout")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if exited, err := handle.WaitExit(ctx, time.Minute); exited || !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitExit with a cancelled context = %t, %v", exited, err)
	}
}

func TestPidfdTerminateAfterExitReportsProcessExited(t *testing.T) {
	cmd, out := startChild(t, "echo ready; read line")
	if _, err := out.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	handle, err := OSPidfdOps{}.Open(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := cmd.Process.Kill(); err != nil { // test-only end of the child
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := handle.Terminate(); !errors.Is(err, ErrProcessExited) {
		t.Fatalf("Terminate = %v, want ErrProcessExited", err)
	}
	if _, err := (OSPidfdOps{}).Open(0); err == nil {
		t.Fatal("Open(0) succeeded")
	}
}
