package agent

import (
	"os"
	"os/exec"
	"syscall"
)

// DefaultSpawn starts the runner of a job: the agent's own executable with the
// internal __job subcommand, in its own session and with no standard stream
// attached to the SSH session. The runner outlives the agent session.
func DefaultSpawn() func(dir string) error {
	return func(dir string) error {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, "__job", dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		// Reap the runner while this process lives; afterwards init adopts it.
		go func() { _ = cmd.Wait() }()
		return nil
	}
}
