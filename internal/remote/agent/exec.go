package agent

import (
	"context"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/remote/wire"
)

// execCommands run a child in a topology namespace for the lifetime of the
// connection (spec.md Section 12.7).
var execCommands = map[string]bool{"exec": true, "router-exec": true}

// drainAfterExit bounds how long output is copied once the child has exited, so
// that a background descendant holding a pipe cannot block the command.
const drainAfterExit = 250 * time.Millisecond

// inputQueue hands stdin frames to a writer goroutine so that a child that does
// not read cannot stall the frame loop.
type inputQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  [][]byte
	closed bool
}

func newInputQueue(sink io.Writer, onEOF func()) *inputQueue {
	q := &inputQueue{}
	q.cond = sync.NewCond(&q.mu)
	go func() {
		for {
			q.mu.Lock()
			for len(q.items) == 0 && !q.closed {
				q.cond.Wait()
			}
			if len(q.items) == 0 && q.closed {
				q.mu.Unlock()
				onEOF()
				return
			}
			item := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			if _, err := sink.Write(item); err != nil {
				// The child is gone or stopped reading; drop what follows.
				q.mu.Lock()
				q.items, q.closed = nil, true
				q.mu.Unlock()
				onEOF()
				return
			}
		}
	}()
	return q
}

// push queues data; an empty slice means end of input.
func (q *inputQueue) push(data []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if len(data) == 0 {
		q.closed = true
	} else {
		q.items = append(q.items, append([]byte(nil), data...))
	}
	q.cond.Signal()
}

func (q *inputQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.items = nil
	q.mu.Unlock()
	q.cond.Broadcast()
}

// execIO is the descriptors and relays of one exec command.
type execIO struct {
	host    *app.ExecHost
	onFrame func(wire.Frame)
	// afterRun runs once the command returned; it closes the agent's copies of
	// the child's descriptors and waits for the output to drain.
	afterRun func()
	// abort releases everything without waiting.
	abort func()
}

// execEnv is the only controller value that reaches the child: TERM, when the
// controller has a terminal.
func execEnv(tty *wire.TTY) []string {
	if tty == nil || tty.Term == "" || len(tty.Term) > 64 {
		return nil
	}
	for _, r := range tty.Term {
		if !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return nil
		}
	}
	return []string{"TERM=" + tty.Term}
}

// signalFromName maps a forwarded signal name.
func signalFromName(name string) (syscall.Signal, bool) {
	switch name {
	case "INT":
		return syscall.SIGINT, true
	case "TERM":
		return syscall.SIGTERM, true
	case "HUP":
		return syscall.SIGHUP, true
	case "QUIT":
		return syscall.SIGQUIT, true
	}
	return 0, false
}

// newExecIO builds the descriptors for a terminal or pipe session.
func (s *session) newExecIO(tty *wire.TTY, signals chan os.Signal, stdout, stderr io.Writer, killAfter time.Duration) (*execIO, error) {
	if tty != nil {
		return s.newPTYIO(tty, signals, stdout, killAfter)
	}
	return s.newPipeIO(signals, stdout, stderr, killAfter)
}

func (s *session) newPTYIO(tty *wire.TTY, signals chan os.Signal, stdout io.Writer, killAfter time.Duration) (*execIO, error) {
	master, slave, err := openPTY(tty.Rows, tty.Cols)
	if err != nil {
		return nil, err
	}
	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		_, _ = io.Copy(stdout, master) // ends with EIO once the slave is closed
	}()
	input := newInputQueue(master, func() {})
	x := &execIO{
		host: &app.ExecHost{
			Stdin: slave, Stdout: slave, Stderr: slave, PTY: true, Signals: signals,
			Env: execEnv(tty), CancelSignal: syscall.SIGHUP, KillAfter: killAfter,
		},
		onFrame: func(f wire.Frame) {
			switch f.Type {
			case wire.TypeStdin:
				input.push(f.Payload)
			case wire.TypeResize:
				var r wire.Resize
				if f.JSON(&r) == nil {
					_ = resizePTY(master, r.Rows, r.Cols)
				}
			}
		},
	}
	x.afterRun = func() {
		input.close()
		_ = slave.Close()
		select {
		case <-relayed:
		case <-time.After(2 * time.Second):
		}
		_ = master.Close()
	}
	x.abort = func() { input.close(); _ = slave.Close(); _ = master.Close() }
	return x, nil
}

func (s *session) newPipeIO(signals chan os.Signal, stdout, stderr io.Writer, killAfter time.Duration) (*execIO, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
		return nil, err
	}
	var relays sync.WaitGroup
	relay := func(r *os.File, w io.Writer) {
		relays.Add(1)
		go func() {
			defer relays.Done()
			_, _ = io.Copy(w, r)
		}()
	}
	relay(outR, stdout)
	relay(errR, stderr)
	input := newInputQueue(inW, func() { _ = inW.Close() })
	x := &execIO{
		host: &app.ExecHost{
			Stdin: inR, Stdout: outW, Stderr: errW, Signals: signals,
			Env: execEnv(nil), CancelSignal: syscall.SIGHUP, KillAfter: killAfter,
		},
		onFrame: func(f wire.Frame) {
			if f.Type == wire.TypeStdin {
				input.push(f.Payload)
			}
		},
	}
	x.afterRun = func() {
		input.close()
		_ = inR.Close()
		_ = outW.Close()
		_ = errW.Close()
		// A descendant that outlives the child may still hold the pipes.
		deadline := time.Now().Add(drainAfterExit)
		_ = outR.SetReadDeadline(deadline)
		_ = errR.SetReadDeadline(deadline)
		relays.Wait()
		_ = outR.Close()
		_ = errR.Close()
		_ = inW.Close()
	}
	x.abort = func() {
		input.close()
		for _, f := range []*os.File{inR, inW, outR, outW, errR, errW} {
			_ = f.Close()
		}
	}
	return x, nil
}

// runExec runs exec and router exec. The child lives and dies with the
// connection: on a lost connection it receives SIGHUP, then SIGKILL.
func (s *session) runExec(req wire.Request) (int, error) {
	p, err := parseRequest(req)
	if err != nil {
		return s.reject(err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.setCancel(cancel)
	stdout := streamWriter{conn: s.conn, typ: wire.TypeStdout}
	stderr := streamWriter{conn: s.conn, typ: wire.TypeStderr}
	signals := make(chan os.Signal, 16)
	x, err := s.newExecIO(req.TTY, signals, stdout, stderr, s.opts.ExecKillAfter)
	if err != nil {
		return s.reject(err)
	}
	s.setHandler(func(f wire.Frame) {
		switch f.Type {
		case wire.TypeSignal:
			var sig wire.Signal
			if f.JSON(&sig) != nil {
				return
			}
			if value, ok := signalFromName(sig.Name); ok {
				select {
				case signals <- value:
				default:
				}
			}
		default:
			x.onFrame(f)
		}
	})
	defer s.setHandler(nil)

	name := req.Command
	command := app.Command{Name: name, Args: req.Args, Topology: p.cfg, WorkingDir: p.workingDir, Helpers: helperOverrides(req.Helpers)}
	runErr := s.opts.Runner.Run(ctx, command, RunIO{Stdout: stdout, Stderr: stderr, ExecHost: x.host})
	x.afterRun()
	return s.finish(runErr, stderr)
}
