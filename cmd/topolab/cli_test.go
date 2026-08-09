package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torob/topolab/internal/app"
)

func TestCLIConfigIsInjectableAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "topology.yml")
	doc := `name: cli
routers:
  - name: edge
    interfaces:
      - name: wan
        external: {sources: [10.0.0.0/24]}
`
	if err := os.WriteFile(file, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(dir, "does-not-exist")
	var out, stderr bytes.Buffer
	cli := CLI{Stdout: &out, Stderr: &stderr, LookupEnv: func(key string) (string, bool) {
		if key == "WORKING_DIR" {
			return working, true
		}
		return "", false
	}, EUID: func() int { return 1234 }}
	if code := cli.Execute(context.Background(), []string{"--file", file, "config"}); code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(working); !os.IsNotExist(err) {
		t.Fatalf("config created working directory: %v", err)
	}
	if !strings.Contains(out.String(), "egress_interface: auto") || !strings.Contains(out.String(), "source_ip: auto") {
		t.Fatalf("missing defaults:\n%s", out.String())
	}
}

func TestCLIRegistersRuntimeCommandsAndRequiresRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(file, []byte("name: x\nrouters: [{name: r, interfaces: [{name: e, external: {sources: [10.0.0.0/24]}}]}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"up", "ps", "logs", "capture", "counters", "start", "stop", "restart", "down", "exec"} {
		var stderr bytes.Buffer
		code := (CLI{Stdout: &bytes.Buffer{}, Stderr: &stderr, EUID: func() int { return 1000 }}).Execute(context.Background(), []string{"--file", file, "--working-dir", filepath.Join(dir, "work"), command})
		if code != 1 || !strings.Contains(stderr.String(), "requires effective UID 0") {
			t.Errorf("%s: code=%d stderr=%q", command, code, stderr.String())
		}
	}
}

type exitRuntime struct{ code int }

func (r exitRuntime) Run(context.Context, RuntimeCommand) error {
	return &app.ChildExitError{Code: r.code}
}

type recordingRuntime struct {
	command RuntimeCommand
}

func (r *recordingRuntime) Run(_ context.Context, command RuntimeCommand) error {
	r.command = command
	return nil
}

func TestCLIVersionNeedsNoTopologyWorkingDirectoryOrRoot(t *testing.T) {
	old := version
	version = "v1.2.3"
	t.Cleanup(func() { version = old })
	var out, stderr bytes.Buffer
	cli := CLI{Stdout: &out, Stderr: &stderr, EUID: func() int { return 1000 }}
	if code := cli.Execute(context.Background(), []string{"--file", filepath.Join(t.TempDir(), "missing.yml"), "version"}); code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, stderr.String())
	}
	if out.String() != "topolab 1.2.3\n" {
		t.Fatalf("version output = %q", out.String())
	}
}

func TestResolveVersionSources(t *testing.T) {
	for _, test := range []struct {
		injected, module, sum, want string
		local                       bool
	}{
		{"2.0.0", "v1.0.0", "h1:sum", "2.0.0", true},
		{"", "v1.4.5", "h1:sum", "1.4.5", false},
		{"", "v1.4.5", "", "devel", false},
		{"", "v1.4.5", "h1:sum", "devel", true},
		{"", "(devel)", "", "devel", false},
		{"", "", "", "devel", false},
	} {
		if got := resolveVersion(test.injected, test.module, test.sum, test.local); got != test.want {
			t.Errorf("resolveVersion(%q, %q, %q, %t) = %q, want %q", test.injected, test.module, test.sum, test.local, got, test.want)
		}
	}
}

func TestCLIRouterExecNestedParsingAndHelperOverrides(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(file, []byte("name: x\nrouters: [{name: edge, interfaces: [{name: wan, external: {sources: [10.0.0.0/24]}}]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &recordingRuntime{}
	cli := CLI{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, EUID: func() int { return 0 }, Runtime: runtime}
	args := []string{"--file", file, "--working-dir", filepath.Join(dir, "work"), "--qemu-system", "/helpers/qemu", "router", "exec", "edge", "--user", "root", "--cwd", "/tmp", "--", "ip", "route", "show"}
	if code := cli.Execute(context.Background(), args); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if runtime.command.Name != "router-exec" || strings.Join(runtime.command.Args, "|") != "edge|--user|root|--cwd|/tmp|--|ip|route|show" {
		t.Fatalf("runtime command = %#v", runtime.command)
	}
	if runtime.command.Helpers.QEMUSystem != "/helpers/qemu" {
		t.Fatalf("helper override missing: %#v", runtime.command.Helpers)
	}
}

func TestCLIRouterRequiresExecAndName(t *testing.T) {
	for _, args := range [][]string{{"router"}, {"router", "show"}, {"router", "exec"}, {"router", "exec", "--user"}} {
		var stderr bytes.Buffer
		code := (CLI{Stdout: &bytes.Buffer{}, Stderr: &stderr}).Execute(context.Background(), args)
		if code != 2 {
			t.Errorf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestCLIPropagatesExactChildExitCode(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(file, []byte("name: x\nrouters: [{name: r, interfaces: [{name: e, external: {sources: [10.0.0.0/24]}}]}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cli := CLI{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, EUID: func() int { return 0 }, Runtime: exitRuntime{code: 42}}
	if code := cli.Execute(context.Background(), []string{"--file", file, "--working-dir", filepath.Join(dir, "work"), "exec", "--", "false"}); code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}
