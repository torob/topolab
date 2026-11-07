package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/remote/agent"
	"golang.org/x/sys/unix"
)

// shRunner runs /bin/sh with the descriptors and policies of an ExecHost.
type shRunner struct{ script string }

func (r shRunner) Run(ctx context.Context, _ app.Command, rio agent.RunIO) error {
	host := rio.ExecHost
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

// execLab is a lab whose runner runs shell scripts.
func execLab(t *testing.T, script string) *lab {
	t.Helper()
	l := newLab(t, nil)
	l.runner = &fakeRunner{run: nil}
	l.execRunner = shRunner{script: script}
	return l
}

func TestExecPipeModeReturnsOutputAndTheExactStatus(t *testing.T) {
	l := execLab(t, `echo out; echo err >&2; exit 5`)
	inv, stdout, stderr := l.invocation("exec", "--", "x")
	inv.Stdin = strings.NewReader("")
	if code := l.runner0(nil).Run(context.Background(), inv); code != 5 || stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecPipeModeForwardsInputAndItsEnd(t *testing.T) {
	l := execLab(t, `cat`)
	inv, stdout, stderr := l.invocation("exec", "--", "x")
	inv.Stdin = strings.NewReader("typed input\n")
	if code := l.runner0(nil).Run(context.Background(), inv); code != 0 || stdout.String() != "typed input\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestExecForwardsInterruptsToTheChildAndKeepsWaiting(t *testing.T) {
	l := execLab(t, `trap 'echo got-int; exit 3' INT; echo ready; while :; do sleep 0.05; done`)
	sigs := make(chan os.Signal, 4)
	inv, stdout, stderr := l.invocation("exec", "--", "x")
	pr, pw := io.Pipe() // input that never ends
	defer pw.Close()
	inv.Stdin = pr
	done := make(chan int, 1)
	go func() { done <- l.runner0(sigs).Run(context.Background(), inv) }()
	waitFor(t, "the child to start", func() bool { return strings.Contains(stdout.String(), "ready") })
	sigs <- syscall.SIGINT
	select {
	case code := <-done:
		if code != 3 || !strings.Contains(stdout.String(), "got-int") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no exit; stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestExecLostConnectionEndsWithStatusOne(t *testing.T) {
	l := execLab(t, `echo ready; sleep 30`)
	inv, stdout, stderr := l.invocation("exec", "--", "x")
	pr, pw := io.Pipe()
	defer pw.Close()
	inv.Stdin = pr
	done := make(chan int, 1)
	go func() { done <- l.runner0(nil).Run(context.Background(), inv) }()
	waitFor(t, "the child to start", func() bool { return strings.Contains(stdout.String(), "ready") })
	l.current.Load().Kill()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(stderr.String(), "connection to the agent was lost") {
			t.Fatalf("code=%d stderr=%q", code, stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no exit after the connection was lost")
	}
}

// terminal is a pseudo-terminal that plays the controller's terminal.
type terminal struct{ master, slave *os.File }

func openTerminal(t *testing.T, rows, cols uint16) terminal {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals: %v", err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	return terminal{master: master, slave: slave}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// readMaster accumulates what the child wrote to the controller's terminal.
type readMaster struct {
	t    *testing.T
	f    *os.File
	buf  syncBuffer
	done chan struct{}
}

func collectTerminal(t *testing.T, tm terminal) *readMaster {
	r := &readMaster{t: t, f: tm.master, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		b := make([]byte, 4096)
		for {
			n, err := tm.master.Read(b)
			if n > 0 {
				r.buf.Write(b[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

func TestExecTerminalModeUsesRawModeSizeAndRestoresTheTerminal(t *testing.T) {
	tm := openTerminal(t, 30, 100)
	before, err := unix.IoctlGetTermios(int(tm.slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	out := collectTerminal(t, tm)
	l := execLab(t, `stty size; read x; echo got:$x; read y; stty size`)
	inv, _, stderr := l.invocation("exec", "--", "x")
	inv.Stdin, inv.Stdout = tm.slave, tm.slave
	done := make(chan int, 1)
	go func() { done <- l.runner0(nil).Run(context.Background(), inv) }()
	waitFor(t, "the size report", func() bool { return strings.Contains(out.buf.String(), "30 100") })

	// While the command runs, the controller's terminal is raw.
	raw, err := unix.IoctlGetTermios(int(tm.slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Lflag&unix.ICANON != 0 || raw.Lflag&unix.ECHO != 0 {
		t.Fatalf("the controller's terminal is not raw: lflag=%#x", raw.Lflag)
	}
	if _, err := tm.master.Write([]byte("hi\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the echoed line", func() bool { return strings.Contains(out.buf.String(), "got:hi") })

	// A resize of the controller's terminal reaches the child's terminal.
	if err := unix.IoctlSetWinsize(int(tm.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 120}); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGWINCH)
	time.Sleep(150 * time.Millisecond)
	if _, err := tm.master.Write([]byte("go\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code=%d stderr=%q out=%q", code, stderr, out.buf.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no exit; out=%q stderr=%q", out.buf.String(), stderr)
	}
	waitFor(t, "the resized report", func() bool { return strings.Contains(out.buf.String(), "40 120") })
	after, err := unix.IoctlGetTermios(int(tm.slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if after.Lflag != before.Lflag || after.Iflag != before.Iflag || after.Oflag != before.Oflag {
		t.Fatalf("the terminal was not restored: before=%+v after=%+v", before, after)
	}
}

// --- capture ---

func captureLab(t *testing.T, blocks ...[]byte) (*lab, *[]string) {
	t.Helper()
	var args []string
	l := newLab(t, func(ctx context.Context, command app.Command, rio agent.RunIO) error {
		args = command.Args
		sink, err := rio.CaptureSink()
		if err != nil {
			return err
		}
		for _, b := range blocks {
			if _, err := sink.Write(b); err != nil {
				return err
			}
		}
		<-ctx.Done()
		return nil // an interrupt ends a capture normally
	})
	return l, &args
}

var (
	blockA = bytes.Repeat([]byte("A"), 40)
	blockB = bytes.Repeat([]byte("B"), 64)
)

func captureInvocation(l *lab, output string) (Invocation, *syncBuffer, *syncBuffer) {
	args := []string{"link1", "--endpoint", "a:b"}
	if output != "" {
		args = append(args, "--output", output)
	}
	return l.invocation("capture", args...)
}

func TestCaptureWritesTheStreamToAControllerFileAndInterruptEndsItNormally(t *testing.T) {
	l, gotArgs := captureLab(t, blockA, blockB)
	path := filepath.Join(t.TempDir(), "out.pcapng")
	sigs := make(chan os.Signal, 2)
	inv, stdout, stderr := captureInvocation(l, path)
	done := make(chan int, 1)
	go func() { done <- l.runner0(sigs).Run(context.Background(), inv) }()
	waitFor(t, "the data", func() bool {
		fi, err := os.Stat(path)
		return err == nil && fi.Size() == int64(len(blockA)+len(blockB))
	})
	sigs <- os.Interrupt
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code=%d stderr=%q", code, stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interrupt did not end the capture")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, append(append([]byte(nil), blockA...), blockB...)) {
		t.Fatalf("file = %q", got)
	}
	if strings.TrimSpace(stdout.String()) != path {
		t.Fatalf("stdout = %q, want the controller path", stdout)
	}
	for _, a := range *gotArgs {
		if a == "--output" || a == path {
			t.Fatalf("--output reached the agent: %v", *gotArgs)
		}
	}
	if strings.Join(*gotArgs, " ") != "link1 --endpoint a:b" {
		t.Fatalf("agent args = %v", *gotArgs)
	}
}

func TestCaptureNeverOverwritesFollowsLinksOrAcceptsBadPaths(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.pcapng")
	if err := os.WriteFile(existing, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.pcapng")
	link := filepath.Join(dir, "link.pcapng")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"existing file":     existing,
		"dangling symlink":  link,
		"wrong extension":   filepath.Join(dir, "out.txt"),
		"missing directory": filepath.Join(dir, "no", "such", "out.pcapng"),
	} {
		t.Run(name, func(t *testing.T) {
			l, _ := captureLab(t, blockA)
			inv, _, stderr := captureInvocation(l, path)
			if code := l.runner0(nil).Run(context.Background(), inv); code != 1 || stderr.String() == "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			if l.dials.Load() != 0 {
				t.Fatal("a rejected output path still started the agent")
			}
		})
	}
	if got, _ := os.ReadFile(existing); string(got) != "keep me" {
		t.Fatalf("existing file changed: %q", got)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("capture wrote through a symlink: %v", err)
	}
}

func TestCaptureDefaultPathIsUnderTheControllersWorkingDirectory(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	l, _ := captureLab(t, blockA)
	sigs := make(chan os.Signal, 2)
	inv, stdout, stderr := captureInvocation(l, "")
	done := make(chan int, 1)
	go func() { done <- l.runner0(sigs).Run(context.Background(), inv) }()
	waitFor(t, "the file", func() bool {
		m, _ := filepath.Glob(filepath.Join(work, "captures", "lists", "link1-*.pcapng"))
		if len(m) != 1 {
			return false
		}
		fi, err := os.Stat(m[0])
		return err == nil && fi.Size() == int64(len(blockA))
	})
	sigs <- os.Interrupt
	if code := <-done; code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), filepath.Join("captures", "lists", "link1-")) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestCaptureThatFailsBeforeAnyDataLeavesNoFile(t *testing.T) {
	l := newLab(t, func(context.Context, app.Command, agent.RunIO) error {
		return errors.New("link link1 has no running datapath")
	})
	path := filepath.Join(t.TempDir(), "out.pcapng")
	inv, _, stderr := captureInvocation(l, path)
	if code := l.runner0(nil).Run(context.Background(), inv); code != 1 || !strings.Contains(stderr.String(), "no running datapath") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an empty capture file remains: %v", err)
	}
}

func TestCaptureCutMidStreamKeepsWholeBlocksAndFails(t *testing.T) {
	l, _ := captureLab(t, blockA)
	path := filepath.Join(t.TempDir(), "out.pcapng")
	inv, _, stderr := captureInvocation(l, path)
	done := make(chan int, 1)
	go func() { done <- l.runner0(nil).Run(context.Background(), inv) }()
	waitFor(t, "the first block", func() bool {
		fi, err := os.Stat(path)
		return err == nil && fi.Size() == int64(len(blockA))
	})
	l.current.Load().Kill()
	if code := <-done; code != 1 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, blockA) {
		t.Fatalf("file = %q, want exactly the one whole block", got)
	}
}

func TestCaptureRejectsBadArgumentsBeforeContactingTheHost(t *testing.T) {
	l, _ := captureLab(t)
	inv, _, stderr := l.invocation("capture", "--endpoint", "a:b")
	if code := l.runner0(nil).Run(context.Background(), inv); code != ExitUsage || !strings.Contains(stderr.String(), "exactly one LINK") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if l.dials.Load() != 0 {
		t.Fatal("bad arguments reached the host")
	}
}
