package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

// DefaultInstallRoot is where agents are installed on the host.
const DefaultInstallRoot = "/usr/local/lib/topolab"

// AgentPath is the path of the agent for a digest.
func AgentPath(root, digest string) string {
	if root == "" {
		root = DefaultInstallRoot
	}
	return root + "/" + digest + "/topolab"
}

// RemoteScript is the constant script that the host's shell runs. Only the
// agent path varies. It contains no single quote, backslash, or exclamation
// mark, so every common login shell passes it unchanged (spec.md Section 12.3).
//
// Exit status 97 means that the agent is missing, and 98 means that sudo does
// not allow the agent without a password.
func RemoteScript(agentPath string) string {
	return `a=` + agentPath + `; ` +
		`[ -x "$a" ] || exit 97; ` +
		`if [ "$(id -u)" = 0 ]; then exec "$a" agent --stdio; fi; ` +
		`sudo -n -l "$a" agent --stdio >/dev/null 2>&1 || exit 98; ` +
		`exec sudo -n "$a" agent --stdio`
}

// SSHBaseArgs builds the ssh arguments up to and including the destination.
// User options come first so that they take precedence: ssh uses the first
// value that it obtains for an option. tty selects -t instead of -T.
func SSHBaseArgs(tty bool, options []string, destination string) []string {
	args := []string{"-T"}
	if tty {
		args = []string{"-t"}
	}
	for _, o := range options {
		args = append(args, "-o", o)
	}
	return append(args,
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"--", destination,
	)
}

// SSHArgs builds the ssh argument vector that runs the agent.
func SSHArgs(options []string, destination, agentPath string) []string {
	return append(SSHBaseArgs(false, options, destination), "sh -c '"+RemoteScript(agentPath)+"'")
}

// SSHDialer starts the agent through the system ssh client.
type SSHDialer struct {
	// Path is the ssh executable; empty means "ssh" from PATH.
	Path        string
	Digest      string
	InstallRoot string
}

// Dial implements Dialer.
func (d SSHDialer) Dial(ctx context.Context, inv Invocation) (*Transport, error) {
	path := d.Path
	if path == "" {
		path = "ssh"
	}
	args := SSHArgs(inv.SSHOptions, inv.Destination, AgentPath(d.InstallRoot, d.Digest))
	return startProcess(path, args, inv.Stderr)
}

// startProcess runs a program with its stdin and stdout as the transport.
func startProcess(path string, args []string, stderr io.Writer) (*Transport, error) {
	cmd := exec.Command(path, args...)
	in, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stderr = stderr
	// The transport gets its own process group. A Ctrl-C in the controller's
	// terminal then reaches only the controller, which decides what an interrupt
	// means (cancel, detach); it never kills ssh behind the controller's back.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", path, err)
	}
	var once sync.Once
	var waitErr error
	wait := func() error {
		once.Do(func() { waitErr = cmd.Wait() })
		return waitErr
	}
	return &Transport{
		In:   in,
		Out:  out,
		Wait: wait,
		Kill: func() {
			if cmd.Process != nil {
				// Everything in the group: a wrapper that did not exec ssh must
				// not outlive the connection.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		},
	}, nil
}

// ErrNoSSH is returned when no ssh client can be found.
var ErrNoSSH = errors.New("ssh client not found in PATH")

// LookSSH finds the ssh executable.
func LookSSH() (string, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return "", ErrNoSSH
	}
	return path, nil
}
