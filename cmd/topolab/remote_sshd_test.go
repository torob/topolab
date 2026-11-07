package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/remote/agent"
	"github.com/torob/topolab/internal/remote/client"
)

// payload is a deterministic binary stream that contains every byte value.
func payload(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*131 + i>>8)
	}
	return out
}

func init() {
	// The test agent writes a large binary stream for the "logs" command.
	bigPayload = payload(3 << 20)
}

var bigPayload []byte

type binaryAgentRunner struct{ testAgentRunner }

func (binaryAgentRunner) Run(ctx context.Context, command app.Command, rio agent.RunIO) error {
	switch command.Name {
	case "logs":
		_, err := rio.Stdout.Write(bigPayload)
		return err
	case "down":
		// A slow lifecycle command: it finishes when the release file appears.
		fmt.Fprintln(rio.Stdout, "begin")
		release := filepath.Join(command.WorkingDir, "release")
		for {
			if _, err := os.Stat(release); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
		fmt.Fprintln(rio.Stdout, "end")
		return nil
	}
	return testAgentRunner{}.Run(ctx, command, rio)
}

// startSSHD runs an sshd for the current user on a loopback port and returns the
// ssh options that reach it. The test is opt-in: it needs /usr/sbin/sshd.
func startSSHD(t *testing.T) []string {
	t.Helper()
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		sshd = "/usr/sbin/sshd"
		if _, statErr := os.Stat(sshd); statErr != nil {
			t.Skip("sshd is not installed")
		}
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, "hostkey")},
		{"-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, "client")},
	} {
		if out, err := exec.Command("ssh-keygen", args...).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v: %s", err, out)
		}
	}
	pub, err := os.ReadFile(filepath.Join(dir, "client.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "authorized_keys"), pub, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	config := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s/hostkey
AuthorizedKeysFile %s/authorized_keys
PidFile %s/sshd.pid
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
StrictModes no
AcceptEnv PATH TOPOLAB_TEST_AGENT
LogLevel ERROR
`, port, dir, dir, dir)
	if err := os.WriteFile(filepath.Join(dir, "sshd_config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	cmd := exec.Command(sshd, "-f", filepath.Join(dir, "sshd_config"), "-D", "-e")
	cmd.Stdout, cmd.Stderr = &logBuf, &logBuf
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sshd: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Skipf("sshd did not start: %s", logBuf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return []string{
		fmt.Sprintf("Port=%d", port),
		"IdentityFile=" + filepath.Join(dir, "client"),
		"IdentitiesOnly=yes",
		"StrictHostKeyChecking=no", // a throwaway host key on loopback
		"UserKnownHostsFile=/dev/null",
		"LogLevel=ERROR",
		"SendEnv=PATH TOPOLAB_TEST_AGENT",
	}
}

// TestRealSSHTransport runs the controller through the real ssh client and a
// real sshd. It is opt-in because it starts a daemon: set TOPOLAB_TEST_SSHD=1.
func TestRealSSHTransport(t *testing.T) {
	if os.Getenv("TOPOLAB_TEST_SSHD") != "1" {
		t.Skip("set TOPOLAB_TEST_SSHD=1 to run the test against a real sshd")
	}
	lab := newSSHLab(t, "")
	lab.installAgent(t)
	options := startSSHD(t)

	run := func(command string, args ...string) (int, []byte, string) {
		file := writeTopology(t, "")
		var stdout, stderr bytes.Buffer
		inv := client.Invocation{
			Destination: "localhost", SSHOptions: options, Command: command, Args: args,
			Stdout: &stdout, Stderr: &stderr,
		}
		cfgResult := loadForTest(t, file)
		inv.Topology, inv.TopologyData, inv.BaseDir = cfgResult.Config, cfgResult.Data, cfgResult.BaseDir
		inv.WorkingDir = t.TempDir()
		r := &client.Runner{
			Dialer:      client.SSHDialer{Digest: e2eDigest, InstallRoot: lab.root},
			Digest:      e2eDigest,
			InstallRoot: lab.root,
		}
		return r.Run(context.Background(), inv), stdout.Bytes(), stderr.String()
	}

	code, stdout, stderr := run("ps")
	if code != 0 || string(stdout) != "ran ps on x\n" {
		t.Fatalf("ps: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if code, _, stderr := run("counters"); code != 9 {
		t.Fatalf("counters: code=%d stderr=%q", code, stderr)
	}
	code, stdout, stderr = run("logs")
	if code != 0 || !bytes.Equal(stdout, bigPayload) {
		t.Fatalf("logs: code=%d got %d bytes, want %d identical bytes; stderr=%q", code, len(stdout), len(bigPayload), stderr)
	}
	// A missing agent is reported through the wrapper's exit status.
	if err := os.RemoveAll(lab.root); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = run("ps")
	if code != 1 || !strings.Contains(stderr, "agent_missing") {
		t.Fatalf("missing agent: code=%d stderr=%q", code, stderr)
	}
}
