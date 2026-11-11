package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

func TestIndexesMatchSlicesAndLookupFallback(t *testing.T) {
	template := &Template{Name: "base", Path: "/base.qcow2"}
	host := &ManagementHostInterface{Name: "host0"}
	disk := &Disk{Name: "data"}
	vmVLAN := &VMVLAN{ID: 10}
	vmIface := &VMInterface{Name: "eth0", VLANs: []*VMVLAN{vmVLAN}}
	vm := &VM{Name: "vm", AdditionalDisks: []*Disk{disk}, Interfaces: []*VMInterface{vmIface}}
	routerVLAN := &RouterVLAN{ID: 20}
	routerIface := &RouterInterface{Name: "lan", VLANs: []*RouterVLAN{routerVLAN}}
	router := &Router{Name: "router", Interfaces: []*RouterInterface{routerIface}}
	switchRecord := &Switch{Name: "switch"}
	link := &Link{Name: "link"}
	cfg := &Config{
		Name: "lab", Templates: []*Template{template}, ManagementHost: ManagementHost{Interfaces: []*ManagementHostInterface{host}},
		VMs: []*VM{vm}, Routers: []*Router{router}, Switches: []*Switch{switchRecord}, Links: []*Link{link},
	}
	if err := buildIndexes(cfg); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"template", cfg.index.templates["base"], template},
		{"management interface", cfg.index.managementHostInterfaces["host0"], host},
		{"VM", cfg.index.vms["vm"], vm},
		{"VM disk", cfg.index.vmDisks["vm"]["data"], disk},
		{"VM interface", cfg.index.vmInterfaces["vm"]["eth0"], vmIface},
		{"VM VLAN", cfg.index.vmVLANs["vm"]["eth0"][10], vmVLAN},
		{"router", cfg.index.routers["router"], router},
		{"router interface", cfg.index.routerInterfaces["router"]["lan"], routerIface},
		{"router VLAN", cfg.index.routerVLANs["router"]["lan"][20], routerVLAN},
		{"switch", cfg.index.switches["switch"], switchRecord},
		{"link", cfg.index.links["link"], link},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s index = %p, want %p", check.name, check.got, check.want)
		}
	}
	if got, ok := cfg.LookupVMVLAN("vm", "eth0", 10); !ok || got != vmVLAN {
		t.Fatalf("indexed VM VLAN lookup = %#v, %t", got, ok)
	}
	if got, ok := cfg.LookupRouterVLAN("router", "lan", 20); !ok || got != routerVLAN {
		t.Fatalf("indexed router VLAN lookup = %#v, %t", got, ok)
	}
	indexedJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	indexedYAML, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.index = nil
	if got, ok := cfg.LookupTemplate("base"); !ok || got != template {
		t.Fatalf("fallback template lookup = %#v, %t", got, ok)
	}
	if got, ok := cfg.LookupVMAdditionalDisk("vm", "data"); !ok || got != disk {
		t.Fatalf("fallback disk lookup = %#v, %t", got, ok)
	}
	if got, ok := cfg.LookupVMVLAN("vm", "eth0", 10); !ok || got != vmVLAN {
		t.Fatalf("fallback VM VLAN lookup = %#v, %t", got, ok)
	}
	if got, ok := cfg.LookupRouterVLAN("router", "lan", 20); !ok || got != routerVLAN {
		t.Fatalf("fallback router VLAN lookup = %#v, %t", got, ok)
	}
	fallbackJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(indexedJSON) != string(fallbackJSON) {
		t.Fatalf("private index changed JSON/hash input:\nindexed=%s\nfallback=%s", indexedJSON, fallbackJSON)
	}
	fallbackYAML, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(indexedYAML) != string(fallbackYAML) {
		t.Fatalf("private index changed YAML:\nindexed=%s\nfallback=%s", indexedYAML, fallbackYAML)
	}
}

func TestMarshalPreservesEveryNamedCollectionOrder(t *testing.T) {
	cfg := &Config{
		Name:      "ordered",
		Templates: []*Template{{Name: "template-z"}, {Name: "template-a"}},
		ManagementHost: ManagementHost{Interfaces: []*ManagementHostInterface{
			{Name: "host-z"}, {Name: "host-a"},
		}},
		VMs: []*VM{
			{Name: "vm-z", AdditionalDisks: []*Disk{{Name: "disk-z"}, {Name: "disk-a"}}, Interfaces: []*VMInterface{
				{Name: "iface-z", VLANs: []*VMVLAN{{ID: 20}, {ID: 10}}}, {Name: "iface-a"},
			}},
			{Name: "vm-a"},
		},
		Routers: []*Router{
			{Name: "router-z", Interfaces: []*RouterInterface{
				{Name: "iface-z", VLANs: []*RouterVLAN{{ID: 20}, {ID: 10}}}, {Name: "iface-a"},
			}},
			{Name: "router-a"},
		},
		Switches: []*Switch{{Name: "switch-z"}, {Name: "switch-a"}},
		Links:    []*Link{{Name: "link-z"}, {Name: "link-a"}},
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := yaml.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Templates[0].Name != "template-z" || decoded.ManagementHost.Interfaces[0].Name != "host-z" ||
		decoded.VMs[0].Name != "vm-z" || decoded.VMs[0].AdditionalDisks[0].Name != "disk-z" ||
		decoded.VMs[0].Interfaces[0].Name != "iface-z" || decoded.VMs[0].Interfaces[0].VLANs[0].ID != 20 ||
		decoded.Routers[0].Name != "router-z" || decoded.Routers[0].Interfaces[0].Name != "iface-z" ||
		decoded.Routers[0].Interfaces[0].VLANs[0].ID != 20 || decoded.Switches[0].Name != "switch-z" || decoded.Links[0].Name != "link-z" {
		t.Fatalf("named collection order changed after marshal:\n%s", out)
	}
}

func TestIndexConstructionRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"null top-level", &Config{Switches: []*Switch{nil}}, "switch entry 0 is null"},
		{"missing top-level name", &Config{Switches: []*Switch{{}}}, "switch entry 0 is missing name"},
		{"duplicate top-level name", &Config{Switches: []*Switch{{Name: "s"}, {Name: "s"}}}, `duplicate switch name "s"`},
		{"null nested", &Config{VMs: []*VM{{Name: "vm", Interfaces: []*VMInterface{nil}}}}, `VM "vm" interface entry 0 is null`},
		{"missing nested name", &Config{VMs: []*VM{{Name: "vm", AdditionalDisks: []*Disk{{}}}}}, `VM "vm" disk entry 0 is missing name`},
		{"duplicate nested name", &Config{Routers: []*Router{{Name: "r", Interfaces: []*RouterInterface{{Name: "lan"}, {Name: "lan"}}}}}, `duplicate router "r" interface name "lan"`},
		{"null VLAN", &Config{VMs: []*VM{{Name: "vm", Interfaces: []*VMInterface{{Name: "eth0", VLANs: []*VMVLAN{nil}}}}}}, `VLAN entry 0 is null`},
		{"missing VLAN ID", &Config{VMs: []*VM{{Name: "vm", Interfaces: []*VMInterface{{Name: "eth0", VLANs: []*VMVLAN{{}}}}}}}, `VLAN entry 0 is missing id`},
		{"duplicate VLAN ID", &Config{Routers: []*Router{{Name: "r", Interfaces: []*RouterInterface{{Name: "lan", VLANs: []*RouterVLAN{{ID: 10}, {ID: 10}}}}}}}, `duplicates VLAN id 10`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := buildIndexes(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestFormerNamedMapSyntaxIsRejected(t *testing.T) {
	docs := []string{
		"name: x\ntemplates: {base: /tmp/base}\n",
		"name: x\nmanagement_host: {interfaces: {host0: {}}}\n",
		"name: x\nvms: {vm: {}}\n",
		"name: x\nrouters: {r: {}}\n",
		"name: x\nswitches: {s: {}}\n",
		"name: x\nlinks: {l: {}}\n",
		"name: x\nvms: [{name: vm, additional_disks: {data: {}}}]\n",
		"name: x\nvms: [{name: vm, interfaces: {eth0: {}}}]\n",
		"name: x\nvms: [{name: vm, interfaces: [{name: eth0, vlans: {10: {}}}]}]\n",
		"name: x\nrouters: [{name: r, interfaces: {lan: {}}}]\n",
		"name: x\nrouters: [{name: r, interfaces: [{name: lan, vlans: {10: {}}}]}]\n",
	}
	for i, doc := range docs {
		if _, _, err := Parse([]byte(doc), t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "cannot unmarshal !!map") {
			t.Errorf("case %d: old map syntax error = %v", i, err)
		}
	}
}

func TestParseRejectsInvalidListIdentities(t *testing.T) {
	tests := []struct {
		doc  string
		want string
	}{
		{"name: x\nswitches: [null]\n", "switch entry 0 is null"},
		{"name: x\nswitches: [{}]\n", "switch entry 0 is missing name"},
		{"name: x\nswitches: [{name: s}, {name: s}]\n", `duplicate switch name "s"`},
		{"name: x\nvms: [{name: vm, interfaces: [null]}]\n", `VM "vm" interface entry 0 is null`},
		{"name: x\nvms: [{name: vm, additional_disks: [{}]}]\n", `VM "vm" disk entry 0 is missing name`},
		{"name: x\nrouters: [{name: r, interfaces: [{name: lan}, {name: lan}]}]\n", `duplicate router "r" interface name "lan"`},
		{"name: x\nvms: [{name: vm, interfaces: [{name: eth0, vlans: [null]}]}]\n", "VLAN entry 0 is null"},
		{"name: x\nvms: [{name: vm, interfaces: [{name: eth0, vlans: [{}]}]}]\n", "VLAN entry 0 is missing id"},
		{"name: x\nrouters: [{name: r, interfaces: [{name: lan, vlans: [{id: 10}, {id: 10}]}]}]\n", "duplicates VLAN id 10"},
	}
	for _, test := range tests {
		if _, _, err := Parse([]byte(test.doc), t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("document %q: error = %v, want substring %q", test.doc, err, test.want)
		}
	}
}

func TestAbsentCollectionsNormalizeToListsAndRoundTrip(t *testing.T) {
	doc := []byte(`name: lists
routers:
  - name: edge
    interfaces:
      - name: external
        external: {sources: [10.0.0.0/24]}
`)
	cfg, _, err := Parse(doc, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Templates == nil || cfg.ManagementHost.Interfaces == nil || cfg.VMs == nil || cfg.Switches == nil || cfg.Links == nil || cfg.Routers[0].Interfaces[0].VLANs == nil {
		t.Fatalf("absent collections were not normalized to empty slices: %#v", cfg)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"templates: []", "interfaces: []", "vms: []", "switches: []", "links: []", "vlans: []"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("normalized YAML lacks %q:\n%s", want, out)
		}
	}
	roundTrip, _, err := Parse(out, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(cfg)
	second, _ := json.Marshal(roundTrip)
	if string(first) != string(second) {
		t.Fatalf("round trip changed configuration:\nfirst=%s\nsecond=%s", first, second)
	}
}

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
  - name: base
    path: "${IMAGE:?image required}"
storages:
  - {name: local, type: file, root_provisioning: snapshot}
vms:
  - name: vm
    template: base
    root_disk: {storage: local}
    ssh_authorized_keys:
      - "${KEY:?key required}"
    interfaces:
      - name: eth0
switches:
  - name: lan
links:
  - name: vm-lan
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
	vm, _ := cfg.LookupVM("vm")
	iface, _ := cfg.LookupVMInterface("vm", "eth0")
	if vm.MemoryMB != 2048 || vm.VCPUs != 2 || iface.MAC == "" || iface.MTU != 1500 {
		t.Fatalf("defaults not applied: %#v", vm)
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{"management_host:", "interfaces: []", "root_ca_certificates: []", "memory_mb: 2048", "model: virtio", "queues: 1", "bus: scsi", "cache: none", "aio: auto", "mtu: 1500"} {
		if !strings.Contains(text, want) {
			t.Errorf("normalized YAML lacks %q:\n%s", want, text)
		}
	}
}

func TestDiskIOPolicyNormalizationValidationAndRoundTrip(t *testing.T) {
	doc, template := validVMDocument(t, "")
	lookup := func(name string) (string, bool) {
		switch name {
		case "IMAGE":
			return template, true
		case "KEY":
			return testKey(t), true
		default:
			return "", false
		}
	}
	withDisk := strings.Replace(string(doc), "    interfaces:\n", "    additional_disks:\n      - {name: data, size_gib: 1, storage: local}\n    interfaces:\n", 1)
	cfg, _, err := Parse([]byte(withDisk), t.TempDir(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	vm, _ := cfg.LookupVM("vm")
	if vm.RootDisk.Cache != "none" || vm.AdditionalDisks[0].Cache != "none" || vm.RootDisk.AIO != "auto" || vm.AdditionalDisks[0].AIO != "auto" {
		t.Fatalf("disk I/O defaults = root %#v additional %#v", vm.RootDisk, vm.AdditionalDisks[0])
	}
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(out), "cache: none") != 2 {
		t.Fatalf("normalized YAML omitted cache defaults:\n%s", out)
	}
	if strings.Count(string(out), "aio: auto") != 2 {
		t.Fatalf("normalized YAML omitted AIO defaults:\n%s", out)
	}
	roundTrip, _, err := Parse(out, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	roundTripVM, _ := roundTrip.LookupVM("vm")
	if roundTripVM.RootDisk.Cache != "none" || roundTripVM.AdditionalDisks[0].Cache != "none" || roundTripVM.RootDisk.AIO != "auto" || roundTripVM.AdditionalDisks[0].AIO != "auto" {
		t.Fatalf("round trip changed disk I/O policies: %#v", roundTripVM)
	}

	for _, cache := range []string{"none", "writeback", "unsafe"} {
		t.Run("cache "+cache, func(t *testing.T) {
			changed := strings.Replace(withDisk, "root_disk: {storage: local}", "root_disk: {storage: local, cache: "+cache+"}", 1)
			changed = strings.Replace(changed, "size_gib: 1, storage: local}", "size_gib: 1, storage: local, cache: "+cache+"}", 1)
			parsed, _, err := Parse([]byte(changed), t.TempDir(), lookup)
			if err != nil {
				t.Fatal(err)
			}
			parsedVM, _ := parsed.LookupVM("vm")
			if parsedVM.RootDisk.Cache != cache || parsedVM.AdditionalDisks[0].Cache != cache {
				t.Fatalf("explicit cache policies not preserved: %#v", parsedVM)
			}
			encoded, err := Marshal(parsed)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(encoded), "cache: "+cache) != 2 {
				t.Fatalf("normalized YAML omitted cache policy:\n%s", encoded)
			}
			roundTrip, _, err := Parse(encoded, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			roundTripVM, _ := roundTrip.LookupVM("vm")
			if roundTripVM.RootDisk.Cache != cache || roundTripVM.AdditionalDisks[0].Cache != cache {
				t.Fatalf("round trip changed explicit cache policies: %#v", roundTripVM)
			}
		})
	}

	for _, aio := range []string{"auto", "io_uring", "threads", "native"} {
		t.Run("aio "+aio, func(t *testing.T) {
			changed := strings.Replace(withDisk, "root_disk: {storage: local}", "root_disk: {storage: local, aio: "+aio+"}", 1)
			changed = strings.Replace(changed, "size_gib: 1, storage: local}", "size_gib: 1, storage: local, aio: "+aio+"}", 1)
			parsed, _, err := Parse([]byte(changed), t.TempDir(), lookup)
			if err != nil {
				t.Fatal(err)
			}
			parsedVM, _ := parsed.LookupVM("vm")
			if parsedVM.RootDisk.AIO != aio || parsedVM.AdditionalDisks[0].AIO != aio {
				t.Fatalf("AIO policies = %#v", parsedVM)
			}
			encoded, err := Marshal(parsed)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(encoded), "aio: "+aio) != 2 {
				t.Fatalf("normalized YAML omitted AIO policy:\n%s", encoded)
			}
			roundTrip, _, err := Parse(encoded, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			roundTripVM, _ := roundTrip.LookupVM("vm")
			if roundTripVM.RootDisk.AIO != aio || roundTripVM.AdditionalDisks[0].AIO != aio {
				t.Fatalf("round trip changed AIO policies: %#v", roundTripVM)
			}
		})
	}

	for _, test := range []struct {
		name, old, replacement, want string
	}{
		{"root unknown", "root_disk: {storage: local}", "root_disk: {storage: local, cache: directsync}", `root_disk: cache must be none, writeback, or unsafe, got "directsync"`},
		{"additional unknown", "size_gib: 1, storage: local}", "size_gib: 1, storage: local, cache: writethrough}", `disk "data": cache must be none, writeback, or unsafe, got "writethrough"`},
		{"uppercase unsafe", "root_disk: {storage: local}", "root_disk: {storage: local, cache: UNSAFE}", `got "UNSAFE"`},
		{"root empty string", "root_disk: {storage: local}", "root_disk: {storage: local, cache: ''}", "cache may not be empty"},
		{"additional null value", "size_gib: 1, storage: local}", "size_gib: 1, storage: local, cache: null}", "cache may not be empty"},
		{"root unknown aio", "root_disk: {storage: local}", "root_disk: {storage: local, aio: posix}", `aio must be auto, io_uring, threads, or native, got "posix"`},
		{"naitive typo", "root_disk: {storage: local}", "root_disk: {storage: local, aio: naitive}", `got "naitive"`},
		{"additional empty aio", "size_gib: 1, storage: local}", "size_gib: 1, storage: local, aio: ''}", "aio may not be empty"},
		{"root null aio", "root_disk: {storage: local}", "root_disk: {storage: local, aio: null}", "aio may not be empty"},
		{"native writeback", "root_disk: {storage: local}", "root_disk: {storage: local, cache: writeback, aio: native}", "aio native requires cache none"},
		{"root native unsafe", "root_disk: {storage: local}", "root_disk: {storage: local, cache: unsafe, aio: native}", "root_disk: aio native requires cache none"},
		{"additional native unsafe", "size_gib: 1, storage: local}", "size_gib: 1, storage: local, cache: unsafe, aio: native}", `disk "data": aio native requires cache none`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := strings.Replace(withDisk, test.old, test.replacement, 1)
			_, _, err := Parse([]byte(changed), t.TempDir(), lookup)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAdditionalDiskAndInterfaceOrderPreserved(t *testing.T) {
	dir := t.TempDir()
	template := filepath.Join(dir, "base.qcow2")
	if err := os.WriteFile(template, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	doc := `name: order
templates: [{name: base, path: ` + template + `}]
storages: [{name: local, type: file, root_provisioning: snapshot}]
vms:
  - name: vm
    template: base
    root_disk: {storage: local}
    ssh_authorized_keys: ["` + testKey(t) + `"]
    additional_disks:
      - {name: zed, size_gib: 1, storage: local}
      - {name: alpha, size_gib: 2, storage: local}
    interfaces:
      - {name: zif}
      - {name: aif}
switches: [{name: lan}]
links:
  - {name: z, endpoints: [{node: vm, interface: zif}, {node: lan, interface: z}]}
  - {name: a, endpoints: [{node: vm, interface: aif}, {node: lan, interface: a}]}
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
	if strings.Index(s, "name: zed") > strings.Index(s, "name: alpha") {
		t.Fatalf("disk order changed:\n%s", s)
	}
	if strings.Index(s, "name: zif") > strings.Index(s, "name: aif") {
		t.Fatalf("interface order changed:\n%s", s)
	}
}

func TestStorageTypeSpecificFieldPresenceValidation(t *testing.T) {
	base := func(storage string) string {
		return "name: storage-fields\nstorages:\n  - " + storage + "\nrouters:\n  - name: edge\n    interfaces:\n      - name: wan\n        external: {sources: [10.0.0.0/24]}\n"
	}
	tests := []struct{ name, storage, want string }{
		{"file valid snapshot", "{name: files, type: file, root_provisioning: snapshot}", ""},
		{"file valid copy", "{name: files, type: file, root_provisioning: copy}", ""},
		{"file missing provisioning", "{name: files, type: file}", `field "root_provisioning" is required for type "file"`},
		{"file empty provisioning", "{name: files, type: file, root_provisioning: ''}", `field "root_provisioning" is required for type "file"`},
		{"file empty VG forbidden", "{name: files, type: file, root_provisioning: copy, volume_group: ''}", `field "volume_group" is forbidden for type "file"`},
		{"file zero pool forbidden", "{name: files, type: file, root_provisioning: copy, pool_size_gib: 0}", `field "pool_size_gib" is forbidden for type "file"`},
		{"file thin zero forbidden", "{name: files, type: file, root_provisioning: copy, zero_new_blocks: false}", `field "zero_new_blocks" is forbidden for type "file"`},
		{"file thick wipe forbidden", "{name: files, type: file, root_provisioning: copy, wipe_on_create: false}", `field "wipe_on_create" is forbidden for type "file"`},
		{"thin valid", "{name: thin, type: lvm-thin, volume_group: vg, pool_size_gib: 4, root_provisioning: snapshot}", ""},
		{"thin zero valid", "{name: thin, type: lvm-thin, volume_group: vg, pool_size_gib: 4, root_provisioning: snapshot, zero_new_blocks: true}", ""},
		{"thin thick wipe forbidden", "{name: thin, type: lvm-thin, volume_group: vg, pool_size_gib: 4, root_provisioning: snapshot, wipe_on_create: false}", `field "wipe_on_create" is forbidden for type "lvm-thin"`},
		{"thin missing VG", "{name: thin, type: lvm-thin, pool_size_gib: 4, root_provisioning: copy}", `field "volume_group" is required for type "lvm-thin"`},
		{"thin empty VG", "{name: thin, type: lvm-thin, volume_group: '', pool_size_gib: 4, root_provisioning: copy}", `field "volume_group" is required for type "lvm-thin"`},
		{"thin missing pool", "{name: thin, type: lvm-thin, volume_group: vg, root_provisioning: copy}", `field "pool_size_gib" must be positive for type "lvm-thin"`},
		{"thin zero pool", "{name: thin, type: lvm-thin, volume_group: vg, pool_size_gib: 0, root_provisioning: copy}", `field "pool_size_gib" must be positive for type "lvm-thin"`},
		{"thin bad provisioning", "{name: thin, type: lvm-thin, volume_group: vg, pool_size_gib: 4, root_provisioning: reflink}", `must be copy or snapshot for type "lvm-thin"`},
		{"thick valid", "{name: thick, type: lvm-thick, volume_group: vg}", ""},
		{"thick wipe valid", "{name: thick, type: lvm-thick, volume_group: vg, wipe_on_create: true}", ""},
		{"thick thin zero forbidden", "{name: thick, type: lvm-thick, volume_group: vg, zero_new_blocks: false}", `field "zero_new_blocks" is forbidden for type "lvm-thick"`},
		{"thick empty VG", "{name: thick, type: lvm-thick, volume_group: ''}", `field "volume_group" is required for type "lvm-thick"`},
		{"thick zero pool forbidden", "{name: thick, type: lvm-thick, volume_group: vg, pool_size_gib: 0}", `field "pool_size_gib" is forbidden for type "lvm-thick"`},
		{"thick empty provisioning forbidden", "{name: thick, type: lvm-thick, volume_group: vg, root_provisioning: ''}", `field "root_provisioning" is forbidden for type "lvm-thick"`},
		{"unsupported", "{name: bad, type: directory}", `unsupported type "directory"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := Parse([]byte(base(test.storage)), t.TempDir(), nil)
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEveryDiskRequiresKnownStorage(t *testing.T) {
	doc, template := validVMDocument(t, "")
	lookup := func(name string) (string, bool) {
		if name == "IMAGE" {
			return template, true
		}
		if name == "KEY" {
			return testKey(t), true
		}
		return "", false
	}
	for _, test := range []struct{ name, old, new, want string }{
		{"root missing", "root_disk: {storage: local}", "root_disk: {}", "root_disk.storage is required"},
		{"root unknown", "root_disk: {storage: local}", "root_disk: {storage: missing}", `root_disk references unknown storage "missing"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := strings.Replace(string(doc), test.old, test.new, 1)
			_, _, err := Parse([]byte(changed), t.TempDir(), lookup)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, test := range []struct{ name, disk, want string }{
		{"additional missing", "{name: data, size_gib: 1}", `disk "data" storage is required`},
		{"additional unknown", "{name: data, size_gib: 1, storage: missing}", `disk "data" references unknown storage "missing"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := strings.Replace(string(doc), "    interfaces:\n", "    additional_disks: ["+test.disk+"]\n    interfaces:\n", 1)
			_, _, err := Parse([]byte(changed), t.TempDir(), lookup)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
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
vms: [{name: management_host}]
links: []`,
		"trunk filtering": `name: x
routers:
  - {name: r, interfaces: [{name: lan}]}
switches: [{name: s}]
links:
  - name: l
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
  - name: r
    interfaces:
      - name: wan
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
		"router DHCP client": "interfaces: [{name: lan, ipv4: {dhcp: true}}]",
	} {
		t.Run(name, func(t *testing.T) {
			doc := "name: x\nrouters:\n  - name: r\n    " + field + "\n"
			if name != "router DHCP client" {
				doc += "    interfaces: [{name: wan, external: {sources: [10.0.0.0/24]}}]\n"
			} else {
				doc += "switches: [{name: s}]\nlinks: [{name: l, endpoints: [{node: r, interface: lan}, {node: s, interface: r}]}]\n"
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
