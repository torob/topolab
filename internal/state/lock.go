package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Lock struct {
	file *os.File
	path string
}

// AcquireLock obtains the topology mutation lock. A deadline or cancellation
// can bound contention; lock acquisition itself never blocks the goroutine in
// the kernel indefinitely.
func AcquireLock(ctx context.Context, path string) (*Lock, error) {
	return acquireLock(ctx, path, syscall.LOCK_EX, nil)
}

// AcquireLockNotify is AcquireLock that calls waiting once, before it polls
// again, when another holder owns the lock.
func AcquireLockNotify(ctx context.Context, path string, waiting func()) (*Lock, error) {
	return acquireLock(ctx, path, syscall.LOCK_EX, waiting)
}

// AcquireSharedLock allows read-only long-lived operations such as exec to
// coexist while making lifecycle mutation wait.
func AcquireSharedLock(ctx context.Context, path string) (*Lock, error) {
	return acquireLock(ctx, path, syscall.LOCK_SH, nil)
}

func acquireLock(ctx context.Context, path string, mode int, waiting func()) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	notified := false
	for {
		err = syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			return &Lock{file: f, path: path}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if waiting != nil && !notified {
			notified = true
			waiting()
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err1 := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err2 := l.file.Close()
	l.file = nil
	return errors.Join(err1, err2)
}

func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}
