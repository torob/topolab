package network

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, &CommandError{Program: program, Args: append([]string(nil), args...), Output: string(out), Err: err}
	}
	return out, nil
}

type CommandError struct {
	Program string
	Args    []string
	Output  string
	Err     error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("%s %v: %v: %s", e.Program, e.Args, e.Err, e.Output)
}

func (e *CommandError) Unwrap() error { return e.Err }

func commandReportsNotFound(err error) bool {
	if err == nil {
		return false
	}
	// A missing executable is a host precondition/cleanup failure, never
	// evidence that the resource inspected by that executable is absent.
	if errors.Is(err, exec.ErrNotFound) {
		return false
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(commandErr.Output))
	var fragments []string
	switch commandErr.Program {
	case "ip":
		fragments = []string{"cannot find device", "does not exist", "rtnetlink answers: no such process"}
	case "nft":
		fragments = []string{"no such file or directory"}
	default:
		return false
	}
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}
