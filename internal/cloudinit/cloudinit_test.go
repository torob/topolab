package cloudinit

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestRenderDeterministicNoCloud(t *testing.T) {
	in := Network{Interfaces: []Interface{{
		Name: "eth0", MAC: "02:00:00:00:00:01", MTU: 1500,
		IPv4:  &IPv4{Addresses: []string{"10.0.0.2/24"}, Routes: []Route{{To: "default", Via: "10.0.0.1"}}, DNSServers: []string{"1.1.1.1"}},
		VLANs: []VLAN{{ID: 10, MTU: 1400, IPv4: &IPv4{DHCP: true}}},
	}}}
	seed, err := Render("instance", UserData{Hostname: "vm", AuthorizedKeys: []string{"ssh-ed25519 AAA key"}}, in)
	if err != nil {
		t.Fatal(err)
	}
	network := string(seed.NetworkConfig)
	for _, want := range []string{`macaddress: "02:00:00:00:00:01"`, `set-name: "eth0"`, `"eth0.10":`, `dhcp4: true`, `to: "0.0.0.0/0"`} {
		if !strings.Contains(network, want) {
			t.Errorf("missing %q in:\n%s", want, network)
		}
	}
	user := string(seed.UserData)
	if !strings.Contains(user, "ssh_deletekeys: false") || strings.Contains(user, "machine-id") || strings.Contains(user, "ca_certs:") {
		t.Fatalf("bad user data:\n%s", user)
	}
	again, _ := Render("instance", UserData{Hostname: "vm", AuthorizedKeys: []string{"ssh-ed25519 AAA key"}}, in)
	if seed.Digest() != again.Digest() {
		t.Fatal("digest is not deterministic")
	}
}

func TestRenderTrustedRootCACertificates(t *testing.T) {
	certificates := []string{
		"-----BEGIN CERTIFICATE-----\nfirst\n-----END CERTIFICATE-----\n",
		"-----BEGIN CERTIFICATE-----\nsecond\n-----END CERTIFICATE-----\n",
	}
	seed, err := Render("instance", UserData{Hostname: "vm", RootCACertificates: certificates}, Network{})
	if err != nil {
		t.Fatal(err)
	}
	user := string(seed.UserData)
	for _, want := range []string{
		"ca_certs:\n",
		"  remove_defaults: false\n",
		"  trusted:\n",
		`    - "-----BEGIN CERTIFICATE-----\nfirst\n-----END CERTIFICATE-----\n"`,
		`    - "-----BEGIN CERTIFICATE-----\nsecond\n-----END CERTIFICATE-----\n"`,
	} {
		if !strings.Contains(user, want) {
			t.Errorf("missing %q in:\n%s", want, user)
		}
	}
	if strings.Contains(user, "remove_defaults: true") {
		t.Fatalf("default trust roots would be removed:\n%s", user)
	}
}

func TestRenderNetworkOptionalInterfaces(t *testing.T) {
	tests := []struct {
		name     string
		iface    Interface
		optional bool
	}{
		{
			name:     "layer 2 only",
			iface:    Interface{Name: "legacy", MAC: "02:00:00:00:00:01"},
			optional: true,
		},
		{
			name:  "static IPv4",
			iface: Interface{Name: "static", MAC: "02:00:00:00:00:02", IPv4: &IPv4{Addresses: []string{"192.0.2.10/24"}}},
		},
		{
			name:  "DHCP",
			iface: Interface{Name: "dhcp", MAC: "02:00:00:00:00:03", IPv4: &IPv4{DHCP: true}},
		},
		{
			name:  "VLAN parent",
			iface: Interface{Name: "trunk", MAC: "02:00:00:00:00:04", VLANs: []VLAN{{ID: 10}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			network, err := renderNetwork(Network{Interfaces: []Interface{tt.iface}})
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(string(network), "    optional: true\n")
			if got != tt.optional {
				t.Fatalf("optional = %t, want %t in:\n%s", got, tt.optional, network)
			}
		})
	}
}

func TestInstanceIDChangesWithGuestIdentity(t *testing.T) {
	a := InstanceID("lab", "vm", "storage", "a")
	b := InstanceID("lab", "vm", "storage", "b")
	if a == b {
		t.Fatal("instance ID did not change")
	}
}

type isoTestRunner struct{ calls int }

func (r *isoTestRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.calls++
	for i, arg := range args {
		if arg == "-output" && i+1 < len(args) {
			return nil, os.WriteFile(args[i+1], []byte("iso"), 0o600)
		}
	}
	return nil, nil
}

func TestISOBuilderNeverReplacesVMSeed(t *testing.T) {
	runner := &isoTestRunner{}
	builder := NewISOBuilder(runner, "xorriso")
	destination := t.TempDir() + "/seed.iso"
	seed := Seed{MetaData: []byte("meta"), UserData: []byte("user"), NetworkConfig: []byte("network")}
	if err := builder.Build(context.Background(), destination, seed); err != nil {
		t.Fatal(err)
	}
	if err := builder.Build(context.Background(), destination, seed); err == nil || !strings.Contains(err.Error(), "seed already exists") {
		t.Fatalf("second build error = %v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "iso" || runner.calls != 1 {
		t.Fatalf("existing seed changed: data=%q calls=%d err=%v", data, runner.calls, err)
	}
}
