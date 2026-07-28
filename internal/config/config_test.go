package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

func testKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func validVMDocument(t *testing.T, extra string) ([]byte, string) {
	t.Helper()
	dir := t.TempDir()
	template := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(template, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	doc := `name: lab
templates:
  base: "${IMAGE:?image required}"
vms:
  vm:
    template: base
    ssh_authorized_keys:
      - "${KEY:?key required}"
    interfaces:
      eth0: {}
switches:
  lan: {}
links:
  vm-lan:
    endpoints:
      - {node: vm, interface: eth0}
      - {node: lan, interface: vm}
` + extra
	return []byte(doc), template
}

func TestParseDefaultsInterpolationAndMarshal(t *testing.T) {
	doc, template := validVMDocument(t, "")
	key := testKey(t)
	cfg, warnings, err := Parse(doc, t.TempDir(), func(name string) (string, bool) {
		switch name {
		case "IMAGE":
			return template, true
		case "KEY":
			return key, true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	vm := cfg.VMs["vm"]
	if vm.MemoryMB != 2048 || vm.VCPUs != 2 || vm.Interfaces["eth0"].MAC == "" || vm.Interfaces["eth0"].MTU != 1500 {
		t.Fatalf("defaults not applied: %#v", vm)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{"management_host:", "interfaces: {}", "memory_mb: 2048", "model: virtio", "queues: 1", "bus: scsi", "mtu: 1500"} {
		if !strings.Contains(text, want) {
			t.Errorf("normalized YAML lacks %q:\n%s", want, text)
		}
	}
}

func TestAdditionalDiskAndInterfaceOrderPreserved(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(template, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	doc := `name: order
templates: {base: ` + template + `}
vms:
  vm:
    template: base
    ssh_authorized_keys: ["` + testKey(t) + `"]
    additional_disks:
      zed: {size_gib: 1}
      alpha: {size_gib: 2}
    interfaces:
      zif: {}
      aif: {}
switches: {lan: {}}
links:
  z: {endpoints: [{node: vm, interface: zif}, {node: lan, interface: z}]}
  a: {endpoints: [{node: vm, interface: aif}, {node: lan, interface: a}]}
`
	cfg, _, err := Parse([]byte(doc), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Index(s, "zed:") > strings.Index(s, "alpha:") {
		t.Fatalf("disk order changed:\n%s", s)
	}
	if strings.Index(s, "zif:") > strings.Index(s, "aif:") {
		t.Fatalf("interface order changed:\n%s", s)
	}
}

func TestStrictDocumentAndInterpolationRules(t *testing.T) {
	doc, template := validVMDocument(t, "unknown: true\n")
	_, _, err := Parse(doc, t.TempDir(), func(name string) (string, bool) {
		if name == "IMAGE" {
			return template, true
		}
		if name == "KEY" {
			return testKey(t), true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "field unknown not found") {
		t.Fatalf("expected unknown field error, got %v", err)
	}

	doc, template = validVMDocument(t, "---\nname: other\n")
	_, _, err = Parse(doc, t.TempDir(), func(name string) (string, bool) {
		if name == "IMAGE" {
			return template, true
		}
		if name == "KEY" {
			return testKey(t), true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "additional YAML") {
		t.Fatalf("expected extra document error, got %v", err)
	}

	got, warnings, err := interpolateString(`${EMPTY:-${FALLBACK-default}}/$UNSET/$$HOME`, lookupFunc(func(name string) (string, bool) {
		if name == "EMPTY" {
			return "", true
		}
		return "", false
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got != "default//$HOME" || len(warnings) != 1 {
		t.Fatalf("got %q warnings=%v", got, warnings)
	}
}

func TestCrossFieldViolations(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]string{
		"management host reserved": `name: x
vms: {management_host: {}}
links: {}`,
		"trunk filtering": `name: x
routers:
  r: {interfaces: {lan: {}}}
switches: {s: {}}
links:
  l:
    endpoints:
      - {node: r, interface: lan}
      - {node: s, interface: r, port: {mode: trunk, allowed_vlans: [2]}}`,
	}
	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Parse([]byte(doc), dir, nil); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRouterSchemaIsNamespaceOnly(t *testing.T) {
	valid := `name: x
routers:
  r:
    interfaces:
      wan:
        external: {sources: [10.0.0.0/24]}
`
	cfg, _, err := Parse([]byte(valid), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if strings.Contains(text, "backend:") || !strings.Contains(text, "routers:") {
		t.Fatalf("unexpected normalized router YAML:\n%s", text)
	}

	for name, field := range map[string]string{
		"backend":            "backend: namespace",
		"template":           "template: base",
		"vcpus":              "vcpus: 1",
		"memory":             "memory_mb: 1024",
		"authorized keys":    "ssh_authorized_keys: []",
		"router DHCP client": "interfaces: {lan: {ipv4: {dhcp: true}}}",
	} {
		t.Run(name, func(t *testing.T) {
			doc := "name: x\nrouters:\n  r:\n    " + field + "\n"
			if name != "router DHCP client" {
				doc += "    interfaces: {wan: {external: {sources: [10.0.0.0/24]}}}\n"
			} else {
				doc += "switches: {s: {}}\nlinks: {l: {endpoints: [{node: r, interface: lan}, {node: s, interface: r}]}}\n"
			}
			_, _, err := Parse([]byte(doc), t.TempDir(), nil)
			if err == nil || !strings.Contains(err.Error(), "field ") || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("expected strict unknown-field error, got %v", err)
			}
		})
	}
}

func TestLegacyHostKeyHasFocusedMigrationDiagnostic(t *testing.T) {
	_, _, err := Parse([]byte("name: x\nhost: {}\nlinks: {}\n"), t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "rename it to \"management_host\"") {
		t.Fatalf("expected focused migration diagnostic, got %v", err)
	}
}

func TestInterpolationVisitsAnchoredScalarOnce(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("values: [ &value \"$$HOME\", *value, *value ]\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := interpolateDocument(&doc, lookupFunc(func(string) (string, bool) { return "", false })); err != nil {
		t.Fatal(err)
	}
	sequence := mappingValue(documentMapping(&doc), "values")
	for i, node := range sequence.Content {
		if node.Kind == yaml.AliasNode {
			node = node.Alias
		}
		if node.Value != "$HOME" {
			t.Fatalf("alias %d was interpolated repeatedly: %q", i, node.Value)
		}
	}
}

func TestNonFiniteImpairmentRejected(t *testing.T) {
	doc, template := validVMDocument(t, "")
	doc = []byte(strings.Replace(string(doc), "- {node: vm, interface: eth0}", "- {node: vm, interface: eth0, egress: {loss_percent: .nan}}", 1))
	_, _, err := Parse(doc, t.TempDir(), func(name string) (string, bool) {
		switch name {
		case "IMAGE":
			return template, true
		case "KEY":
			return testKey(t), true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), "invalid egress impairment") {
		t.Fatalf("unexpected error: %v", err)
	}
}
