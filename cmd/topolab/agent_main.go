package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/torob/topolab/internal/remote/agent"
	"github.com/torob/topolab/internal/remote/install"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// agentSearchPath is the PATH of the agent. It does not depend on what sudo or
// the login produces (spec.md Section 12.3).
const agentSearchPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// agentRefused is the exit status of an agent that is not run as root or is
// run on a terminal. The remote script maps it to privilege_denied.
const agentRefused = 99

type agentConfig struct {
	EUID       func() int
	IsTerminal func(fd int) bool
	Runner     agent.Runner
	Digest     func() (string, error)
	Version    string
}

// runAgent serves one session and returns the process exit status.
func runAgent(ctx context.Context, in io.Reader, out io.Writer, stderr io.Writer, stdinFD, stdoutFD int, cfg agentConfig) int {
	if cfg.EUID() != 0 {
		fmt.Fprintln(stderr, "topolab: the agent must run as root")
		return agentRefused
	}
	if cfg.IsTerminal(stdinFD) || cfg.IsTerminal(stdoutFD) {
		fmt.Fprintln(stderr, "topolab: agent --stdio is not an interactive command")
		return agentRefused
	}
	digest, err := cfg.Digest()
	if err != nil {
		fmt.Fprintf(stderr, "topolab: hash the agent executable: %v\n", err)
		return 1
	}
	err = agent.Serve(ctx, in, out, agent.Options{Digest: digest, Version: cfg.Version, Runner: cfg.Runner})
	if err != nil {
		fmt.Fprintf(stderr, "topolab: agent: %v\n", err)
		return 1
	}
	return 0
}

// protectStdout moves the protocol stream to a private descriptor that child
// processes do not inherit, and points descriptor 1 at standard error. A stray
// write to standard output then cannot corrupt the stream, and no child holds
// the SSH pipe open after the agent exits.
func protectStdout() (*os.File, error) {
	fd, err := unix.FcntlInt(1, unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, err
	}
	if err := unix.Dup3(2, 1, 0); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "agent-protocol"), nil
}

// agentMain is `topolab agent --stdio`.
func agentMain() int {
	signal.Ignore(syscall.SIGPIPE)
	cfg := agentConfig{
		EUID:       os.Geteuid,
		IsTerminal: term.IsTerminal,
		Runner:     agent.LocalRunner{},
		Digest:     func() (string, error) { d, _, err := install.SelfDigest(); return d, err },
		Version:    buildVersion(),
	}
	// Refuse before touching any descriptor.
	if cfg.EUID() != 0 || cfg.IsTerminal(0) || cfg.IsTerminal(1) {
		return runAgent(context.Background(), os.Stdin, io.Discard, os.Stderr, 0, 1, cfg)
	}
	_ = os.Setenv("PATH", agentSearchPath)
	out, err := protectStdout()
	if err != nil {
		fmt.Fprintf(os.Stderr, "topolab: protect standard output: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return runAgent(ctx, os.Stdin, out, os.Stderr, 0, 1, cfg)
}

// jobMain is the internal `topolab __job DIR` runner of a remote lifecycle job.
// It runs in its own session, ignores the hangup of the SSH session that
// started it, and stops on SIGTERM as an interrupt does in local mode.
func jobMain(dir string) int {
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE, syscall.SIGINT)
	_ = os.Setenv("PATH", agentSearchPath)
	_ = os.Chdir("/")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	return agent.RunJob(ctx, dir, agent.LocalRunner{})
}
