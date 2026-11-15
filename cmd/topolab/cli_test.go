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
	args := []string{"--file", file, "--working-dir", filepath.Join(dir, "work"), "--qemu-system", "/helpers/qemu", "--lvm", "/helpers/lvm", "router", "exec", "edge", "--user", "root", "--cwd", "/tmp", "--", "ip", "route", "show"}
	if code := cli.Execute(context.Background(), args); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if runtime.command.Name != "router-exec" || strings.Join(runtime.command.Args, "|") != "edge|--user|root|--cwd|/tmp|--|ip|route|show" {
		t.Fatalf("runtime command = %#v", runtime.command)
	}
	if runtime.command.Helpers.QEMUSystem != "/helpers/qemu" {
		t.Fatalf("helper override missing: %#v", runtime.command.Helpers)
	}
	if runtime.command.Helpers.LVM != "/helpers/lvm" {
		t.Fatalf("LVM override missing: %#v", runtime.command.Helpers)
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

func TestCLIStopHelpNeedsNoTopologyOrRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yml")
	for _, args := range [][]string{{"stop", "--help"}, {"stop", "--hard", "vm1", "-h"}} {
		var out, stderr bytes.Buffer
		cli := CLI{Stdout: &out, Stderr: &stderr, EUID: func() int { return 1000 }}
		if code := cli.Execute(context.Background(), append([]string{"--file", missing}, args...)); code != 0 {
			t.Fatalf("%q: exit %d, stderr=%s", args, code, stderr.String())
		}
		help := strings.Join(strings.Fields(out.String()), " ")
		for _, fragment := range []string{
			"stop [--hard] [NODE...]",
			"ACPI power-button event",
			"one after another",
			"--hard Simulate a power loss",
			"at the same moment",
			"TopoLab never sends SIGKILL",
			"within 30 seconds",
			"exits with status 1",
			"NAME, RESULT",
			"STOP_TIME",
			"EXIT_TIME",
			"does not make stop --hard wait",
			"With cache: unsafe, a hard stop keeps the disk images consistent",
			"host crash",
		} {
			if !strings.Contains(help, fragment) {
				t.Errorf("%q: stop help omits %q:\n%s", args, fragment, out.String())
			}
		}
	}
}

func TestCLIUsageShowsStopSyntax(t *testing.T) {
	var stderr bytes.Buffer
	if code := (CLI{Stdout: &bytes.Buffer{}, Stderr: &stderr}).Execute(context.Background(), nil); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "topolab stop [--hard] [NODE...]") {
		t.Fatalf("usage omits the stop syntax:\n%s", stderr.String())
	}
}

func TestCLIPassesStopHardArgumentsUnchanged(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(file, []byte("name: x\nrouters: [{name: r, interfaces: [{name: e, external: {sources: [10.0.0.0/24]}}]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &recordingRuntime{}
	cli := CLI{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, EUID: func() int { return 0 }, Runtime: runtime}
	if code := cli.Execute(context.Background(), []string{"--file", file, "--working-dir", filepath.Join(dir, "work"), "stop", "--hard", "vm1"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if runtime.command.Name != "stop" || strings.Join(runtime.command.Args, "|") != "--hard|vm1" {
		t.Fatalf("runtime command = %#v", runtime.command)
	}
}

func TestCLISnapshotHelpNeedsNoTopologyOrRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yml")
	for _, args := range [][]string{{"snapshot", "--help"}, {"snapshot", "-h"}, {"snapshot", "create", "base", "--help"}, {"snapshot", "restore", "-h"}} {
		var out, stderr bytes.Buffer
		cli := CLI{Stdout: &out, Stderr: &stderr, EUID: func() int { return 1000 }}
		if code := cli.Execute(context.Background(), append([]string{"--file", missing}, args...)); code != 0 {
			t.Fatalf("%q: exit %d, stderr=%s", args, code, stderr.String())
		}
		help := strings.Join(strings.Fields(out.String()), " ")
		for _, fragment := range []string{
			"snapshot create NAME [NODE...]",
			"snapshot restore NAME [NODE...]",
			"snapshot delete NAME [NODE...]",
			"snapshot list [--format table|json]",
			"does not save the RAM",
			"lvm-thick",
			"pause at the same moment",
			"crash-consistent",
			"stop --hard method",
			"stays stopped",
			"run the same command again",
			"NAME, BEFORE, RESULT and AFTER",
			"topolab down deletes all snapshots",
		} {
			if !strings.Contains(help, fragment) {
				t.Errorf("%q: snapshot help omits %q:\n%s", args, fragment, out.String())
			}
		}
	}
}

func TestCLISnapshotRequiresSubcommand(t *testing.T) {
	for _, args := range [][]string{{"snapshot"}, {"snapshot", "take", "base"}} {
		var stderr bytes.Buffer
		code := (CLI{Stdout: &bytes.Buffer{}, Stderr: &stderr}).Execute(context.Background(), args)
		if code != 2 || !strings.Contains(stderr.String(), "create, restore, delete, or list") {
			t.Errorf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestCLISnapshotSubcommandsBecomeRuntimeCommands(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "topology.yml")
	if err := os.WriteFile(file, []byte("name: x\nrouters: [{name: r, interfaces: [{name: e, external: {sources: [10.0.0.0/24]}}]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		name string
		rest string
	}{
		{[]string{"snapshot", "create", "base"}, "snapshot-create", "base"},
		{[]string{"snapshot", "restore", "base", "vm1", "vm2"}, "snapshot-restore", "base|vm1|vm2"},
		{[]string{"snapshot", "delete", "base", "vm1"}, "snapshot-delete", "base|vm1"},
		{[]string{"snapshot", "list", "--format", "json"}, "snapshot-list", "--format|json"},
	} {
		runtime := &recordingRuntime{}
		cli := CLI{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, EUID: func() int { return 0 }, Runtime: runtime}
		if code := cli.Execute(context.Background(), append([]string{"--file", file, "--working-dir", filepath.Join(dir, "work")}, test.args...)); code != 0 {
			t.Fatalf("%q: exit %d", test.args, code)
		}
		if runtime.command.Name != test.name || strings.Join(runtime.command.Args, "|") != test.rest {
			t.Errorf("%q: runtime command = %#v", test.args, runtime.command)
		}
	}
	var stderr bytes.Buffer
	code := (CLI{Stdout: &bytes.Buffer{}, Stderr: &stderr, EUID: func() int { return 1000 }}).Execute(context.Background(), []string{"--file", file, "--working-dir", filepath.Join(dir, "work"), "snapshot", "list"})
	if code != 1 || !strings.Contains(stderr.String(), "snapshot list requires effective UID 0") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestCLIUsageShowsSnapshotSyntax(t *testing.T) {
	var stderr bytes.Buffer
	if code := (CLI{Stdout: &bytes.Buffer{}, Stderr: &stderr}).Execute(context.Background(), nil); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "topolab snapshot create|restore|delete NAME [NODE...] | list") {
		t.Fatalf("usage omits the snapshot syntax:\n%s", stderr.String())
	}
}
