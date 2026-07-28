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
