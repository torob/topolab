// Package install puts the controller's own executable on a host as the agent
// (spec.md Section 12.3).
package install

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"github.com/torob/topolab/internal/fsx"
	"github.com/torob/topolab/internal/remote/client"
)

// SelfDigest returns the SHA-256 digest of the running executable and its path.
func SelfDigest() (digest, path string, err error) {
	path, err = os.Executable()
	if err != nil {
		return "", "", err
	}
	digest, _, err = fsx.HashFile(path)
	if err != nil {
		return "", "", err
	}
	return digest, path, nil
}

var (
	digestPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	stagingPattern = regexp.MustCompile(`^/[A-Za-z0-9._/+-]+$`)
)

// Installer installs the agent.
type Installer struct {
	// SSHPath is the ssh executable; empty means "ssh" from PATH.
	SSHPath string
	// Root is the install root on the host; empty means the default.
	Root string
	// Executable and Digest identify the file to install; empty values mean the
	// running executable.
	Executable string
	Digest     string
	// GOOS and GOARCH default to the running program's values.
	GOOS, GOARCH string
}

// stageScript receives the executable on standard input into a private
// directory, hashes it, and prints the digest and the directory.
const stageScript = `umask 077; d=$(mktemp -d) || exit 1; cat > "$d/topolab" || exit 1; h=$(sha256sum "$d/topolab" | cut -d" " -f1); echo "$h"; echo "$d"`

// rootScript runs as root. It never trusts the staged file: it copies it into a
// root-owned directory and checks the digest of the copy.
func rootScript(root, digest, staged string) string {
	return fmt.Sprintf(`set -eu
root=%s
digest=%s
src=%s
umask 022
dest="$root/$digest"
me=$(id -u)
check_dir() { [ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c %%u "$1")" = "$me" ] && [ "$(( 0$(stat -c %%a "$1") & 0022 ))" = 0 ]; }
mkdir -p "$root"
check_dir "$root" || { echo "topolab: $root must be a real directory, owned by the installing account, and not writable by group or others" >&2; exit 1; }
if [ -e "$dest/topolab" ]; then
  h=$(sha256sum "$dest/topolab" | cut -d" " -f1)
  [ "$h" = "$digest" ] || { echo "topolab: the agent installed at $dest does not match its digest" >&2; exit 1; }
  echo "agent already installed: $dest/topolab"
  exit 0
fi
tmp=$(mktemp -d "$root/.install-XXXXXX")
trap 'rm -rf "$tmp"' EXIT
cp "$src" "$tmp/topolab"
chmod 0755 "$tmp/topolab" "$tmp"
h=$(sha256sum "$tmp/topolab" | cut -d" " -f1)
[ "$h" = "$digest" ] || { echo "topolab: the staged file does not match its digest" >&2; exit 1; }
mv -T "$tmp" "$dest"
trap - EXIT
echo "agent installed: $dest/topolab"
if grep -Rqs '^[[:space:]]*KillUserProcesses=yes' /etc/systemd/logind.conf /etc/systemd/logind.conf.d 2>/dev/null; then
  echo "warning: logind kills user processes at logout (KillUserProcesses=yes); jobs and VMs started in an SSH session would stop" >&2
fi
`, root, digest, staged)
}

// rootCommand is the remote command of phase 2. The script travels as base64 so
// that no login shell can change it. The command runs it directly when the
// account is root and through sudo otherwise.
func rootCommand(script string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return `sh -c 's=$(printf %s ` + encoded + ` | base64 -d); if [ "$(id -u)" = 0 ]; then sh -c "$s" topolab-install; else sudo sh -c "$s" topolab-install; fi'`
}

// Install implements the agent install command and returns its exit status.
func (in Installer) Install(ctx context.Context, destination string, sshOptions []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "topolab: "+format+"\n", args...)
		return 1
	}
	goos, goarch := in.GOOS, in.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if goos != "linux" || goarch != "amd64" {
		return fail("agent install needs a Linux x86-64 controller executable; this one is %s/%s", goos, goarch)
	}
	exe, digest := in.Executable, in.Digest
	if exe == "" || digest == "" {
		var err error
		digest, exe, err = SelfDigest()
		if err != nil {
			return fail("hash the running executable: %v", err)
		}
	}
	if !digestPattern.MatchString(digest) {
		return fail("invalid executable digest %q", digest)
	}
	root := in.Root
	if root == "" {
		root = client.DefaultInstallRoot
	}
	if !stagingPattern.MatchString(root) {
		return fail("invalid install root %q", root)
	}
	ssh := in.SSHPath
	if ssh == "" {
		path, err := client.LookSSH()
		if err != nil {
			return fail("%v", err)
		}
		ssh = path
	}

	// Phase 1: as the SSH user, without sudo.
	file, err := os.Open(exe)
	if err != nil {
		return fail("open %s: %v", exe, err)
	}
	defer file.Close()
	stageArgs := append(client.SSHBaseArgs(false, sshOptions, destination), "sh -c '"+stageScript+"'")
	stage := exec.CommandContext(ctx, ssh, stageArgs...)
	stage.Stdin = file
	stage.Stderr = stderr
	out, err := stage.Output()
	if err != nil {
		return fail("stage the agent on %s: %v", destination, err)
	}
	lines := strings.Fields(string(out))
	if len(lines) != 2 {
		return fail("unexpected staging output %q", out)
	}
	stagedDigest, staged := lines[0], lines[1]
	if stagedDigest != digest {
		cleanup(ctx, ssh, sshOptions, destination, staged, stderr)
		return fail("the staged copy has digest %s, want %s", stagedDigest, digest)
	}
	if !stagingPattern.MatchString(staged) {
		return fail("unexpected staging directory %q", staged)
	}
	staged += "/topolab"
	defer cleanup(context.WithoutCancel(ctx), ssh, sshOptions, destination, strings.TrimSuffix(staged, "/topolab"), stderr)

	// Phase 2: as root. -t lets sudo ask for a password.
	installArgs := append(client.SSHBaseArgs(true, sshOptions, destination), rootCommand(rootScript(root, digest, staged)))
	install := exec.CommandContext(ctx, ssh, installArgs...)
	install.Stdin, install.Stdout, install.Stderr = stdin, stdout, stderr
	if err := install.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fail("install the agent on %s failed (exit status %d)", destination, exitErr.ExitCode())
		}
		return fail("install the agent on %s: %v", destination, err)
	}
	return 0
}

// cleanup removes the staging directory.
func cleanup(ctx context.Context, ssh string, sshOptions []string, destination, dir string, stderr io.Writer) {
	if !stagingPattern.MatchString(dir) {
		return
	}
	args := append(client.SSHBaseArgs(false, sshOptions, destination), "rm -rf -- "+dir)
	cmd := exec.CommandContext(ctx, ssh, args...)
	cmd.Stderr = stderr
	_ = cmd.Run()
}
