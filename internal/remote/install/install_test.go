package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost makes `ssh` run the remote command locally and `sudo` run its
// arguments, so the whole install flow runs without a host.
type fakeHost struct {
	ssh, root, tmp, exe, digest string
	sshOptsLog                  string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{
		ssh:        filepath.Join(bin, "ssh"),
		root:       filepath.Join(dir, "lib", "topolab"),
		tmp:        filepath.Join(dir, "tmp"),
		exe:        filepath.Join(dir, "topolab-controller"),
		sshOptsLog: filepath.Join(dir, "ssh-calls.log"),
	}
	if err := os.MkdirAll(filepath.Dir(h.root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + h.sshOptsLog + "\n" +
		"while [ $# -gt 0 ] && [ \"$1\" != \"--\" ]; do shift; done\n" +
		"shift\nshift\n" +
		"exec sh -c \"$1\"\n"
	if err := os.WriteFile(h.ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("#!/bin/sh\necho pretend agent\n")
	if err := os.WriteFile(h.exe, content, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	h.digest = hex.EncodeToString(sum[:])
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TMPDIR", h.tmp)
	return h
}

func (h *fakeHost) installer() Installer {
	return Installer{SSHPath: h.ssh, Root: h.root, Executable: h.exe, Digest: h.digest, GOOS: "linux", GOARCH: "amd64"}
}

func (h *fakeHost) run(t *testing.T, in Installer) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := in.Install(context.Background(), "ops@lab", []string{"Port=2222"}, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (h *fakeHost) leftovers(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(h.tmp)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestInstallPlacesTheAgentAndCleansUp(t *testing.T) {
	h := newFakeHost(t)
	code, stdout, stderr := h.run(t, h.installer())
	if code != 0 || !strings.Contains(stdout, "agent installed: "+h.root+"/"+h.digest+"/topolab") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	file := filepath.Join(h.root, h.digest, "topolab")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("agent file: %v %v", info, err)
	}
	dirInfo, err := os.Stat(filepath.Dir(file))
	if err != nil || dirInfo.Mode().Perm() != 0o755 {
		t.Fatalf("agent dir: %v %v", dirInfo, err)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "pretend agent") {
		t.Fatalf("content = %q", data)
	}
	if left := h.leftovers(t); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
	if entries, _ := os.ReadDir(h.root); len(entries) != 1 {
		t.Fatalf("install root has %d entries, want 1 (no temporary directory)", len(entries))
	}
	log, _ := os.ReadFile(h.sshOptsLog)
	if !strings.Contains(string(log), "-o Port=2222") || !strings.Contains(string(log), "-t ") {
		t.Fatalf("ssh calls = %q", log)
	}
}

func TestSecondInstallChangesNothing(t *testing.T) {
	h := newFakeHost(t)
	if code, _, stderr := h.run(t, h.installer()); code != 0 {
		t.Fatal(stderr)
	}
	file := filepath.Join(h.root, h.digest, "topolab")
	before, _ := os.Stat(file)
	code, stdout, stderr := h.run(t, h.installer())
	if code != 0 || !strings.Contains(stdout, "already installed") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	after, _ := os.Stat(file)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("second install rewrote the agent")
	}
	if left := h.leftovers(t); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
}

func TestInstallRefusesATamperedInstalledAgent(t *testing.T) {
	h := newFakeHost(t)
	if code, _, stderr := h.run(t, h.installer()); code != 0 {
		t.Fatal(stderr)
	}
	file := filepath.Join(h.root, h.digest, "topolab")
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := h.run(t, h.installer())
	if code != 1 || !strings.Contains(stderr, "does not match its digest") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestInstallRefusesAnUnsafeInstallRoot(t *testing.T) {
	h := newFakeHost(t)
	if err := os.MkdirAll(h.root, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.root, 0o775); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := h.run(t, h.installer())
	if code != 1 || !strings.Contains(stderr, "must be a real directory") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(h.root, h.digest)); !os.IsNotExist(err) {
		t.Fatalf("agent was installed into an unsafe root: %v", err)
	}
	if left := h.leftovers(t); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}

	// A symbolic link as the root is refused too.
	h2 := newFakeHost(t)
	real := filepath.Join(filepath.Dir(h2.root), "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, h2.root); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = h2.run(t, h2.installer())
	if code != 1 || !strings.Contains(stderr, "must be a real directory") {
		t.Fatalf("symlink root: code=%d stderr=%q", code, stderr)
	}
}

func TestInstallRejectsWrongArchitectureAndDigest(t *testing.T) {
	h := newFakeHost(t)
	in := h.installer()
	in.GOARCH = "arm64"
	if code, _, stderr := h.run(t, in); code != 1 || !strings.Contains(stderr, "Linux x86-64") {
		t.Fatalf("arch: code=%d stderr=%q", code, stderr)
	}
	in = h.installer()
	in.Digest = strings.Repeat("0", 64)
	code, _, stderr := h.run(t, in)
	if code != 1 || !strings.Contains(stderr, "staged copy has digest") {
		t.Fatalf("digest: code=%d stderr=%q", code, stderr)
	}
	if left := h.leftovers(t); len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
	in = h.installer()
	in.Digest = "not-a-digest"
	if code, _, stderr := h.run(t, in); code != 1 || !strings.Contains(stderr, "invalid executable digest") {
		t.Fatalf("invalid digest: code=%d stderr=%q", code, stderr)
	}
	in = h.installer()
	in.Root = "/opt/has space"
	if code, _, stderr := h.run(t, in); code != 1 || !strings.Contains(stderr, "invalid install root") {
		t.Fatalf("invalid root: code=%d stderr=%q", code, stderr)
	}
}

func TestSelfDigestMatchesTheExecutable(t *testing.T) {
	digest, path, err := SelfDigest()
	if err != nil || !digestPattern.MatchString(digest) || path == "" {
		t.Fatalf("SelfDigest = %q %q %v", digest, path, err)
	}
}
