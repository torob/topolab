package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/remote/agent"
	"github.com/torob/topolab/internal/remote/client"
	"github.com/torob/topolab/internal/remote/jobs"
	"golang.org/x/sys/unix"
)

const e2eDigest = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0"

// TestMain lets this test binary act as the agent: the install directory that
// the tests build contains a script that runs it with TOPOLAB_TEST_AGENT=1.
func TestMain(m *testing.M) {
	if os.Getenv("TOPOLAB_TEST_AGENT") == "1" && len(os.Args) == 3 && os.Args[1] == "__job" {
		os.Exit(agent.RunJob(context.Background(), os.Args[2], binaryAgentRunner{}))
	}
	if os.Getenv("TOPOLAB_TEST_AGENT") == "1" && len(os.Args) == 3 && os.Args[1] == "agent" && os.Args[2] == "--stdio" {
		os.Exit(runAgent(context.Background(), os.Stdin, os.Stdout, os.Stderr, 0, 1, agentConfig{
			EUID:       func() int { return 0 },
			IsTerminal: func(int) bool { return false },
			Runner:     binaryAgentRunner{},
			Digest:     func() (string, error) { return e2eDigest, nil },
			Version:    "test",
		}))
	}
	os.Exit(m.Run())
}

// testAgentRunner answers `ps` without touching a host.
type testAgentRunner struct{}

func (testAgentRunner) Run(_ context.Context, command app.Command, rio agent.RunIO) error {
	fmt.Fprintf(rio.Stdout, "ran %s on %s\n", command.Name, command.Topology.Name)
	if command.Name == "counters" {
		return &app.ChildExitError{Code: 9}
	}
	return nil
}

type sshLab struct {
	ssh, root string
	sudoDeny  string
}

func newSSHLab(t *testing.T, banner string) *sshLab {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	lab := &sshLab{ssh: filepath.Join(bin, "ssh"), root: filepath.Join(dir, "lib")}
	// ssh: print login-script noise, drop options up to "--", run the command.
	script := "#!/bin/sh\n"
	if banner != "" {
		script += "echo '" + banner + "'\n"
	}
	script += "while [ $# -gt 0 ] && [ \"$1\" != \"--\" ]; do shift; done\nshift\nshift\nexec sh -c \"$1\"\n"
	if err := os.WriteFile(lab.ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sudo := "#!/bin/sh\n[ \"$1\" = \"-n\" ] && shift\nif [ \"$1\" = \"-l\" ]; then [ -z \"$FAKE_SUDO_DENY\" ] && exit 0; exit 1; fi\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return lab
}

func (l *sshLab) installAgent(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(l.root, e2eDigest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nTOPOLAB_TEST_AGENT=1 exec " + self + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "topolab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (l *sshLab) runner() RemoteRunner {
	return realRunner{lab: l}
}

type realRunner struct{ lab *sshLab }

func (r realRunner) Run(ctx context.Context, inv client.Invocation) int {
	run := &client.Runner{Dialer: client.SSHDialer{Path: r.lab.ssh, Digest: e2eDigest, InstallRoot: r.lab.root}, Digest: e2eDigest, InstallRoot: r.lab.root}
	return run.Run(ctx, inv)
}

// loadForTest loads a topology file for an invocation.
func loadForTest(t *testing.T, file string) *config.Result {
	t.Helper()
	result, err := config.LoadTopology(config.LoadOptions{File: file, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func runViaSSH(t *testing.T, lab *sshLab, args ...string) (int, string, string) {
	t.Helper()
	file := writeTopology(t, "")
	work := t.TempDir()
	var stdout, stderr bytes.Buffer
	cli := CLI{
		Stdout: &stdout, Stderr: &stderr, EUID: func() int { return 1000 }, Remote: lab.runner(),
		LookupEnv: func(key string) (string, bool) {
			if key == "WORKING_DIR" {
				return work, true
			}
			return "", false
		},
	}
	code := cli.Execute(context.Background(), append([]string{"--remote", "ops@lab", "--file", file}, args...))
	return code, stdout.String(), stderr.String()
}

func TestEndToEndThroughTheSSHWrapper(t *testing.T) {
	lab := newSSHLab(t, "Welcome to the lab host")
	lab.installAgent(t)
	code, stdout, stderr := runViaSSH(t, lab, "ps")
	if code != 0 || stdout != "ran ps on x\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "Welcome to the lab host") {
		t.Fatalf("login noise was lost: %q", stderr)
	}
	// The agent's exit status passes through the wrapper and the protocol.
	if code, _, stderr := runViaSSH(t, lab, "counters"); code != 9 {
		t.Fatalf("counters: code=%d stderr=%q", code, stderr)
	}
}

func TestMissingAgentAndDeniedSudoAreReportedByCode(t *testing.T) {
	lab := newSSHLab(t, "")
	code, _, stderr := runViaSSH(t, lab, "ps")
	if code != 1 || !strings.Contains(stderr, "topolab: remote: agent_missing:") {
		t.Fatalf("missing agent: code=%d stderr=%q", code, stderr)
	}
	lab.installAgent(t)
	t.Setenv("FAKE_SUDO_DENY", "1")
	code, _, stderr = runViaSSH(t, lab, "ps")
	if code != 1 || !strings.Contains(stderr, "topolab: remote: privilege_denied:") || !strings.Contains(stderr, "NOPASSWD: "+lab.root+"/"+e2eDigest+"/topolab agent --stdio") {
		t.Fatalf("denied sudo: code=%d stderr=%q", code, stderr)
	}
}

func TestAgentRefusesWithoutRootOrOnATerminal(t *testing.T) {
	for name, cfg := range map[string]agentConfig{
		"not root": {EUID: func() int { return 1000 }, IsTerminal: func(int) bool { return false }},
		"terminal": {EUID: func() int { return 0 }, IsTerminal: func(fd int) bool { return fd == 1 }},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := runAgent(context.Background(), strings.NewReader(""), &bytes.Buffer{}, &stderr, 0, 1, cfg)
			if code != agentRefused || stderr.Len() == 0 {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}

// recordingDialer remembers the transport of the latest connection.
type recordingDialer struct {
	inner client.Dialer
	mu    sync.Mutex
	last  *client.Transport
}

func (d *recordingDialer) Dial(ctx context.Context, inv client.Invocation) (*client.Transport, error) {
	tr, err := d.inner.Dial(ctx, inv)
	d.mu.Lock()
	d.last = tr
	d.mu.Unlock()
	return tr, err
}

func (d *recordingDialer) transport() *client.Transport {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// TestJobSurvivesTheEndOfItsSSHSessionAsARealProcess starts a real runner
// process through the agent, kills the SSH session, and checks that the job
// goes on in its own process and can be followed afterwards.
func TestJobSurvivesTheEndOfItsSSHSessionAsARealProcess(t *testing.T) {
	lab := newSSHLab(t, "")
	lab.installAgent(t)
	file := writeTopology(t, "")
	work := t.TempDir()
	rec := &recordingDialer{inner: client.SSHDialer{Path: lab.ssh, Digest: e2eDigest, InstallRoot: lab.root}}
	newRunner := func() *client.Runner {
		return &client.Runner{Dialer: rec, Digest: e2eDigest, InstallRoot: lab.root, BackoffInitial: 10 * time.Millisecond}
	}
	result := loadForTest(t, file)
	invocation := func(command string, args ...string) (client.Invocation, *lockedBuffer, *lockedBuffer) {
		stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
		return client.Invocation{
			Destination: "ops@lab", Command: command, Args: args, Topology: result.Config, TopologyData: result.Data,
			BaseDir: result.BaseDir, WorkingDir: work, Stdout: stdout, Stderr: stderr, ReconnectTimeout: 0,
		}, stdout, stderr
	}

	inv, stdout, stderr := invocation("down")
	done := make(chan int, 1)
	go func() { done <- newRunner().Run(context.Background(), inv) }()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(stdout.String(), "begin") {
		if time.Now().After(deadline) {
			t.Fatalf("the job did not start; stderr=%q", stderr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.transport().Kill() // the SSH session dies
	select {
	case code := <-done:
		if code != client.ExitDetached {
			t.Fatalf("code=%d stderr=%q", code, stderr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the controller did not give up")
	}

	list, err := jobs.NewStore(work).List("x")
	if err != nil || len(list) != 1 {
		t.Fatalf("jobs = %+v %v", list, err)
	}
	st := list[0]
	if st.State != jobs.StateRunning || st.Runner.PID == 0 || st.Runner.PID == os.Getpid() {
		t.Fatalf("the job is not running in its own process: %+v", st)
	}
	if err := syscall.Kill(st.Runner.PID, 0); err != nil {
		t.Fatalf("the runner died with the SSH session: %v", err)
	}
	if sid, err := unix.Getsid(st.Runner.PID); err != nil || sid != st.Runner.PID {
		t.Fatalf("the runner is not its own session leader: sid=%d err=%v", sid, err)
	}

	// Let the job finish, then follow it from a new connection.
	if err := os.WriteFile(filepath.Join(work, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	attach, out, errOut := invocation("jobs-attach", st.ID)
	attach.ReconnectTimeout = 5 * time.Second
	if code := newRunner().Run(context.Background(), attach); code != 0 || out.String() != "begin\nend\n" {
		t.Fatalf("attach: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	final, err := jobs.NewStore(work).Load("x", st.ID)
	if err != nil || final.State != jobs.StateSucceeded {
		t.Fatalf("final = %+v %v", final, err)
	}
}

// lockedBuffer is a bytes.Buffer that a test can read while a client writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
