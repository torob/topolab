// Package agent serves one remote command over a framed stream. It runs as
// root on the host, started by the controller through SSH (spec.md Section 12).
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/remote/jobs"
	"github.com/torob/topolab/internal/remote/wire"
	"golang.org/x/sys/unix"
)

// Runner runs one command on the host with the semantics of spec.md Sections 3
// through 10.
type Runner interface {
	Run(ctx context.Context, command app.Command, rio RunIO) error
}

// RunIO carries the streams that a command uses.
type RunIO struct {
	Stdout, Stderr io.Writer
	// ExecHost drives exec and router exec; it is nil for other commands.
	ExecHost *app.ExecHost
	// CaptureSink receives the pcapng stream of capture.
	CaptureSink func() (io.WriteCloser, error)
	// TemplateDigests maps a template path to the digest it must have.
	TemplateDigests map[string]string
	// LockNotice runs once when a lifecycle command waits for the topology lock.
	LockNotice func()
}

// Options configure a session.
type Options struct {
	// Digest is the SHA-256 of the running executable.
	Digest  string
	Version string
	Runner  Runner
	// PingInterval is the silence after which each side sends a ping.
	// PeerTimeout is the silence after which the peer is considered gone.
	PingInterval time.Duration
	PeerTimeout  time.Duration
	Now          func() time.Time
	// SpawnJob starts the runner of a job directory. The default starts the
	// agent executable with the __job subcommand in its own session.
	SpawnJob func(dir string) error
	// JobProcs reads process facts for job liveness; nil means /proc.
	JobProcs jobs.Procs
	// JobStartTimeout bounds the wait for a runner to record `running`.
	JobStartTimeout time.Duration
	// PollInterval is how often a job's output log is checked.
	PollInterval time.Duration
	// SignalRunner asks a runner to stop; the default sends SIGTERM.
	SignalRunner func(pid int)
	// ExecKillAfter is how long an exec child may ignore SIGHUP before it
	// receives SIGKILL; the default is 5 seconds.
	ExecKillAfter time.Duration
}

func (o *Options) defaults() {
	if o.PingInterval <= 0 {
		o.PingInterval = 15 * time.Second
	}
	if o.PeerTimeout <= 0 {
		o.PeerTimeout = 60 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.JobStartTimeout <= 0 {
		o.JobStartTimeout = 10 * time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 50 * time.Millisecond
	}
	if o.SpawnJob == nil {
		o.SpawnJob = DefaultSpawn()
	}
	if o.ExecKillAfter <= 0 {
		o.ExecKillAfter = 5 * time.Second
	}
	if o.SignalRunner == nil {
		o.SignalRunner = func(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }
	}
}

// errPeerTimeout is the cancellation cause when the controller goes silent.
var errPeerTimeout = errors.New("controller sent no frame within the peer timeout")

type session struct {
	opts   Options
	conn   *wire.Conn
	ctx    context.Context
	cancel context.CancelCauseFunc

	lastRecv atomic.Int64
	lastSend atomic.Int64

	requests chan wire.Request

	mu        sync.Mutex
	handler   func(wire.Frame)
	cmdCancel context.CancelFunc
	// cancelRequested is sticky: a cancel frame that arrives before the command
	// registers its cancel function is not lost.
	cancelRequested bool
	// pending holds data frames that arrived before a handler was registered,
	// for example input that the controller sends right after its request.
	pending      []wire.Frame
	pendingBytes int

	// jobID is the job that the exit frame reports.
	jobID string
}

// touchWriter records the time of the last write.
type touchWriter struct {
	w    io.Writer
	last *atomic.Int64
	now  func() time.Time
}

func (t *touchWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	t.last.Store(t.now().UnixNano())
	return n, err
}

// Serve runs one session. It returns nil when the command finished and the
// exit frame was sent, and an error when the stream failed first.
func Serve(ctx context.Context, in io.Reader, out io.Writer, opts Options) error {
	opts.defaults()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	s := &session{opts: opts, ctx: ctx, cancel: cancel, requests: make(chan wire.Request, 1)}
	tw := &touchWriter{w: out, last: &s.lastSend, now: opts.Now}
	s.conn = wire.NewConn(in, tw)
	if closer, ok := in.(io.Closer); ok {
		defer closer.Close()
	}
	if closer, ok := out.(io.Closer); ok {
		// When the session ends abnormally, close the output so that a writer
		// blocked on a stalled peer fails instead of waiting for ever.
		go func() {
			<-ctx.Done()
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				_ = closer.Close()
			}
		}()
	}

	first, err := s.conn.Read()
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	var hello wire.Hello
	if first.Type != wire.TypeHello {
		return fmt.Errorf("%w: first frame is %s, want hello", wire.ErrProtocol, first.Type)
	}
	if err := first.JSON(&hello); err != nil {
		return err
	}
	if hello.Protocol != wire.Protocol || len(hello.Nonce) != 32 {
		s.fail(wire.CodeProtocolError, fmt.Sprintf("unsupported hello: protocol %d", hello.Protocol))
		return fmt.Errorf("%w: unsupported hello", wire.ErrProtocol)
	}
	if _, err := tw.Write(wire.Preamble(hello.Nonce)); err != nil {
		return err
	}
	reply := wire.Hello{Protocol: wire.Protocol, Digest: opts.Digest, Arch: runtime.GOARCH, Kernel: kernelRelease(), Version: opts.Version}
	if err := s.conn.WriteJSON(wire.TypeHello, reply); err != nil {
		return err
	}
	now := opts.Now().UnixNano()
	s.lastRecv.Store(now)
	s.lastSend.Store(now)

	go s.readLoop()
	go s.heartbeat()

	var req wire.Request
	select {
	case req = <-s.requests:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	code, err := s.handle(req)
	if err != nil {
		return err
	}
	return s.conn.WriteJSON(wire.TypeExit, wire.Exit{Code: code, Job: s.jobID})
}

func kernelRelease() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return unix.ByteSliceToString(u.Release[:])
}

func (s *session) readLoop() {
	for {
		f, err := s.conn.Read()
		if err != nil {
			s.cancel(fmt.Errorf("connection lost: %w", err))
			return
		}
		s.lastRecv.Store(s.opts.Now().UnixNano())
		switch f.Type {
		case wire.TypePing:
			_ = s.conn.Write(wire.TypePong, nil)
		case wire.TypePong:
		case wire.TypeRequest:
			var req wire.Request
			if err := f.JSON(&req); err != nil {
				s.cancel(err)
				return
			}
			select {
			case s.requests <- req:
			default:
				s.cancel(fmt.Errorf("%w: second request", wire.ErrProtocol))
				return
			}
		case wire.TypeCancel:
			s.mu.Lock()
			s.cancelRequested = true
			cancel := s.cmdCancel
			s.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		default:
			s.deliver(f)
		}
	}
}

// heartbeat sends pings after silence and ends the session when the peer goes
// silent. The timeout check never waits on a write: a stalled peer can block
// writes, and the check must still fire. Pings therefore go out through a
// separate goroutine.
func (s *session) heartbeat() {
	step := s.opts.PingInterval / 2
	if step <= 0 {
		step = time.Millisecond
	}
	pings := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-pings:
				_ = s.conn.Write(wire.TypePing, nil)
			}
		}
	}()
	ticker := time.NewTicker(step)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		now := s.opts.Now().UnixNano()
		if time.Duration(now-s.lastRecv.Load()) >= s.opts.PeerTimeout {
			s.cancel(errPeerTimeout)
			return
		}
		if time.Duration(now-s.lastSend.Load()) >= s.opts.PingInterval {
			select {
			case pings <- struct{}{}:
			default:
			}
		}
	}
}

// maxPending bounds the frames held for a handler that is not yet registered.
const (
	maxPendingFrames = 1024
	maxPendingBytes  = 8 << 20
)

// deliver hands a frame to the handler, or holds it until one is registered.
// The handler runs under the lock so that frames keep their order; handlers
// never block.
func (s *session) deliver(f wire.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handler != nil {
		s.handler(f)
		return
	}
	s.pendingBytes += len(f.Payload)
	if len(s.pending) >= maxPendingFrames || s.pendingBytes > maxPendingBytes {
		s.cancel(fmt.Errorf("%w: too much input before the command was ready", wire.ErrProtocol))
		return
	}
	s.pending = append(s.pending, f)
}

// setHandler routes frames that the read loop does not handle itself. Frames
// that arrived earlier are delivered first, in order.
func (s *session) setHandler(h func(wire.Frame)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = h
	if h == nil {
		return
	}
	pending := s.pending
	s.pending, s.pendingBytes = nil, 0
	for _, f := range pending {
		h(f)
	}
}

// setCancel registers the function that a cancel frame calls. When a cancel
// frame already arrived, the function runs at once.
func (s *session) setCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	s.cmdCancel = cancel
	requested := s.cancelRequested
	s.mu.Unlock()
	if requested && cancel != nil {
		cancel()
	}
}

// fail reports an error and ends the command with status 1.
func (s *session) fail(code, message string) {
	_ = s.conn.WriteJSON(wire.TypeError, wire.ErrorMsg{Code: code, Message: message})
	_ = s.conn.WriteJSON(wire.TypeExit, wire.Exit{Code: 1})
}

// streamWriter turns writes into frames of at most 64 KiB.
type streamWriter struct {
	conn *wire.Conn
	typ  wire.Type
}

func (w streamWriter) Write(p []byte) (int, error) {
	const chunk = 64 << 10
	written := 0
	for len(p) > 0 {
		part := p
		if len(part) > chunk {
			part = part[:chunk]
		}
		if err := w.conn.Write(w.typ, part); err != nil {
			return written, err
		}
		written += len(part)
		p = p[len(part):]
	}
	return written, nil
}
