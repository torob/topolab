package client

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/torob/topolab/internal/remote/wire"
	"golang.org/x/term"
)

// signalName maps a signal to its protocol name, or "".
func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "INT"
	case syscall.SIGTERM:
		return "TERM"
	case syscall.SIGHUP:
		return "HUP"
	case syscall.SIGQUIT:
		return "QUIT"
	}
	return ""
}

// terminalFD returns the descriptor of f when it is a terminal.
func terminalFD(v any) (int, bool) {
	f, ok := v.(*os.File)
	if !ok || f == nil {
		return 0, false
	}
	fd := int(f.Fd())
	return fd, term.IsTerminal(fd)
}

// runExec runs exec and router exec. With a terminal on both input and output
// the controller's terminal is raw and the host child gets a pseudo-terminal;
// otherwise stdin, stdout, and stderr are separate streams. The command returns
// the child's exact exit status (spec.md Section 12.7).
func (r *Runner) runExec(ctx context.Context, inv Invocation) int {
	inFD, inTTY := terminalFD(inv.Stdin)
	outFD, outTTY := terminalFD(inv.Stdout)
	tty := inTTY && outTTY
	if tty {
		cols, rows, err := term.GetSize(outFD)
		if err != nil {
			cols, rows = 80, 24
		}
		inv.TTY = &wire.TTY{Rows: uint16(rows), Cols: uint16(cols), Term: os.Getenv("TERM")}
	}
	req, err := BuildRequest(inv)
	if err != nil {
		Print(inv.Stderr, err)
		return ExitFailure
	}
	sigs, stop := r.Notify()
	defer stop()
	s, err := r.open(ctx, &inv)
	if err != nil {
		Print(inv.Stderr, err)
		return ExitFailure
	}
	defer s.close()
	if err := s.sendJSON(wire.TypeRequest, req); err != nil {
		Print(inv.Stderr, fmt.Errorf("send request: %w", err))
		return ExitFailure
	}
	restore := func() {}
	if tty {
		old, err := term.MakeRaw(inFD)
		if err != nil {
			Print(inv.Stderr, fmt.Errorf("set the terminal to raw mode: %w", err))
			return ExitFailure
		}
		restore = func() { _ = term.Restore(inFD, old) }
		defer restore()
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for {
				select {
				case <-s.done:
					return
				case <-winch:
					if cols, rows, err := term.GetSize(outFD); err == nil {
						_ = s.sendJSON(wire.TypeResize, wire.Resize{Rows: uint16(rows), Cols: uint16(cols)})
					}
				}
			}
		}()
	}
	if inv.Stdin != nil {
		go forwardInput(s, inv, tty)
	}
	res := r.pump(ctx, s, sigs, pumpConfig{
		stdout: inv.Stdout,
		stderr: inv.Stderr,
		// Signals go to the child's process group, as local exec forwards them.
		onSignal: func(s *session, sig os.Signal, _ int) bool {
			if name := signalName(sig); name != "" {
				_ = s.sendJSON(wire.TypeSignal, wire.Signal{Name: name})
			}
			return false
		},
	})
	restore()
	return r.report(inv, res)
}

// forwardInput copies the controller's input to the child. Without a terminal
// the end of input is forwarded as well.
func forwardInput(s *session, inv Invocation, tty bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := inv.Stdin.Read(buf)
		if n > 0 {
			if s.send(wire.TypeStdin, buf[:n]) != nil {
				return
			}
		}
		if err != nil {
			if !tty {
				_ = s.send(wire.TypeStdin, nil)
			}
			return
		}
		select {
		case <-s.done:
			return
		default:
		}
	}
}
