// Package client is the controller side of remote operation: it starts an
// agent through SSH, sends one request, and relays the result.
package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/wire"
)

// Exit statuses (spec.md Section 12.8).
const (
	ExitOK       = 0
	ExitFailure  = 1
	ExitUsage    = 2
	ExitDetached = 3
)

// Invocation is one controller command.
type Invocation struct {
	Destination      string
	SSHOptions       []string
	ReconnectTimeout time.Duration

	// JobID is set by the runner for lifecycle commands.
	JobID   string
	Command string
	Args    []string

	Topology     *config.Config
	TopologyData []byte
	BaseDir      string
	Env          map[string]wire.EnvValue
	WorkingDir   string
	Helpers      wire.Helpers

	// Templates are filled by the runner for `up`.
	Templates []wire.TemplateRef
	// TTY describes the controller's terminal for exec.
	TTY *wire.TTY

	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Transport is one running agent process.
type Transport struct {
	// In is the agent's standard output; Out is its standard input.
	In  io.Reader
	Out io.WriteCloser
	// Wait blocks until the remote command ends and returns its status as an
	// error with an ExitCode method, or nil.
	Wait func() error
	// Kill ends the remote command.
	Kill func()
}

// Dialer starts an agent.
type Dialer interface {
	Dial(ctx context.Context, inv Invocation) (*Transport, error)
}

// Runner runs invocations.
type Runner struct {
	Dialer Dialer
	// Digest is the SHA-256 of the controller's own executable.
	Digest string
	// Notify returns the interrupt channel and a function that stops it. The
	// default registers SIGINT, SIGTERM, and SIGHUP.
	Notify       func() (<-chan os.Signal, func())
	PingInterval time.Duration
	PeerTimeout  time.Duration
	// BackoffInitial and BackoffMax shape the delay between reconnection
	// attempts: it starts at BackoffInitial, doubles, and stops at BackoffMax.
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	// InstallRoot is where agents are installed on the host.
	InstallRoot string
	// DigestCache optionally remembers template digests.
	DigestCache *DigestCache
}

func (r *Runner) defaults() {
	if r.PingInterval <= 0 {
		r.PingInterval = 15 * time.Second
	}
	if r.PeerTimeout <= 0 {
		r.PeerTimeout = 60 * time.Second
	}
	if r.Notify == nil {
		r.Notify = func() (<-chan os.Signal, func()) {
			ch := make(chan os.Signal, 8)
			signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
			return ch, func() { signal.Stop(ch) }
		}
	}
	if r.BackoffInitial <= 0 {
		r.BackoffInitial = time.Second
	}
	if r.BackoffMax <= 0 {
		r.BackoffMax = 15 * time.Second
	}
}

type frameMsg struct {
	f   wire.Frame
	err error
}

// session is one connected agent.
type session struct {
	conn   *wire.Conn
	tr     *Transport
	frames chan frameMsg
	done   chan struct{}

	lastRecv atomic.Int64
	lastSend atomic.Int64
	timedOut atomic.Bool

	closeOnce sync.Once
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.tr.Kill()
		_ = s.tr.Out.Close()
	})
}

func (s *session) readFrames() {
	for {
		f, err := s.conn.Read()
		select {
		case s.frames <- frameMsg{f: f, err: err}:
		case <-s.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// open starts an agent and completes the handshake.
func (r *Runner) open(ctx context.Context, inv *Invocation) (*session, error) {
	tr, err := r.Dialer.Dial(ctx, *inv)
	if err != nil {
		return nil, err
	}
	nonce, err := wire.NewNonce()
	if err != nil {
		tr.Kill()
		return nil, err
	}
	br := bufio.NewReaderSize(tr.In, 64<<10)
	conn := wire.NewConn(br, tr.Out)
	fail := func(cause error) (*session, error) {
		return nil, r.explainStartFailure(tr, cause)
	}
	if err := conn.WriteJSON(wire.TypeHello, wire.Hello{Protocol: wire.Protocol, Digest: r.Digest, Nonce: nonce}); err != nil {
		return fail(err)
	}
	noise, err := wire.ScanPreamble(br, nonce, wire.MaxPreamble)
	if err != nil {
		return fail(err)
	}
	if len(noise) > 0 && inv.Stderr != nil {
		_, _ = inv.Stderr.Write(noise)
	}
	f, err := conn.Read()
	if err != nil {
		return fail(err)
	}
	var hello wire.Hello
	if f.Type != wire.TypeHello {
		return fail(fmt.Errorf("%w: agent sent %s instead of hello", wire.ErrProtocol, f.Type))
	}
	if err := f.JSON(&hello); err != nil {
		return fail(err)
	}
	if hello.Protocol != wire.Protocol {
		return fail(&RemoteError{Code: wire.CodeAgentMismatch, Message: fmt.Sprintf("agent speaks protocol %d, controller speaks %d", hello.Protocol, wire.Protocol)})
	}
	if hello.Digest != r.Digest || (hello.Arch != "" && hello.Arch != runtime.GOARCH) {
		return fail(&RemoteError{Code: wire.CodeAgentMismatch, Message: fmt.Sprintf("agent digest %q (%s) differs from the controller's %q (%s)", hello.Digest, hello.Arch, r.Digest, runtime.GOARCH)})
	}
	s := &session{conn: conn, tr: tr, frames: make(chan frameMsg), done: make(chan struct{})}
	now := time.Now().UnixNano()
	s.lastRecv.Store(now)
	s.lastSend.Store(now)
	go s.readFrames()
	go s.monitor(r.PingInterval, r.PeerTimeout)
	return s, nil
}

// monitor sends pings after silence and ends the session when the agent goes
// silent. Pings go out through a separate goroutine, because a stalled agent
// can block writes while the timeout check must still fire.
func (s *session) monitor(ping, timeout time.Duration) {
	step := ping / 2
	if step <= 0 {
		step = time.Millisecond
	}
	pings := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-s.done:
				return
			case <-pings:
				_ = s.send(wire.TypePing, nil)
			}
		}
	}()
	ticker := time.NewTicker(step)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
		now := time.Now().UnixNano()
		if time.Duration(now-s.lastRecv.Load()) >= timeout {
			s.timedOut.Store(true)
			s.close()
			return
		}
		if time.Duration(now-s.lastSend.Load()) >= ping {
			select {
			case pings <- struct{}{}:
			default:
			}
		}
	}
}

func (s *session) send(t wire.Type, payload []byte) error {
	err := s.conn.Write(t, payload)
	s.lastSend.Store(time.Now().UnixNano())
	return err
}

func (s *session) sendJSON(t wire.Type, v any) error {
	err := s.conn.WriteJSON(t, v)
	s.lastSend.Store(time.Now().UnixNano())
	return err
}

// explainStartFailure turns a failed start into the most useful error. The
// wrapper script reports a missing agent and a denied privilege through its
// exit status.
func (r *Runner) explainStartFailure(tr *Transport, cause error) error {
	waited := make(chan error, 1)
	go func() { waited <- tr.Wait() }()
	var exitErr error
	select {
	case exitErr = <-waited:
	case <-time.After(2 * time.Second):
		tr.Kill()
		exitErr = <-waited
	}
	_ = tr.Out.Close()
	var coder interface{ ExitCode() int }
	if errors.As(exitErr, &coder) {
		switch coder.ExitCode() {
		case 97:
			return &RemoteError{Code: wire.CodeAgentMissing, Message: "the agent is not installed on the host; run `topolab agent install` with --remote or TOPOLAB_REMOTE"}
		case 98, 99:
			return &RemoteError{Code: wire.CodePrivilegeDenied, Message: r.privilegeHelp()}
		case 255:
			return fmt.Errorf("ssh could not connect or authenticate (exit status 255)")
		}
	}
	return cause
}

func (r *Runner) privilegeHelp() string {
	root := r.InstallRoot
	if root == "" {
		root = DefaultInstallRoot
	}
	return fmt.Sprintf("the account cannot run the agent as root without a password; use a root account or allow it in sudoers (this rule gives the account root-equivalent control): ACCOUNT ALL=(root) NOPASSWD: %s/%s/topolab agent --stdio", root, r.Digest)
}

// Print reports a failure the way the controller always does.
func Print(w io.Writer, err error) {
	if w == nil || err == nil {
		return
	}
	fmt.Fprintf(w, "topolab: %v\n", err)
}
