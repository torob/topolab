package qemu

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrProcessExited reports that the process of an ExitHandle ended before
// the handle could signal it.
var ErrProcessExited = errors.New("process already exited")

// ExitHandle refers to one exact process. A reused PID never receives its
// signal. The handle can send only SIGTERM: it has no way to send SIGKILL.
type ExitHandle interface {
	// Terminate sends SIGTERM. It returns ErrProcessExited when the process
	// already ended.
	Terminate() error
	// Exited reports whether the process ended, without waiting.
	Exited() bool
	// WaitExit waits until the process ends, the timeout ends, or ctx ends.
	// It returns true only when the process ended.
	WaitExit(ctx context.Context, timeout time.Duration) (bool, error)
	Close() error
}

// PidfdOps opens exit handles for PIDs.
type PidfdOps interface {
	Open(pid int) (ExitHandle, error)
}

// OSPidfdOps opens Linux pidfds (Linux 5.3 or later).
type OSPidfdOps struct{}

func (OSPidfdOps) Open(pid int) (ExitHandle, error) {
	if pid <= 0 {
		return nil, errors.New("invalid PID")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil, ErrProcessExited
		}
		return nil, fmt.Errorf("open pidfd for PID %d: %w", pid, err)
	}
	return &pidfdHandle{fd: fd}, nil
}

type pidfdHandle struct {
	mu sync.Mutex
	fd int
}

// waitSlice bounds one poll call, so a cancelled context stops a wait soon.
const waitSlice = 100 * time.Millisecond

func (h *pidfdHandle) Terminate() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fd < 0 {
		return errors.New("pidfd is closed")
	}
	if err := unix.PidfdSendSignal(h.fd, unix.SIGTERM, nil, 0); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return ErrProcessExited
		}
		return err
	}
	return nil
}

func (h *pidfdHandle) Exited() bool {
	exited, err := h.poll(0)
	return err == nil && exited
}

func (h *pidfdHandle) WaitExit(ctx context.Context, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return h.poll(0)
		}
		exited, err := h.poll(min(remaining, waitSlice))
		if err != nil || exited {
			return exited, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
}

// poll reports whether the process ended. A pidfd becomes readable when its
// process ends; the parent does not have to reap it first.
func (h *pidfdHandle) poll(wait time.Duration) (bool, error) {
	h.mu.Lock()
	fd := h.fd
	h.mu.Unlock()
	if fd < 0 {
		return false, errors.New("pidfd is closed")
	}
	ms := 0
	if wait > 0 {
		ms = max(int(wait/time.Millisecond), 1)
	}
	for {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, ms)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("poll pidfd: %w", err)
		}
		return n > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
	}
}

func (h *pidfdHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fd < 0 {
		return nil
	}
	err := unix.Close(h.fd)
	h.fd = -1
	return err
}
