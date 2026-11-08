package config

import (
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingFS fails every call, so a test shows that parsing never touches the
// files of a template with remote: true.
type failingFS struct{ t *testing.T }

func (f failingFS) Stat(path string) (fs.FileInfo, error) {
	f.t.Errorf("Stat(%q): files of remote templates are not read while parsing", path)
	return nil, fs.ErrNotExist
}

func (f failingFS) ReadFile(path string) ([]byte, error) {
	f.t.Errorf("ReadFile(%q)", path)
	return nil, fs.ErrNotExist
}

func qcow2File(t *testing.T, dir, name string, backing uint64) string {
	t.Helper()
	header := make([]byte, 64)
	copy(header, "QFI\xfb")
	binary.BigEndian.PutUint32(header[4:8], 3)
	binary.BigEndian.PutUint64(header[8:16], backing)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, header, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func templateDocument(entry string) string {
	return "templates:\n  - " + entry + "\n" + routerOnlyDocument
}

func TestRemoteTemplateIsAcceptedAndNormalized(t *testing.T) {
	doc := templateDocument(`{name: base, path: /srv//images/../images/base.qcow2, remote: true}`)
	cfg, _, err := ParseWith([]byte(doc), ParseOptions{BaseDir: t.TempDir(), FS: failingFS{t}})
	if err != nil {
		t.Fatal(err)
	}
	tmpl := cfg.Templates[0]
	if !tmpl.Remote || tmpl.Path != "/srv/images/base.qcow2" {
		t.Fatalf("template = %+v", tmpl)
	}
	if len(cfg.HostTemplates()) != 1 || len(cfg.ControllerTemplates()) != 0 {
		t.Fatalf("host = %d, controller = %d", len(cfg.HostTemplates()), len(cfg.ControllerTemplates()))
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "path: /srv/images/base.qcow2") || !strings.Contains(string(out), "remote: true") {
		t.Fatalf("normalized YAML:\n%s", out)
	}
	again, _, err := ParseWith(out, ParseOptions{BaseDir: t.TempDir(), FS: failingFS{t}})
	if err != nil || !again.Templates[0].Remote || again.Templates[0].Path != tmpl.Path {
		t.Fatalf("round trip: %+v, %v", again, err)
	}
}

func TestLocalTemplateNormalizesAsBeforeWithOrWithoutRemoteFalse(t *testing.T) {
	dir := t.TempDir()
	file := qcow2File(t, dir, "base.qcow2", 0)
	var outputs []string
	for _, entry := range []string{`{name: base, path: ./base.qcow2}`, `{name: base, path: ./base.qcow2, remote: false}`} {
		cfg, _, err := Parse([]byte(templateDocument(entry)), dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := cfg.Templates[0]
		if tmpl.Remote || tmpl.Path != file {
			t.Fatalf("template = %+v, want path %s", tmpl, file)
		}
		if len(cfg.HostTemplates()) != 0 || len(cfg.ControllerTemplates()) != 1 {
			t.Fatalf("host = %d, controller = %d", len(cfg.HostTemplates()), len(cfg.ControllerTemplates()))
		}
		out, _ := Marshal(cfg)
		if strings.Contains(string(out), "remote") || !strings.Contains(string(out), "path: "+file) {
			t.Fatalf("normalized YAML:\n%s", out)
		}
		outputs = append(outputs, string(out))
	}
	if outputs[0] != outputs[1] {
		t.Fatalf("an explicit remote: false changes the output:\n%s\n%s", outputs[0], outputs[1])
	}
}

func TestTemplatePathAndRemoteAreValidated(t *testing.T) {
	tests := map[string]struct{ entry, want string }{
		"relative remote path": {`{name: base, path: images/a.qcow2, remote: true}`, "must be absolute"},
		"dot remote path":      {`{name: base, path: ./a.qcow2, remote: true}`, "must be absolute"},
		"empty path":           {`{name: base, path: ""}`, "requires a path"},
		"no path":              {`{name: base}`, "requires a path"},
		"remote not a boolean": {`{name: base, path: /a.qcow2, remote: maybe}`, "into bool"},
		"remote empty":         {`{name: base, path: /a.qcow2, remote: }`, "remote may not be empty"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseWith([]byte(templateDocument(tt.entry)), ParseOptions{BaseDir: t.TempDir(), FS: failingFS{t}})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	// The old key is gone; strict parsing rejects it.
	_, _, err := ParseWith([]byte(templateDocument(`{name: base, host_path: /a.qcow2}`)), ParseOptions{BaseDir: t.TempDir(), FS: failingFS{t}})
	if err == nil || !strings.Contains(err.Error(), "host_path") {
		t.Fatalf("the removed host_path key was accepted: %v", err)
	}
}

func TestLoadTopologyLeavesRemoteTemplateFilesAloneButLoadChecksThem(t *testing.T) {
	dir := t.TempDir()
	topology := filepath.Join(dir, "topology.yml")
	missing := filepath.Join(dir, "absent.qcow2")
	write := func(path string) {
		t.Helper()
		doc := templateDocument("{name: base, path: " + path + ", remote: true}")
		if err := os.WriteFile(topology, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(missing)
	// A controller cannot see the host, so it accepts the topology.
	if _, err := LoadTopology(LoadOptions{File: topology}); err != nil {
		t.Fatalf("LoadTopology: %v", err)
	}
	// Local mode runs on the host and checks the file like any other template.
	if _, err := Load(LoadOptions{File: topology, WorkingDir: filepath.Join(dir, "wd")}); err == nil || !strings.Contains(err.Error(), `template "base"`) {
		t.Fatalf("Load of a missing remote template file: %v", err)
	}
	write(qcow2File(t, dir, "backed.qcow2", 0x20))
	if _, err := Load(LoadOptions{File: topology, WorkingDir: filepath.Join(dir, "wd")}); err == nil || !strings.Contains(err.Error(), "backing file") {
		t.Fatalf("Load of a backed remote template file: %v", err)
	}
	write(qcow2File(t, dir, "good.qcow2", 0))
	if _, err := Load(LoadOptions{File: topology, WorkingDir: filepath.Join(dir, "wd")}); err != nil {
		t.Fatalf("Load of a good remote template file: %v", err)
	}
}

func TestLoadTopologyStillChecksControllerTemplates(t *testing.T) {
	dir := t.TempDir()
	topology := filepath.Join(dir, "topology.yml")
	for _, entry := range []string{`{name: base, path: ./absent.qcow2}`, `{name: base, path: ./absent.qcow2, remote: false}`} {
		if err := os.WriteFile(topology, []byte(templateDocument(entry)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTopology(LoadOptions{File: topology}); err == nil {
			t.Fatalf("a missing controller template was accepted: %s", entry)
		}
	}
}
