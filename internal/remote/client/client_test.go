package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/agent"
	"github.com/torob/topolab/internal/remote/wire"
)

const testDigest = "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"

const testTopology = `name: lists
routers:
  - name: edge
    interfaces:
      - name: external
        external: {sources: [10.0.0.0/24]}
`

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitCode) ExitCode() int { return int(e) }

type fakeRunner struct {
	mu       sync.Mutex
	commands []app.Command
	run      func(ctx context.Context, command app.Command, rio agent.RunIO) error
}

func (f *fakeRunner) Run(ctx context.Context, command app.Command, rio agent.RunIO) error {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, command, rio)
	}
	return nil
}

// inProcDialer runs the real agent session in this process.
type inProcDialer struct {
	opts  agent.Options
	noise string
	dials int
}

func (d *inProcDialer) Dial(ctx context.Context, inv Invocation) (*Transport, error) {
	d.dials++
	clientRead, agentWrite := io.Pipe()
	agentRead, clientWrite := io.Pipe()
	agentCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	opts := d.opts
	if opts.Digest == "" {
		opts.Digest = testDigest
	}
	var out io.Writer = agentWrite
	if d.noise != "" {
		out = &noiseWriter{w: agentWrite, noise: []byte(d.noise)}
	}
	go func() {
		err := agent.Serve(agentCtx, agentRead, out, opts)
		_ = agentWrite.Close()
		done <- err
	}()
	return &Transport{
		In:   clientRead,
		Out:  clientWrite,
		Wait: func() error { <-done; return nil },
		Kill: func() { cancel(); _ = agentWrite.Close(); _ = clientWrite.Close(); _ = clientRead.Close() },
	}, nil
}

// noiseWriter puts login-script output in front of the agent's first write.
type noiseWriter struct {
	w     io.Writer
	noise []byte
}

func (n *noiseWriter) Write(p []byte) (int, error) {
	if n.noise != nil {
		noise := n.noise
		n.noise = nil
		if _, err := n.w.Write(append(append([]byte(nil), noise...), p...)); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return n.w.Write(p)
}

// scriptedDialer returns transports that a test controls completely.
type scriptedDialer struct {
	transport func() *Transport
}

func (d scriptedDialer) Dial(context.Context, Invocation) (*Transport, error) {
	return d.transport(), nil
}

func newRunner(d Dialer, sigs chan os.Signal) *Runner {
	r := &Runner{Dialer: d, Digest: testDigest, PingInterval: 20 * time.Millisecond, PeerTimeout: 2 * time.Second}
	if sigs != nil {
		r.Notify = func() (<-chan os.Signal, func()) { return sigs, func() {} }
	} else {
		r.Notify = func() (<-chan os.Signal, func()) { return make(chan os.Signal), func() {} }
	}
	return r
}

func testInvocation(t *testing.T, command string, args ...string) (Invocation, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cfg, _, err := config.Parse([]byte(testTopology), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	return Invocation{
		Destination:  "ops@lab",
		Command:      command,
		Args:         args,
		Topology:     cfg,
		TopologyData: []byte(testTopology),
		BaseDir:      t.TempDir(),
		WorkingDir:   t.TempDir(),
		Stdout:       &stdout,
		Stderr:       &stderr,
	}, &stdout, &stderr
}

func TestRunRelaysOutputAndStatus(t *testing.T) {
	runner := &fakeRunner{run: func(_ context.Context, _ app.Command, rio agent.RunIO) error {
		fmt.Fprint(rio.Stdout, "hello\n")
		fmt.Fprint(rio.Stderr, "warn\n")
		return nil
	}}
	inv, stdout, stderr := testInvocation(t, "ps", "--format", "json")
	code := newRunner(&inProcDialer{opts: agent.Options{Runner: runner}}, nil).Run(context.Background(), inv)
	if code != 0 || stdout.String() != "hello\n" || stderr.String() != "warn\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if got := runner.commands[0]; got.Name != "ps" || strings.Join(got.Args, " ") != "--format json" {
		t.Fatalf("command = %+v", got)
	}
}

func TestRunPassesChildStatusAndPlainErrors(t *testing.T) {
	for name, tt := range map[string]struct {
		err    error
		code   int
		stderr string
	}{
		"child status": {&app.ChildExitError{Code: 7}, 7, ""},
		"plain error":  {errors.New("topology is not up"), 1, "topolab: topology is not up\n"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{run: func(context.Context, app.Command, agent.RunIO) error { return tt.err }}
			inv, _, stderr := testInvocation(t, "counters")
			code := newRunner(&inProcDialer{opts: agent.Options{Runner: runner}}, nil).Run(context.Background(), inv)
			if code != tt.code || stderr.String() != tt.stderr {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
		})
	}
}

func TestRunPrintsRemoteErrorsInTheDocumentedFormat(t *testing.T) {
	inv, _, stderr := testInvocation(t, "ps")
	inv.WorkingDir = "relative"
	code := newRunner(&inProcDialer{opts: agent.Options{Runner: &fakeRunner{}}}, nil).Run(context.Background(), inv)
	if code != 1 || !strings.HasPrefix(stderr.String(), "topolab: remote: working_dir_invalid: ") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestRunCopiesLoginNoiseToStderr(t *testing.T) {
	inv, _, stderr := testInvocation(t, "ps")
	code := newRunner(&inProcDialer{noise: "Welcome to the lab host\n", opts: agent.Options{Runner: &fakeRunner{}}}, nil).Run(context.Background(), inv)
	if code != 0 || stderr.String() != "Welcome to the lab host\n" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestRunRejectsAgentWithAnotherDigest(t *testing.T) {
	inv, _, stderr := testInvocation(t, "ps")
	r := newRunner(&inProcDialer{opts: agent.Options{Digest: strings.Repeat("e", 64), Runner: &fakeRunner{}}}, nil)
	code := r.Run(context.Background(), inv)
	if code != 1 || !strings.Contains(stderr.String(), "remote: agent_mismatch:") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestStartFailureStatusesMapToErrorCodes(t *testing.T) {
	for status, want := range map[int]string{97: "agent_missing", 98: "privilege_denied", 99: "privilege_denied"} {
		t.Run(want, func(t *testing.T) {
			d := scriptedDialer{transport: func() *Transport {
				return &Transport{In: strings.NewReader(""), Out: nopWriteCloser{io.Discard}, Wait: func() error { return exitCode(status) }, Kill: func() {}}
			}}
			inv, _, stderr := testInvocation(t, "ps")
			code := newRunner(d, nil).Run(context.Background(), inv)
			if code != 1 || !strings.Contains(stderr.String(), "topolab: remote: "+want+":") {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			if want == "privilege_denied" && !strings.Contains(stderr.String(), "NOPASSWD: /usr/local/lib/topolab/"+testDigest+"/topolab agent --stdio") {
				t.Fatalf("no sudoers example: %q", stderr)
			}
		})
	}
	t.Run("ssh failure", func(t *testing.T) {
		d := scriptedDialer{transport: func() *Transport {
			return &Transport{In: strings.NewReader(""), Out: nopWriteCloser{io.Discard}, Wait: func() error { return exitCode(255) }, Kill: func() {}}
		}}
		inv, _, stderr := testInvocation(t, "ps")
		if code := newRunner(d, nil).Run(context.Background(), inv); code != 1 || !strings.Contains(stderr.String(), "could not connect") {
			t.Fatalf("code=%d stderr=%q", code, stderr)
		}
	})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestFirstInterruptCancelsAndSecondAbandons(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	runner := &fakeRunner{run: func(ctx context.Context, _ app.Command, _ agent.RunIO) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	sigs := make(chan os.Signal, 4)
	inv, _, _ := testInvocation(t, "logs", "--follow")
	done := make(chan int, 1)
	go func() {
		done <- newRunner(&inProcDialer{opts: agent.Options{Runner: runner}}, sigs).Run(context.Background(), inv)
	}()
	<-started
	sigs <- os.Interrupt
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("first interrupt did not cancel the remote command")
	}
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish after cancel")
	}

	// A command that ignores the cancel is abandoned by the second interrupt.
	stuck := make(chan struct{})
	started2 := make(chan struct{})
	runner2 := &fakeRunner{run: func(ctx context.Context, _ app.Command, _ agent.RunIO) error {
		close(started2)
		<-stuck
		return nil
	}}
	defer close(stuck)
	inv2, _, _ := testInvocation(t, "logs", "--follow")
	done2 := make(chan int, 1)
	go func() {
		done2 <- newRunner(&inProcDialer{opts: agent.Options{Runner: runner2}}, sigs).Run(context.Background(), inv2)
	}()
	<-started2
	sigs <- os.Interrupt
	sigs <- syscall.SIGTERM
	select {
	case code := <-done2:
		if code != 1 {
			t.Fatalf("code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second interrupt did not abandon the command")
	}
}

func TestSilentAgentIsDetectedByTheTimeout(t *testing.T) {
	// A scripted agent that answers the handshake and then says nothing.
	d := scriptedDialer{transport: func() *Transport {
		clientRead, agentWrite := io.Pipe()
		agentRead, clientWrite := io.Pipe()
		go func() {
			conn := wire.NewConn(agentRead, agentWrite)
			f, err := conn.Read()
			if err != nil {
				return
			}
			var hello wire.Hello
			_ = f.JSON(&hello)
			_, _ = agentWrite.Write(wire.Preamble(hello.Nonce))
			_ = conn.WriteJSON(wire.TypeHello, wire.Hello{Protocol: wire.Protocol, Digest: testDigest})
			for {
				if _, err := conn.Read(); err != nil {
					return
				}
			}
		}()
		return &Transport{In: clientRead, Out: clientWrite, Wait: func() error { return nil }, Kill: func() { _ = agentWrite.Close(); _ = clientWrite.Close(); _ = clientRead.Close() }}
	}}
	r := newRunner(d, nil)
	r.PingInterval = 10 * time.Millisecond
	r.PeerTimeout = 100 * time.Millisecond
	inv, _, stderr := testInvocation(t, "ps")
	done := make(chan int, 1)
	go func() { done <- r.Run(context.Background(), inv) }()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(stderr.String(), "stopped answering") {
			t.Fatalf("code=%d stderr=%q", code, stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client ignored the silent agent")
	}
}

func TestRemoteScriptAndSSHArgs(t *testing.T) {
	path := AgentPath("", testDigest)
	if path != "/usr/local/lib/topolab/"+testDigest+"/topolab" {
		t.Fatalf("path = %q", path)
	}
	script := RemoteScript(path)
	for _, bad := range []string{"'", `\`, "!"} {
		if strings.Contains(script, bad) {
			t.Fatalf("script contains %q: %s", bad, script)
		}
	}
	for _, want := range []string{"exit 97", "exit 98", `exec "$a" agent --stdio`, "exec sudo -n"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q: %s", want, script)
		}
	}
	args := SSHArgs([]string{"ProxyJump=bastion", "Port=2222"}, "ops@lab", path)
	want := []string{"-T", "-o", "ProxyJump=bastion", "-o", "Port=2222", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "--", "ops@lab"}
	if len(args) != len(want)+1 {
		t.Fatalf("args = %q", args)
	}
	for i, w := range want {
		if args[i] != w {
			t.Fatalf("args[%d] = %q, want %q (%q)", i, args[i], w, args)
		}
	}
	if args[len(args)-1] != "sh -c '"+script+"'" {
		t.Fatalf("remote command = %q", args[len(args)-1])
	}
	custom := AgentPath("/opt/topolab", testDigest)
	if custom != "/opt/topolab/"+testDigest+"/topolab" {
		t.Fatalf("custom path = %q", custom)
	}
}

func TestRecordingLookupRemembersSetAndUnsetNames(t *testing.T) {
	r := &RecordingLookup{Base: func(name string) (string, bool) {
		if name == "SET" {
			return "value", true
		}
		return "", false
	}}
	if v, ok := r.Lookup("SET"); !ok || v != "value" {
		t.Fatal("set variable lost")
	}
	if _, ok := r.Lookup("UNSET"); ok {
		t.Fatal("unset variable reported as set")
	}
	got := r.Recorded()
	if len(got) != 2 || !got["SET"].Set || got["SET"].Value != "value" || got["UNSET"].Set {
		t.Fatalf("recorded = %+v", got)
	}
}

func TestBuildRequestLimitsAndDigest(t *testing.T) {
	inv, _, _ := testInvocation(t, "ps")
	req, err := BuildRequest(inv)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := config.Parse([]byte(testTopology), t.TempDir(), nil)
	want, _ := config.Digest(cfg)
	if req.ConfigDigest != want || req.Topology != testTopology || req.WorkingDir != inv.WorkingDir {
		t.Fatalf("request = %+v", req)
	}
	inv.TopologyData = bytes.Repeat([]byte("#"), wire.MaxRequest)
	_, err = BuildRequest(inv)
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != wire.CodeRequestTooLarge {
		t.Fatalf("err = %v", err)
	}
}
