package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/remote/wire"
)

// shRunner runs /bin/sh with the descriptors and policies of an ExecHost, the
// way app.App runs an exec child, so the agent's plumbing runs unprivileged.
type shRunner struct{ script string }

func (r shRunner) Run(ctx context.Context, _ app.Command, rio RunIO) error {
	host := rio.ExecHost
	if host == nil {
		return errors.New("no exec host")
	}
	cmd := exec.Command("/bin/sh", "-c", r.script)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = host.Stdin, host.Stdout, host.Stderr
	if host.PTY {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	cmd.Env = append(os.Environ(), host.Env...)
	if err := cmd.Start(); err != nil {
		return err
	}
	cancelSignal := syscall.SIGTERM
	if host.CancelSignal != 0 {
		cancelSignal = host.CancelSignal
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var kill <-chan time.Time
	ctxDone := ctx.Done()
	for {
		select {
		case sig := <-host.Signals:
			if v, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(-cmd.Process.Pid, v)
			}
		case <-ctxDone:
			_ = syscall.Kill(-cmd.Process.Pid, cancelSignal)
			ctxDone = nil
			if host.KillAfter > 0 {
				kill = time.After(host.KillAfter)
			}
		case <-kill:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			kill = nil
		case err := <-done:
			if err == nil {
				return nil
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				if st, ok := exitErr.Sys().(syscall.WaitStatus); ok && st.Signaled() {
					return &app.ChildExitError{Code: 128 + int(st.Signal())}
				}
				return &app.ChildExitError{Code: exitErr.ExitCode()}
			}
			return err
		}
	}
}

func startExec(t *testing.T, script string, tty *wire.TTY) *peer {
	t.Helper()
	p := startAgent(t, Options{Runner: shRunner{script: script}, ExecKillAfter: 200 * time.Millisecond})
	p.handshake("")
	req := baseRequest(t, "exec", "--", "ignored")
	req.TTY = tty
	if err := p.conn.WriteJSON(wire.TypeRequest, req); err != nil {
		t.Fatal(err)
	}
	return p
}

// readUntil collects output until it contains want, or the exit frame arrives.
func (p *peer) readUntil(want string) (stdout, stderr string, exit *wire.Exit) {
	p.t.Helper()
	var out, errb strings.Builder
	deadline := time.Now().Add(10 * time.Second)
	for {
		if want != "" && strings.Contains(out.String(), want) {
			return out.String(), errb.String(), nil
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("timed out waiting for %q; stdout=%q stderr=%q", want, out.String(), errb.String())
		}
		f, err := p.conn.Read()
		if err != nil {
			p.t.Fatalf("read: %v (stdout=%q)", err, out.String())
		}
		switch f.Type {
		case wire.TypeStdout:
			out.Write(f.Payload)
		case wire.TypeStderr:
			errb.Write(f.Payload)
		case wire.TypeExit:
			var e wire.Exit
			_ = f.JSON(&e)
			return out.String(), errb.String(), &e
		case wire.TypePing:
			_ = p.conn.Write(wire.TypePong, nil)
		}
	}
}

func (p *peer) finishExec() (string, string, wire.Exit) {
	p.t.Helper()
	out, errs, exit := p.readUntil("")
	if exit == nil {
		p.t.Fatal("no exit frame")
	}
	return out, errs, *exit
}

func TestExecPipeModeSeparatesStreamsAndReturnsTheChildStatus(t *testing.T) {
	p := startExec(t, `echo out; echo err >&2; exit 7`, nil)
	stdout, stderr, exit := p.finishExec()
	if stdout != "out\n" || stderr != "err\n" || exit.Code != 7 {
		t.Fatalf("stdout=%q stderr=%q exit=%+v", stdout, stderr, exit)
	}
}

func TestExecPipeModeForwardsStdinAndItsEnd(t *testing.T) {
	p := startExec(t, `cat`, nil)
	if err := p.conn.Write(wire.TypeStdin, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := p.conn.Write(wire.TypeStdin, nil); err != nil { // end of input
		t.Fatal(err)
	}
	stdout, _, exit := p.finishExec()
	if stdout != "hello\n" || exit.Code != 0 {
		t.Fatalf("stdout=%q exit=%+v", stdout, exit)
	}
}

func TestExecDoesNotWaitForBackgroundDescendantsHoldingThePipes(t *testing.T) {
	p := startExec(t, `sleep 2 & echo started`, nil)
	start := time.Now()
	stdout, _, exit := p.finishExec()
	if stdout != "started\n" || exit.Code != 0 {
		t.Fatalf("stdout=%q exit=%+v", stdout, exit)
	}
	if took := time.Since(start); took > 1500*time.Millisecond {
		t.Fatalf("the command waited %v for a background descendant", took)
	}
}

func TestExecForwardsSignalFramesToTheChildGroup(t *testing.T) {
	p := startExec(t, `trap 'echo got-int; exit 3' INT; echo ready; while :; do sleep 0.05; done`, nil)
	if _, _, exit := p.readUntil("ready"); exit != nil {
		t.Fatal("exited early")
	}
	if err := p.conn.WriteJSON(wire.TypeSignal, wire.Signal{Name: "INT"}); err != nil {
		t.Fatal(err)
	}
	stdout, _, exit := p.finishExec()
	if !strings.Contains(stdout, "got-int") || exit.Code != 3 {
		t.Fatalf("stdout=%q exit=%+v", stdout, exit)
	}
	// Unknown signal names are ignored, never delivered.
	q := startExec(t, `echo ready; sleep 0.3; echo alive`, nil)
	q.readUntil("ready")
	_ = q.conn.WriteJSON(wire.TypeSignal, wire.Signal{Name: "KILL"})
	stdout, _, exit = q.finishExec()
	if !strings.Contains(stdout, "alive") || exit.Code != 0 {
		t.Fatalf("a KILL frame reached the child: %q %+v", stdout, exit)
	}
}

func TestExecCancelFrameHangsUpTheChild(t *testing.T) {
	p := startExec(t, `echo ready; sleep 30`, nil)
	p.readUntil("ready")
	if err := p.conn.Write(wire.TypeCancel, nil); err != nil {
		t.Fatal(err)
	}
	_, _, exit := p.finishExec()
	if exit.Code != 128+int(syscall.SIGHUP) {
		t.Fatalf("exit = %+v, want 128+SIGHUP", exit)
	}
}

func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	// A zombie still answers kill(0); read its state.
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	text := string(raw)
	return strings.HasPrefix(strings.TrimSpace(text[strings.LastIndexByte(text, ')')+1:]), "Z")
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d is still alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLostConnectionHangsUpTheChildAndKillsOneThatIgnoresIt(t *testing.T) {
	for name, script := range map[string]string{
		"dies on hangup": `echo $$ > %s; exec sleep 30`,
		"ignores hangup": `trap '' HUP; echo $$ > %s; while :; do sleep 0.05; done`,
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			p := startExec(t, fmt.Sprintf(script, pidFile), nil)
			pid := readPid(t, pidFile)
			closeInput(p)
			if err := <-p.done; err == nil {
				t.Fatal("Serve returned nil after a lost connection")
			}
			waitGone(t, pid)
		})
	}
}

func TestExecTerminalModeGivesAPseudoTerminalWithSizeAndTerm(t *testing.T) {
	p := startExec(t, `stty size; echo TERM=$TERM; read x; echo got:$x; stty size`, &wire.TTY{Rows: 24, Cols: 80, Term: "xterm-256color"})
	out, _, exit := p.readUntil("TERM=xterm-256color")
	if exit != nil || !strings.Contains(out, "24 80") {
		t.Fatalf("out=%q exit=%v", out, exit)
	}
	if err := p.conn.WriteJSON(wire.TypeResize, wire.Resize{Rows: 50, Cols: 132}); err != nil {
		t.Fatal(err)
	}
	// Let the resize reach the terminal before the script reads its next line.
	time.Sleep(100 * time.Millisecond)
	if err := p.conn.Write(wire.TypeStdin, []byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	stdout, _, exitFrame := p.finishExec()
	if !strings.Contains(stdout, "got:hello") || !strings.Contains(stdout, "50 132") || exitFrame.Code != 0 {
		t.Fatalf("stdout=%q exit=%+v", stdout, exitFrame)
	}
}

func TestExecTerminalModeDeliversTheInterruptCharacterToTheChild(t *testing.T) {
	p := startExec(t, `trap 'echo got-int; exit 4' INT; echo ready; while :; do sleep 0.05; done`, &wire.TTY{Rows: 24, Cols: 80})
	if _, _, exit := p.readUntil("ready"); exit != nil {
		t.Fatal("exited early")
	}
	if err := p.conn.Write(wire.TypeStdin, []byte{0x03}); err != nil {
		t.Fatal(err)
	}
	stdout, _, exit := p.finishExec()
	if !strings.Contains(stdout, "got-int") || exit.Code != 4 {
		t.Fatalf("stdout=%q exit=%+v", stdout, exit)
	}
}

func TestExecEnvForwardsOnlyASafeTerm(t *testing.T) {
	for _, tt := range []struct {
		tty  *wire.TTY
		want []string
	}{
		{nil, nil},
		{&wire.TTY{Term: ""}, nil},
		{&wire.TTY{Term: "xterm-256color"}, []string{"TERM=xterm-256color"}},
		{&wire.TTY{Term: "xterm;rm -rf /"}, nil},
		{&wire.TTY{Term: strings.Repeat("a", 65)}, nil},
		{&wire.TTY{Term: "a b"}, nil},
	} {
		got := execEnv(tt.tty)
		if strings.Join(got, ",") != strings.Join(tt.want, ",") {
			t.Errorf("execEnv(%+v) = %v, want %v", tt.tty, got, tt.want)
		}
	}
}

func TestInputQueueNeverBlocksTheFrameLoop(t *testing.T) {
	block := make(chan struct{})
	written := make(chan string, 8)
	q := newInputQueue(writerFunc(func(p []byte) (int, error) {
		<-block
		written <- string(p)
		return len(p), nil
	}), func() { written <- "EOF" })
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			q.push([]byte("x"))
		}
		q.push(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("push blocked while the sink was stuck")
	}
	close(block)
	for i := 0; i < 1000; i++ {
		if got := <-written; got != "x" {
			t.Fatalf("item %d = %q", i, got)
		}
	}
	if got := <-written; got != "EOF" {
		t.Fatalf("last = %q", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
