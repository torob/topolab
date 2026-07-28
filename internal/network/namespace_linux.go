//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// NamespaceOps is the single injectable seam for namespace-scoped commands,
// processes, temporary setns work, PID discovery, and identity comparison.
type NamespaceOps interface {
	Run(context.Context, string, string, ...string) ([]byte, error)
	Start(string, []string, []*os.File) (int, *exec.Cmd, error)
	With(string, func() error) error
	PIDs(context.Context, string) ([]int, error)
	SameProcess(int, string) (bool, error)
}

type OSNamespaceOps struct{ Runner CommandRunner }

func (o OSNamespaceOps) runner() CommandRunner {
	if o.Runner == nil {
		return ExecRunner{}
	}
	return o.Runner
}

func (o OSNamespaceOps) Run(ctx context.Context, namespace, program string, args ...string) ([]byte, error) {
	if namespace == "" {
		return o.runner().Run(ctx, program, args...)
	}
	nsargs := append([]string{"netns", "exec", namespace, program}, args...)
	out, err := o.runner().Run(ctx, "ip", nsargs...)
	if err != nil {
		output := string(out)
		var commandErr *CommandError
		if output == "" && errors.As(err, &commandErr) {
			output = commandErr.Output
		}
		return out, &CommandError{Program: program, Args: append([]string(nil), args...), Output: output, Err: err}
	}
	return out, nil
}

func (o OSNamespaceOps) Start(namespace string, args []string, files []*os.File) (int, *exec.Cmd, error) {
	if len(args) == 0 {
		return 0, nil, errors.New("empty process argv")
	}
	program, argv := args[0], args[1:]
	if namespace != "" {
		program = "ip"
		argv = append([]string{"netns", "exec", namespace, args[0]}, args[1:]...)
	}
	cmd := exec.Command(program, argv...)
	cmd.ExtraFiles = files
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	return cmd.Process.Pid, cmd, nil
}

// With locks the current goroutine to one OS thread, enters the requested
// namespace, and restores the original namespace even after callback panic.
func (o OSNamespaceOps) With(namespace string, fn func() error) (err error) {
	if namespace == "" {
		return fn()
	}
	return withNamespaceFiles("/var/run/netns/"+namespace, fn, func(fd int) error {
		return unix.Setns(fd, unix.CLONE_NEWNET)
	})
}

func withNamespaceFiles(targetPath string, fn func() error, setns func(int) error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	original, err := os.Open("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("open current network namespace: %w", err)
	}
	defer original.Close()
	target, err := os.Open(targetPath)
	if err != nil {
		return fmt.Errorf("open network namespace %s: %w", targetPath, err)
	}
	defer target.Close()
	if err := setns(int(target.Fd())); err != nil {
		return fmt.Errorf("enter network namespace %s: %w", targetPath, err)
	}
	defer func() {
		restoreErr := setns(int(original.Fd()))
		if recovered := recover(); recovered != nil {
			if restoreErr != nil {
				panic(fmt.Sprintf("callback panic: %v; restore network namespace: %v", recovered, restoreErr))
			}
			panic(recovered)
		}
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore network namespace: %w", restoreErr))
		}
	}()
	return fn()
}

func (o OSNamespaceOps) PIDs(ctx context.Context, namespace string) ([]int, error) {
	out, err := o.runner().Run(ctx, "ip", "netns", "pids", namespace)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		pid, parseErr := strconv.Atoi(field)
		if parseErr != nil || pid <= 0 {
			return nil, fmt.Errorf("invalid PID %q listed for namespace %s", field, namespace)
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

func (o OSNamespaceOps) SameProcess(pid int, namespace string) (bool, error) {
	process, err := os.Stat("/proc/" + strconv.Itoa(pid) + "/ns/net")
	if err != nil {
		return false, err
	}
	target, err := os.Stat("/var/run/netns/" + namespace)
	if err != nil {
		return false, err
	}
	return os.SameFile(process, target), nil
}
