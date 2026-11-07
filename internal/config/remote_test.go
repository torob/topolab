package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const routerOnlyDocument = `name: lists
routers:
  - name: edge
    interfaces:
      - name: external
        external: {sources: [10.0.0.0/24]}
`

func TestRemoteSectionAcceptedAndMarshaled(t *testing.T) {
	doc := "remote: {ssh: \"operator@lab01.example\"}\n" + routerOnlyDocument
	cfg, _, err := Parse([]byte(doc), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote == nil || cfg.Remote.SSH != "operator@lab01.example" {
		t.Fatalf("Remote = %#v", cfg.Remote)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "remote:") || !strings.Contains(string(out), "operator@lab01.example") {
		t.Fatalf("normalized YAML lacks remote:\n%s", out)
	}
	again, _, err := Parse(out, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Remote == nil || again.Remote.SSH != cfg.Remote.SSH {
		t.Fatalf("round trip lost remote: %#v", again.Remote)
	}
}

func TestRemoteSectionIsOmittedWhenAbsent(t *testing.T) {
	cfg, _, err := Parse([]byte(routerOnlyDocument), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote != nil {
		t.Fatalf("Remote = %#v, want nil", cfg.Remote)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "remote") {
		t.Fatalf("normalized YAML mentions remote:\n%s", out)
	}
}

func TestRemoteSectionInterpolates(t *testing.T) {
	doc := "remote: {ssh: \"${LAB_REMOTE:-fallback@lab}\"}\n" + routerOnlyDocument
	cfg, _, err := Parse([]byte(doc), t.TempDir(), func(name string) (string, bool) {
		if name == "LAB_REMOTE" {
			return "ops@lab02", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote.SSH != "ops@lab02" {
		t.Fatalf("ssh = %q", cfg.Remote.SSH)
	}
}

func TestRemoteSectionRejections(t *testing.T) {
	tests := map[string]struct {
		remote string
		want   string
	}{
		"null":             {"remote:\n", "remote may not be empty"},
		"empty mapping":    {"remote: {}\n", "destination may not be empty"},
		"empty ssh":        {"remote: {ssh: \"\"}\n", "destination may not be empty"},
		"leading dash":     {"remote: {ssh: \"-oProxyCommand=x\"}\n", "may not begin with '-'"},
		"whitespace":       {"remote: {ssh: \"a b\"}\n", "whitespace or control"},
		"control":          {"remote: {ssh: \"a\\tb\"}\n", "whitespace or control"},
		"unknown field":    {"remote: {ssh: a, port: 22}\n", "field port not found"},
		"scalar not map":   {"remote: host\n", "decode strict YAML"},
		"missing required": {"remote: {ssh: \"${LAB_REMOTE:?remote required}\"}\n", "remote required"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := Parse([]byte(tt.remote+routerOnlyDocument), t.TempDir(), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLegacyHostKeyStaysRejectedBesideRemote(t *testing.T) {
	doc := "remote: {ssh: a@b}\nhost: {interfaces: []}\n" + routerOnlyDocument
	_, _, err := Parse([]byte(doc), t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), `legacy top-level key "host"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateDestination(t *testing.T) {
	for _, ok := range []string{"host", "user@host", "ssh://user@host:2222", "lab-alias", "u@192.0.2.1"} {
		if err := ValidateDestination(ok); err != nil {
			t.Errorf("ValidateDestination(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-host", "a b", "a\nb", "a\x00b", "\tx"} {
		if err := ValidateDestination(bad); err == nil {
			t.Errorf("ValidateDestination(%q) succeeded", bad)
		}
	}
}

// memoryFS serves files from memory so parsing never touches the host.
type memoryFS struct {
	files map[string][]byte
	stats []string
}

type memoryInfo struct {
	name string
	size int64
}

func (i memoryInfo) Name() string       { return i.name }
func (i memoryInfo) Size() int64        { return i.size }
func (i memoryInfo) Mode() fs.FileMode  { return 0o444 }
func (i memoryInfo) ModTime() time.Time { return time.Time{} }
func (i memoryInfo) IsDir() bool        { return false }
func (i memoryInfo) Sys() any           { return nil }

func (m *memoryFS) Stat(path string) (fs.FileInfo, error) {
	m.stats = append(m.stats, path)
	data, ok := m.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return memoryInfo{name: filepath.Base(path), size: int64(len(data))}, nil
}

func (m *memoryFS) ReadFile(path string) ([]byte, error) {
	data, ok := m.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return data, nil
}

func TestParseWithServesFilesFromInjectedFileSystem(t *testing.T) {
	doc, _ := validVMDocument(t, "")
	key := testKey(t)
	const remoteTemplate = "/controller/templates/base.qcow2"
	memory := &memoryFS{files: map[string][]byte{remoteTemplate: []byte("qcow2")}}
	cfg, _, err := ParseWith(doc, ParseOptions{
		BaseDir: "/controller",
		FS:      memory,
		Lookup: func(name string) (string, bool) {
			switch name {
			case "IMAGE":
				return remoteTemplate, true
			case "KEY":
				return key, true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Templates[0].Path != remoteTemplate {
		t.Fatalf("template path = %q", cfg.Templates[0].Path)
	}
	if len(memory.stats) == 0 {
		t.Fatal("template was not checked through the injected file system")
	}
	if _, err := os.Stat(remoteTemplate); !errors.Is(err, os.ErrNotExist) {
		t.Skipf("host unexpectedly has %s", remoteTemplate)
	}
}

func TestParseWithRejectsMissingTemplateInInjectedFileSystem(t *testing.T) {
	doc, _ := validVMDocument(t, "")
	_, _, err := ParseWith(doc, ParseOptions{
		BaseDir: "/controller",
		FS:      &memoryFS{},
		Lookup: func(name string) (string, bool) {
			if name == "IMAGE" {
				return "/controller/missing.qcow2", true
			}
			return testKey(t), true
		},
	})
	if err == nil || !strings.Contains(err.Error(), "missing.qcow2") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadTopologyChecksNoWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "base.qcow2")
	header := make([]byte, 24)
	copy(header, []byte{'Q', 'F', 'I', 0xfb, 0, 0, 0, 3})
	if err := os.WriteFile(template, header, 0o600); err != nil {
		t.Fatal(err)
	}
	topology := filepath.Join(dir, "topology.yml")
	doc := "name: " + strings.Repeat("n", 60) + "\ntemplates:\n  - {name: base, path: " + template + "}\n" + routerOnlyDocument[len("name: lists\n"):]
	if err := os.WriteFile(topology, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := LoadTopology(LoadOptions{File: topology, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkingDir != "" {
		t.Fatalf("WorkingDir = %q, want empty", result.WorkingDir)
	}
	if _, err := Load(LoadOptions{File: topology, WorkingDir: "", LookupEnv: func(string) (string, bool) { return "", false }}); err == nil || !strings.Contains(err.Error(), "working directory is required") {
		t.Fatalf("Load without a working directory: %v", err)
	}
}
