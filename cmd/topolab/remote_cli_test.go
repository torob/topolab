package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torob/topolab/internal/remote/client"
)

const routerOnly = `name: x
routers: [{name: r, interfaces: [{name: e, external: {sources: [10.0.0.0/24]}}]}]
`

type recordingRemote struct {
	invocations []client.Invocation
	status      int
}

func (r *recordingRemote) Run(_ context.Context, inv client.Invocation) int {
	r.invocations = append(r.invocations, inv)
	return r.status
}

func writeTopology(t *testing.T, extra string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "topology.yml")
	if err := os.WriteFile(file, []byte(extra+routerOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

type cliResult struct {
	code           int
	stdout, stderr string
	remote         *recordingRemote
	runtime        *recordingRuntime
}

func runCLI(t *testing.T, env map[string]string, euid int, args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	remote := &recordingRemote{}
	runtime := &recordingRuntime{}
	cli := CLI{
		Stdout: &stdout, Stderr: &stderr, EUID: func() int { return euid }, Remote: remote, Runtime: runtime,
		LookupEnv: func(key string) (string, bool) { v, ok := env[key]; return v, ok },
	}
	code := cli.Execute(context.Background(), args)
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String(), remote: remote, runtime: runtime}
}

func TestModeSelectionPrecedence(t *testing.T) {
	work := t.TempDir()
	withYAML := writeTopology(t, "remote: {ssh: yaml@host}\n")
	plain := writeTopology(t, "")
	tests := []struct {
		name        string
		file        string
		env         map[string]string
		flags       []string
		wantRemote  string // empty means local
		wantLocalOK bool
	}{
		{"yaml only", withYAML, nil, nil, "yaml@host", false},
		{"env beats yaml", withYAML, map[string]string{"TOPOLAB_REMOTE": "env@host"}, nil, "env@host", false},
		{"flag beats env and yaml", withYAML, map[string]string{"TOPOLAB_REMOTE": "env@host"}, []string{"--remote", "flag@host"}, "flag@host", false},
		{"local beats yaml", withYAML, nil, []string{"--local"}, "", true},
		{"local beats env", plain, map[string]string{"TOPOLAB_REMOTE": "env@host"}, []string{"--local"}, "", true},
		{"nothing is local", plain, nil, nil, "", true},
		{"env selects remote", plain, map[string]string{"TOPOLAB_REMOTE": "env@host"}, nil, "env@host", false},
		{"empty env is ignored", plain, map[string]string{"TOPOLAB_REMOTE": ""}, nil, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"WORKING_DIR": work}
			for k, v := range tt.env {
				env[k] = v
			}
			args := append(append([]string{}, tt.flags...), "--file", tt.file, "ps")
			euid := 0
			if tt.wantLocalOK {
				euid = 0
			}
			res := runCLI(t, env, euid, args...)
			if tt.wantRemote != "" {
				if res.code != 0 || len(res.remote.invocations) != 1 || res.remote.invocations[0].Destination != tt.wantRemote {
					t.Fatalf("code=%d invocations=%+v stderr=%s", res.code, res.remote.invocations, res.stderr)
				}
				if len(res.runtime.command.Name) != 0 {
					t.Fatal("remote mode also ran the local runtime")
				}
				return
			}
			if len(res.remote.invocations) != 0 || res.runtime.command.Name != "ps" {
				t.Fatalf("not local: remote=%d runtime=%q stderr=%s", len(res.remote.invocations), res.runtime.command.Name, res.stderr)
			}
		})
	}
}

func TestRemoteModeNeedsNoRootAndCarriesTheRequest(t *testing.T) {
	work := t.TempDir()
	file := writeTopology(t, "")
	res := runCLI(t, map[string]string{"WORKING_DIR": work}, 1000,
		"--remote", "ops@lab", "--ssh-opt", "Port=2222", "--ssh-opt", "ProxyJump=bastion", "--reconnect-timeout", "30s",
		"--qemu-img", "/opt/qemu-img", "--file", file, "capture", "link", "--endpoint", "a:b")
	if res.code != 0 || len(res.remote.invocations) != 1 {
		t.Fatalf("code=%d stderr=%s", res.code, res.stderr)
	}
	inv := res.remote.invocations[0]
	if inv.Destination != "ops@lab" || strings.Join(inv.SSHOptions, "|") != "Port=2222|ProxyJump=bastion" || inv.ReconnectTimeout != 30*time.Second {
		t.Fatalf("invocation = %+v", inv)
	}
	if inv.Command != "capture" || strings.Join(inv.Args, " ") != "link --endpoint a:b" || inv.WorkingDir != work {
		t.Fatalf("command = %q args=%q wd=%q", inv.Command, inv.Args, inv.WorkingDir)
	}
	if inv.Helpers.QEMUImg != "/opt/qemu-img" || inv.Topology == nil || inv.Topology.Name != "x" || string(inv.TopologyData) != routerOnly || inv.BaseDir != filepath.Dir(file) {
		t.Fatalf("invocation = %+v", inv)
	}
}

func TestRemoteRouterExecAndSubcommandNames(t *testing.T) {
	work := t.TempDir()
	file := writeTopology(t, "")
	env := map[string]string{"WORKING_DIR": work}
	id := strings.Repeat("a", 32)
	tests := []struct {
		args []string
		name string
		rest string
	}{
		{[]string{"router", "exec", "r", "--", "ip", "a"}, "router-exec", "r -- ip a"},
		{[]string{"jobs", "ls", "--all"}, "jobs-ls", "--all"},
		{[]string{"jobs", "show", id}, "jobs-show", id},
		{[]string{"jobs", "attach", id}, "jobs-attach", id},
		{[]string{"jobs", "cancel", id}, "jobs-cancel", id},
		{[]string{"jobs", "prune"}, "jobs-prune", ""},
		{[]string{"templates", "ls"}, "templates-ls", ""},
		{[]string{"templates", "prune", "--min-age", "48h"}, "templates-prune", "--min-age 48h"},
		{[]string{"up"}, "up", ""},
		{[]string{"exec", "--", "ls"}, "exec", "-- ls"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runCLI(t, env, 1000, append([]string{"--remote", "h", "--file", file}, tt.args...)...)
			if res.code != 0 || len(res.remote.invocations) != 1 {
				t.Fatalf("code=%d stderr=%s", res.code, res.stderr)
			}
			inv := res.remote.invocations[0]
			if inv.Command != tt.name || strings.Join(inv.Args, " ") != tt.rest {
				t.Fatalf("command=%q args=%q", inv.Command, inv.Args)
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	work := t.TempDir()
	file := writeTopology(t, "")
	env := map[string]string{"WORKING_DIR": work}
	id := strings.Repeat("b", 32)
	tests := map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"local and remote":         {[]string{"--local", "--remote", "h", "ps"}, nil, "cannot be combined"},
		"leading dash destination": {[]string{"--remote", "-oProxyCommand=x", "ps"}, nil, "may not begin with '-'"},
		"bad env destination":      {[]string{"ps"}, map[string]string{"TOPOLAB_REMOTE": "a b"}, "whitespace"},
		"bad reconnect timeout":    {[]string{"--reconnect-timeout", "5", "--remote", "h", "ps"}, nil, "--reconnect-timeout"},
		"negative reconnect":       {[]string{"--reconnect-timeout", "-1s", "--remote", "h", "ps"}, nil, "--reconnect-timeout"},
		"relative working dir":     {[]string{"--remote", "h", "--working-dir", "rel", "ps"}, nil, "absolute path in remote mode"},
		"jobs without sub":         {[]string{"--remote", "h", "jobs"}, nil, "requires a subcommand"},
		"jobs unknown sub":         {[]string{"--remote", "h", "jobs", "rm"}, nil, "unknown jobs subcommand"},
		"jobs show without id":     {[]string{"--remote", "h", "jobs", "show"}, nil, "job identifier"},
		"jobs show bad id":         {[]string{"--remote", "h", "jobs", "show", "XYZ"}, nil, "job identifier"},
		"jobs ls bad flag":         {[]string{"--remote", "h", "jobs", "ls", "-x"}, nil, "only --all"},
		"jobs prune extra":         {[]string{"--remote", "h", "jobs", "prune", id}, nil, "accepts no arguments"},
		"templates prune bad":      {[]string{"--remote", "h", "templates", "prune", "--max-age", "1h"}, nil, "only --min-age"},
		"jobs in local mode":       {[]string{"jobs", "ls"}, nil, "jobs ls requires remote mode"},
		"templates in local mode":  {[]string{"templates", "ls"}, nil, "templates ls requires remote mode"},
		"jobs with --local":        {[]string{"--local", "jobs", "ls"}, map[string]string{"TOPOLAB_REMOTE": "h"}, "requires remote mode"},
		"agent without sub":        {[]string{"agent"}, nil, "requires the install subcommand"},
		"agent stdio via CLI":      {[]string{"agent", "--stdio"}, nil, "requires the install subcommand"},
		"agent install no host":    {[]string{"agent", "install"}, nil, "requires --remote or TOPOLAB_REMOTE"},
		"agent install extra":      {[]string{"--remote", "h", "agent", "install", "x"}, nil, "accepts no arguments"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e := map[string]string{"WORKING_DIR": work}
			for k, v := range env {
				e[k] = v
			}
			for k, v := range tt.env {
				e[k] = v
			}
			res := runCLI(t, e, 0, append([]string{"--file", file}, tt.args...)...)
			if res.code != 2 || !strings.Contains(res.stderr, tt.want) {
				t.Fatalf("code=%d stderr=%q, want usage error containing %q", res.code, res.stderr, tt.want)
			}
			if len(res.remote.invocations) != 0 || res.runtime.command.Name != "" {
				t.Fatal("a rejected command ran")
			}
		})
	}
}

func TestRemoteConfigNeverContactsTheHostAndPrintsRemote(t *testing.T) {
	work := t.TempDir()
	file := writeTopology(t, "remote: {ssh: yaml@host}\n")
	res := runCLI(t, map[string]string{"WORKING_DIR": work}, 1000, "--file", file, "config")
	if res.code != 0 || len(res.remote.invocations) != 0 {
		t.Fatalf("code=%d invocations=%d stderr=%s", res.code, len(res.remote.invocations), res.stderr)
	}
	if !strings.Contains(res.stdout, "remote:") || !strings.Contains(res.stdout, "yaml@host") {
		t.Fatalf("config output lacks remote:\n%s", res.stdout)
	}
	// A relative working directory is still a usage error.
	res = runCLI(t, map[string]string{"WORKING_DIR": "rel"}, 1000, "--file", file, "config")
	if res.code != 2 {
		t.Fatalf("relative working directory: code=%d stderr=%s", res.code, res.stderr)
	}
	// The working directory is required in remote mode.
	res = runCLI(t, nil, 1000, "--file", file, "config")
	if res.code != 1 || !strings.Contains(res.stderr, "working directory is required") {
		t.Fatalf("missing working directory: code=%d stderr=%s", res.code, res.stderr)
	}
}

func TestRemoteModeDoesNotResolveTheWorkingDirectoryLocally(t *testing.T) {
	// A path that the controller cannot even stat is fine: it names a host path.
	file := writeTopology(t, "")
	res := runCLI(t, nil, 1000, "--remote", "h", "--working-dir", "/proc/1/root/forbidden/lab", "--file", file, "ps")
	if res.code != 0 || len(res.remote.invocations) != 1 || res.remote.invocations[0].WorkingDir != "/proc/1/root/forbidden/lab" {
		t.Fatalf("code=%d stderr=%s", res.code, res.stderr)
	}
	res = runCLI(t, nil, 1000, "--remote", "h", "--working-dir", "/", "--file", file, "ps")
	if res.code != 1 || !strings.Contains(res.stderr, "filesystem root") {
		t.Fatalf("root working directory: code=%d stderr=%s", res.code, res.stderr)
	}
}

func TestRecordedEnvironmentContainsOnlyLookedUpNames(t *testing.T) {
	work := t.TempDir()
	file := filepath.Join(t.TempDir(), "topology.yml")
	doc := "name: x\nrouters: [{name: r, interfaces: [{name: e, external: {sources: [\"${SOURCE:-10.0.0.0/24}\"]}}]}]\n"
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"WORKING_DIR": work, "TOPOLAB_REMOTE": "h", "AWS_SECRET_ACCESS_KEY": "do-not-send", "SOURCE": "10.1.0.0/24"}
	res := runCLI(t, env, 1000, "--file", file, "ps")
	if res.code != 0 {
		t.Fatalf("stderr=%s", res.stderr)
	}
	got := res.remote.invocations[0].Env
	if len(got) != 1 || !got["SOURCE"].Set || got["SOURCE"].Value != "10.1.0.0/24" {
		t.Fatalf("recorded env = %+v", got)
	}
}

func TestRemoteWarningsComeFromTheAgentExceptForConfig(t *testing.T) {
	work := t.TempDir()
	file := filepath.Join(t.TempDir(), "topology.yml")
	doc := "name: x\nrouters: [{name: r, interfaces: [{name: e, external: {sources: [\"${UNSET_VAR}10.0.0.0/24\"]}}]}]\n"
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"WORKING_DIR": work, "TOPOLAB_REMOTE": "h"}
	if res := runCLI(t, env, 1000, "--file", file, "ps"); strings.Contains(res.stderr, "warning:") {
		t.Fatalf("controller printed a warning that the agent owns: %q", res.stderr)
	}
	if res := runCLI(t, env, 1000, "--file", file, "config"); !strings.Contains(res.stderr, "warning:") {
		t.Fatalf("config did not print the warning: %q", res.stderr)
	}
}

func TestParseReconnectTimeout(t *testing.T) {
	for in, want := range map[string]time.Duration{"0": 0, "250ms": 250 * time.Millisecond, "2m": 2 * time.Minute, "1h": time.Hour, "15s": 15 * time.Second} {
		got, err := parseReconnectTimeout(in)
		if err != nil || got != want {
			t.Errorf("parseReconnectTimeout(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "5", "-1s", "1.5s", "01s", "5d", "m", "0s"} {
		if _, err := parseReconnectTimeout(in); err == nil {
			t.Errorf("parseReconnectTimeout(%q) succeeded", in)
		}
	}
}

// writeQcow2 writes a minimal qcow2 header that passes template validation.
func writeQcow2(t *testing.T) string {
	t.Helper()
	header := make([]byte, 64)
	copy(header, "QFI\xfb")
	binary.BigEndian.PutUint32(header[4:8], 3)
	path := filepath.Join(t.TempDir(), "base.qcow2")
	if err := os.WriteFile(path, header, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRemoteTemplateFileIsCheckedOnlyOnTheMachineWhereItLives(t *testing.T) {
	env := map[string]string{"WORKING_DIR": t.TempDir()}
	missing := filepath.Join(t.TempDir(), "absent.qcow2")
	file := writeTopology(t, "templates:\n  - {name: base, path: "+missing+", remote: true}\n")

	// Remote mode: the file belongs to the host, so the controller neither reads
	// it nor fails because it is absent here.
	res := runCLI(t, env, 1000, "--remote", "ops@lab", "--file", file, "config")
	if res.code != 0 || len(res.remote.invocations) != 0 || !strings.Contains(res.stdout, "remote: true") {
		t.Fatalf("remote config: code=%d stdout=%s stderr=%s", res.code, res.stdout, res.stderr)
	}
	res = runCLI(t, env, 1000, "--remote", "ops@lab", "--file", file, "ps")
	if res.code != 0 || len(res.remote.invocations) != 1 {
		t.Fatalf("remote ps: code=%d stderr=%s", res.code, res.stderr)
	}

	// Local mode: this machine is the host, so the file is checked like `path`.
	res = runCLI(t, env, 1000, "--local", "--file", file, "config")
	if res.code != 1 || !strings.Contains(res.stderr, `template "base"`) {
		t.Fatalf("local config of a missing file: code=%d stderr=%s", res.code, res.stderr)
	}
	res = runCLI(t, env, 0, "--local", "--file", file, "up")
	if res.code != 1 || res.runtime.command.Name != "" {
		t.Fatalf("local up of a missing file: code=%d ran=%q stderr=%s", res.code, res.runtime.command.Name, res.stderr)
	}
	good := writeTopology(t, "templates:\n  - {name: base, path: "+writeQcow2(t)+", remote: true}\n")
	res = runCLI(t, env, 1000, "--local", "--file", good, "config")
	if res.code != 0 || !strings.Contains(res.stdout, "remote: true") {
		t.Fatalf("local config of a good file: code=%d stderr=%s", res.code, res.stderr)
	}
}
