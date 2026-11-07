package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/wire"
)

const testTopology = `name: lists
routers:
  - name: edge
    interfaces:
      - name: external
        external: {sources: [10.0.0.0/24]}
`

type fakeRunner struct {
	mu       sync.Mutex
	commands []app.Command
	run      func(ctx context.Context, command app.Command, rio RunIO) error
}

func (f *fakeRunner) Run(ctx context.Context, command app.Command, rio RunIO) error {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(ctx, command, rio)
	}
	return nil
}

// peer is the controller side of an in-process session.
type peer struct {
	t           *testing.T
	conn        *wire.Conn
	br          *bufio.Reader
	clientWrite *io.PipeWriter
	done        chan error
	hello       wire.Hello
	noise       []byte
}

func startAgent(t *testing.T, opts Options) *peer {
	t.Helper()
	if opts.Digest == "" {
		opts.Digest = strings.Repeat("a", 64)
	}
	if opts.Runner == nil {
		opts.Runner = &fakeRunner{}
	}
	clientRead, agentWrite := io.Pipe()
	agentRead, clientWrite := io.Pipe()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		err := Serve(ctx, agentRead, agentWrite, opts)
		_ = agentWrite.Close()
		done <- err
	}()
	br := bufio.NewReader(clientRead)
	p := &peer{t: t, br: br, clientWrite: clientWrite, conn: wire.NewConn(br, clientWrite), done: done}
	t.Cleanup(func() { _ = clientWrite.Close(); _ = clientRead.Close() })
	return p
}

func (p *peer) handshake(noise string) {
	p.t.Helper()
	nonce, err := wire.NewNonce()
	if err != nil {
		p.t.Fatal(err)
	}
	if err := p.conn.WriteJSON(wire.TypeHello, wire.Hello{Protocol: wire.Protocol, Digest: strings.Repeat("a", 64), Nonce: nonce}); err != nil {
		p.t.Fatal(err)
	}
	p.noise, err = wire.ScanPreamble(p.br, nonce, wire.MaxPreamble)
	if err != nil {
		p.t.Fatal(err)
	}
	f, err := p.conn.Read()
	if err != nil || f.Type != wire.TypeHello {
		p.t.Fatalf("agent hello = %v, %v", f, err)
	}
	if err := f.JSON(&p.hello); err != nil {
		p.t.Fatal(err)
	}
}

// collect reads frames until exit and returns stdout, stderr, the error frame,
// and the exit status.
func (p *peer) collect() (stdout, stderr string, errMsg *wire.ErrorMsg, code int) {
	p.t.Helper()
	var out, errb strings.Builder
	for {
		f, err := p.conn.Read()
		if err != nil {
			p.t.Fatalf("read: %v (stdout=%q stderr=%q)", err, out.String(), errb.String())
		}
		switch f.Type {
		case wire.TypeStdout:
			out.Write(f.Payload)
		case wire.TypeStderr:
			errb.Write(f.Payload)
		case wire.TypeError:
			var m wire.ErrorMsg
			if err := f.JSON(&m); err != nil {
				p.t.Fatal(err)
			}
			errMsg = &m
		case wire.TypeExit:
			var e wire.Exit
			if err := f.JSON(&e); err != nil {
				p.t.Fatal(err)
			}
			return out.String(), errb.String(), errMsg, e.Code
		case wire.TypePing:
			_ = p.conn.Write(wire.TypePong, nil)
		}
	}
}

func baseRequest(t *testing.T, command string, args ...string) wire.Request {
	t.Helper()
	cfg, _, err := config.Parse([]byte(testTopology), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := config.Digest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return wire.Request{Command: command, Args: args, Topology: testTopology, BaseDir: t.TempDir(), WorkingDir: t.TempDir(), ConfigDigest: digest}
}

func TestHandshakeSkipsNoiseAndReportsAgentFacts(t *testing.T) {
	p := startAgent(t, Options{Version: "v9"})
	p.handshake("")
	if p.hello.Protocol != wire.Protocol || p.hello.Digest != strings.Repeat("a", 64) || p.hello.Version != "v9" || p.hello.Arch == "" {
		t.Fatalf("hello = %+v", p.hello)
	}
}

func TestReadOnlyCommandRunsAndStreamsOutput(t *testing.T) {
	runner := &fakeRunner{run: func(ctx context.Context, command app.Command, rio RunIO) error {
		fmt.Fprint(rio.Stdout, "table output\n")
		fmt.Fprint(rio.Stderr, "diagnostic\n")
		return nil
	}}
	p := startAgent(t, Options{Runner: runner})
	p.handshake("")
	req := baseRequest(t, "ps", "--format", "json")
	req.Helpers = wire.Helpers{QEMUImg: "/opt/qemu-img"}
	if err := p.conn.WriteJSON(wire.TypeRequest, req); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, errMsg, code := p.collect()
	if stdout != "table output\n" || stderr != "diagnostic\n" || errMsg != nil || code != 0 {
		t.Fatalf("got %q %q %v %d", stdout, stderr, errMsg, code)
	}
	if err := <-p.done; err != nil {
		t.Fatal(err)
	}
	cmd := runner.commands[0]
	if cmd.Name != "ps" || len(cmd.Args) != 2 || cmd.Topology.Name != "lists" || cmd.WorkingDir == "" || cmd.Helpers.QEMUImg != "/opt/qemu-img" {
		t.Fatalf("command = %+v", cmd)
	}
}

func TestRunnerErrorPrintsLikeLocalModeAndChildStatusPasses(t *testing.T) {
	tests := map[string]struct {
		err    error
		stderr string
		code   int
	}{
		"plain error":  {errors.New("topology is not up"), "topolab: topology is not up\n", 1},
		"child status": {&app.ChildExitError{Code: 42}, "", 42},
		"canceled":     {context.Canceled, "", 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := startAgent(t, Options{Runner: &fakeRunner{run: func(context.Context, app.Command, RunIO) error { return tt.err }}})
			p.handshake("")
			if err := p.conn.WriteJSON(wire.TypeRequest, baseRequest(t, "counters")); err != nil {
				t.Fatal(err)
			}
			_, stderr, errMsg, code := p.collect()
			if stderr != tt.stderr || errMsg != nil || code != tt.code {
				t.Fatalf("got stderr=%q err=%v code=%d", stderr, errMsg, code)
			}
		})
	}
}

func TestRequestRejections(t *testing.T) {
	tests := map[string]struct {
		mutate func(*wire.Request)
		code   string
	}{
		"unknown command":       {func(r *wire.Request) { r.Command = "rm" }, wire.CodeProtocolError},
		"relative working dir":  {func(r *wire.Request) { r.WorkingDir = "work" }, wire.CodeWorkingDirInvalid},
		"empty working dir":     {func(r *wire.Request) { r.WorkingDir = "" }, wire.CodeWorkingDirInvalid},
		"config digest differs": {func(r *wire.Request) { r.ConfigDigest = strings.Repeat("0", 64) }, wire.CodeProtocolError},
		"no topology":           {func(r *wire.Request) { r.Topology = "" }, wire.CodeProtocolError},
		"bad job id":            {func(r *wire.Request) { r.JobID = "XYZ" }, wire.CodeProtocolError},
		"oversize Root CA": {func(r *wire.Request) {
			r.RootCAs = []wire.RootCAFile{{Path: "/ca", Content: make([]byte, wire.MaxRootCAFile+1)}}
		}, wire.CodeRequestTooLarge},
		"bad template digest": {func(r *wire.Request) { r.Templates = []wire.TemplateRef{{Name: "t", SHA256: "xx"}} }, wire.CodeProtocolError},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			p := startAgent(t, Options{Runner: runner})
			p.handshake("")
			req := baseRequest(t, "ps")
			tt.mutate(&req)
			if err := p.conn.WriteJSON(wire.TypeRequest, req); err != nil {
				t.Fatal(err)
			}
			_, _, errMsg, code := p.collect()
			if errMsg == nil || errMsg.Code != tt.code || code != 1 {
				t.Fatalf("err = %+v code = %d, want %s", errMsg, code, tt.code)
			}
			if len(runner.commands) != 0 {
				t.Fatal("rejected request reached the runner")
			}
		})
	}
}

func TestEnvironmentIsStrict(t *testing.T) {
	topology := "name: lists\nrouters:\n  - name: edge\n    interfaces:\n      - name: external\n        external: {sources: [\"${SRC:-10.0.0.0/24}\"]}\n"
	build := func(env map[string]wire.EnvValue) wire.Request {
		cfg, _, err := config.Parse([]byte(topology), t.TempDir(), func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		digest, _ := config.Digest(cfg)
		return wire.Request{Command: "ps", Topology: topology, BaseDir: t.TempDir(), WorkingDir: t.TempDir(), ConfigDigest: digest, Env: env}
	}
	t.Run("listed unset variable is accepted", func(t *testing.T) {
		p := startAgent(t, Options{})
		p.handshake("")
		if err := p.conn.WriteJSON(wire.TypeRequest, build(map[string]wire.EnvValue{"SRC": {Set: false}})); err != nil {
			t.Fatal(err)
		}
		if _, _, errMsg, code := p.collect(); errMsg != nil || code != 0 {
			t.Fatalf("err=%v code=%d", errMsg, code)
		}
	})
	t.Run("unlisted variable is a divergence", func(t *testing.T) {
		p := startAgent(t, Options{})
		p.handshake("")
		if err := p.conn.WriteJSON(wire.TypeRequest, build(nil)); err != nil {
			t.Fatal(err)
		}
		_, _, errMsg, code := p.collect()
		if errMsg == nil || errMsg.Code != wire.CodeProtocolError || !strings.Contains(errMsg.Message, "env_divergence") || !strings.Contains(errMsg.Message, "SRC") || code != 1 {
			t.Fatalf("err=%+v code=%d", errMsg, code)
		}
	})
}

func TestCancelFrameCancelsTheCommand(t *testing.T) {
	started := make(chan struct{})
	runner := &fakeRunner{run: func(ctx context.Context, _ app.Command, _ RunIO) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	p := startAgent(t, Options{Runner: runner})
	p.handshake("")
	if err := p.conn.WriteJSON(wire.TypeRequest, baseRequest(t, "logs", "--follow")); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := p.conn.Write(wire.TypeCancel, nil); err != nil {
		t.Fatal(err)
	}
	_, _, errMsg, code := p.collect()
	if errMsg != nil || code != 1 {
		t.Fatalf("err=%v code=%d", errMsg, code)
	}
}

func TestConnectionLossCancelsTheCommand(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	runner := &fakeRunner{run: func(ctx context.Context, _ app.Command, _ RunIO) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	p := startAgent(t, Options{Runner: runner})
	p.handshake("")
	if err := p.conn.WriteJSON(wire.TypeRequest, baseRequest(t, "logs")); err != nil {
		t.Fatal(err)
	}
	<-started
	// Closing the controller's write side ends the agent's input.
	closeInput(p)
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("command was not canceled after connection loss")
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after connection loss")
	}
}

// closeInput ends the agent's input stream.
func closeInput(p *peer) { _ = p.clientWrite.Close() }

func TestPeerTimeoutEndsTheSession(t *testing.T) {
	started := make(chan struct{})
	runner := &fakeRunner{run: func(ctx context.Context, _ app.Command, _ RunIO) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	p := startAgent(t, Options{Runner: runner, PingInterval: 20 * time.Millisecond, PeerTimeout: 120 * time.Millisecond})
	p.handshake("")
	if err := p.conn.WriteJSON(wire.TypeRequest, baseRequest(t, "logs")); err != nil {
		t.Fatal(err)
	}
	<-started
	// Stay silent: do not answer pings. The agent must give up.
	select {
	case err := <-p.done:
		if err == nil {
			t.Fatal("Serve returned nil after a peer timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent ignored the peer timeout")
	}
}

func TestPingIsSentAfterSilenceAndAnsweredWithPong(t *testing.T) {
	release := make(chan struct{})
	runner := &fakeRunner{run: func(ctx context.Context, _ app.Command, _ RunIO) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	p := startAgent(t, Options{Runner: runner, PingInterval: 20 * time.Millisecond, PeerTimeout: 5 * time.Second})
	p.handshake("")
	if err := p.conn.WriteJSON(wire.TypeRequest, baseRequest(t, "ps")); err != nil {
		t.Fatal(err)
	}
	gotPing := false
	deadline := time.After(5 * time.Second)
	for !gotPing {
		select {
		case <-deadline:
			t.Fatal("no ping from the agent")
		default:
		}
		f, err := p.conn.Read()
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == wire.TypePing {
			gotPing = true
		}
	}
	// A ping from the controller is answered.
	if err := p.conn.Write(wire.TypePing, nil); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := p.conn.Read()
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == wire.TypePong {
			break
		}
	}
	close(release)
	if _, _, errMsg, code := p.collect(); errMsg != nil || code != 0 {
		t.Fatalf("err=%v code=%d", errMsg, code)
	}
}

func TestBadHelloIsRejected(t *testing.T) {
	p := startAgent(t, Options{})
	if err := p.conn.WriteJSON(wire.TypeHello, wire.Hello{Protocol: 99, Nonce: strings.Repeat("0", 32)}); err != nil {
		t.Fatal(err)
	}
	_, _, errMsg, code := p.collect()
	if errMsg == nil || errMsg.Code != wire.CodeProtocolError || code != 1 {
		t.Fatalf("err=%+v code=%d", errMsg, code)
	}
	if err := <-p.done; !errors.Is(err, wire.ErrProtocol) {
		t.Fatalf("Serve err = %v", err)
	}
}
