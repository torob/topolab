package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type StartedProcess struct {
	PID int
	Cmd *exec.Cmd
}

// Start launches QEMU without a shell. args must include argv[0].
func Start(args []string, extraFiles []*os.File) (*StartedProcess, error) {
	if len(args) == 0 {
		return nil, errors.New("empty QEMU argv")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.ExtraFiles = extraFiles
	// Keep the long-lived guest detached from the invoking CLI's standard
	// streams. In particular, inheriting a pipe-backed stderr would keep that
	// pipe open after topolab exits and make non-interactive callers hang.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	return &StartedProcess{PID: cmd.Process.Pid, Cmd: cmd}, nil
}

type ProcessOps interface {
	Alive(pid int) bool
	CommandLine(pid int) ([]string, error)
	Signal(pid int, signal os.Signal) error
}

type OSProcessOps struct{}

func (OSProcessOps) Alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "State:") {
			return !strings.Contains(line, "Z (zombie)")
		}
	}
	return true
}
func (OSProcessOps) CommandLine(pid int) ([]string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	raw := strings.Split(string(b), "\x00")
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}
	return raw, nil
}
func (OSProcessOps) Signal(pid int, signal os.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(signal)
}

type Powerdowner interface {
	Powerdown(context.Context) error
	Close() error
}

type Stopper struct {
	Processes       ProcessOps
	OpenQMP         func(string) Powerdowner
	GracefulTimeout time.Duration
	TermTimeout     time.Duration
	PollInterval    time.Duration
}

func NewStopper() *Stopper {
	return &Stopper{Processes: OSProcessOps{}, OpenQMP: func(path string) Powerdowner { return NewQMPClient(path, nil) }, GracefulTimeout: 30 * time.Second, TermTimeout: 5 * time.Second, PollInterval: 100 * time.Millisecond}
}

// Stop verifies process ownership before QMP and signal fallbacks.
func (s *Stopper) Stop(ctx context.Context, pid int, topology, node, qmpPath string) error {
	if pid <= 0 {
		return errors.New("invalid PID")
	}
	if s.Processes == nil {
		s.Processes = OSProcessOps{}
	}
	want := "topolab:" + topology + ":" + node
	if err := VerifyProcessIdentity(s.Processes, pid, want); err != nil {
		return err
	}
	if !s.Processes.Alive(pid) {
		return nil
	}
	if s.OpenQMP != nil {
		client := s.OpenQMP(qmpPath)
		qctx, cancel := context.WithTimeout(ctx, minDuration(s.GracefulTimeout, 5*time.Second))
		_ = client.Powerdown(qctx)
		cancel()
		_ = client.Close()
	}
	if waitDead(ctx, s.Processes, pid, s.GracefulTimeout, s.PollInterval) {
		return nil
	}
	// The original process may have exited and its PID may have been reused
	// while graceful shutdown was pending. Revalidate immediately before each
	// destructive signal.
	if err := VerifyProcessIdentity(s.Processes, pid, want); err != nil {
		if !s.Processes.Alive(pid) {
			return nil
		}
		return err
	}
	if err := s.Processes.Signal(pid, syscall.SIGTERM); err != nil && s.Processes.Alive(pid) {
		return err
	}
	if waitDead(ctx, s.Processes, pid, s.TermTimeout, s.PollInterval) {
		return nil
	}
	if err := VerifyProcessIdentity(s.Processes, pid, want); err != nil {
		if !s.Processes.Alive(pid) {
			return nil
		}
		return err
	}
	if err := s.Processes.Signal(pid, syscall.SIGKILL); err != nil && s.Processes.Alive(pid) {
		return err
	}
	if !waitDead(ctx, s.Processes, pid, s.TermTimeout, s.PollInterval) {
		return fmt.Errorf("PID %d did not exit after SIGKILL", pid)
	}
	return nil
}

func VerifyProcessIdentity(processes ProcessOps, pid int, want string) error {
	cmdline, err := processes.CommandLine(pid)
	if err != nil {
		if !processes.Alive(pid) {
			return nil
		}
		return fmt.Errorf("verify QEMU PID %d: %w", pid, err)
	}
	for _, arg := range cmdline {
		if arg == "guest="+want+",debug-threads=on" {
			return nil
		}
	}
	return fmt.Errorf("PID %d does not identify expected node %s", pid, want)
}

func waitDead(ctx context.Context, p ProcessOps, pid int, timeout, poll time.Duration) bool {
	if timeout <= 0 {
		return !p.Alive(pid)
	}
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if !p.Alive(pid) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return !p.Alive(pid)
		case <-t.C:
		}
	}
}
func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 || a > b {
		return b
	}
	return a
}
