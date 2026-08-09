//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/state"
)

func TestParseExecStrict(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want execOptions
		bad  bool
	}{
		{"argv", []string{"--", "printf", "%s", "hello world"}, execOptions{Argv: []string{"printf", "%s", "hello world"}}, false},
		{"options", []string{"--user", "alice", "--cwd", "/tmp", "--", "pwd"}, execOptions{User: "alice", Cwd: "/tmp", Argv: []string{"pwd"}}, false},
		{"missing separator", []string{"true"}, execOptions{}, true},
		{"missing command", []string{"--"}, execOptions{}, true},
		{"unknown", []string{"--shell", "--", "true"}, execOptions{}, true},
		{"equals rejected", []string{"--cwd=/tmp", "--", "true"}, execOptions{}, true},
		{"missing value", []string{"--cwd", "--", "true"}, execOptions{}, true},
		{"duplicate", []string{"--user", "a", "--user", "b", "--", "true"}, execOptions{}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseExec(test.args)
			if (err != nil) != test.bad {
				t.Fatalf("error=%v bad=%t", err, test.bad)
			}
			if !test.bad && (got.User != test.want.User || got.Cwd != test.want.Cwd || !slices.Equal(got.Argv, test.want.Argv)) {
				t.Fatalf("got %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRouterExecTargetAndOwnedNamespace(t *testing.T) {
	cfg := &config.Config{Name: "lab", VMs: []*config.VM{{Name: "vm"}}, Routers: []*config.Router{{Name: "edge"}}, Switches: []*config.Switch{{Name: "sw"}}}
	if err := validateRouterExecTarget(cfg, "edge"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vm", "sw", "missing"} {
		if err := validateRouterExecTarget(cfg, name); err == nil {
			t.Fatalf("accepted target %q", name)
		}
	}
	ns := network.RouterNamespaceName("lab", "edge")
	alias := network.NamespaceOwnerAlias("lab", "edge")
	valid := &network.State{Namespaces: map[string]string{"edge": ns}, NamespaceAliases: map[string]string{"edge": alias}, Resources: []network.Resource{{Kind: network.ResourceNamespace, Name: ns, Alias: alias}}}
	if got, err := routerExecNamespace("lab", "edge", valid); err != nil || got != ns {
		t.Fatalf("namespace = %q, %v", got, err)
	}
	for _, mutate := range []func(*network.State){
		func(s *network.State) { s.Namespaces["edge"] = "" },
		func(s *network.State) { s.Namespaces["edge"] = "wrong" },
		func(s *network.State) { s.NamespaceAliases["edge"] = "wrong" },
		func(s *network.State) { s.Resources = nil },
	} {
		copyState := *valid
		copyState.Namespaces = map[string]string{"edge": valid.Namespaces["edge"]}
		copyState.NamespaceAliases = map[string]string{"edge": valid.NamespaceAliases["edge"]}
		copyState.Resources = append([]network.Resource(nil), valid.Resources...)
		mutate(&copyState)
		if _, err := routerExecNamespace("lab", "edge", &copyState); err == nil {
			t.Fatalf("accepted invalid state %#v", copyState)
		}
	}
}

func TestRouterExecRejectsAbsentLegacyAndUnownedStateBeforeStarting(t *testing.T) {
	cfg := &config.Config{Name: "lab", Routers: []*config.Router{{Name: "edge"}}}
	parent, err := os.MkdirTemp("/tmp", "tlexec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	layout, err := state.NewLayout(filepath.Join(parent, "work"), "lab")
	if err != nil {
		t.Fatal(err)
	}
	a := New(nil, nil)
	a.defaults()
	args := []string{"edge", "--", "true"}
	if err := a.runRouterExec(context.Background(), cfg, layout, nil, args); err == nil || !strings.Contains(err.Error(), "not up") {
		t.Fatalf("absent error = %v", err)
	}
	manifest := state.NewManifest("lab")
	legacy := &network.State{DatapathVersion: 1, Topology: "lab"}
	manifest.NetworkState, _ = json.Marshal(legacy)
	if err := a.runRouterExec(context.Background(), cfg, layout, manifest, args); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy error = %v", err)
	}
	missing := &network.State{DatapathVersion: network.DatapathVersion, Topology: "lab"}
	manifest.NetworkState, _ = json.Marshal(missing)
	if err := a.runRouterExec(context.Background(), cfg, layout, manifest, args); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("missing namespace error = %v", err)
	}
	wrong := &network.State{DatapathVersion: network.DatapathVersion, Topology: "lab", Namespaces: map[string]string{"edge": "wrong"}}
	manifest.NetworkState, _ = json.Marshal(wrong)
	if err := a.runRouterExec(context.Background(), cfg, layout, manifest, args); err == nil || !strings.Contains(err.Error(), "owned namespace") {
		t.Fatalf("ownership error = %v", err)
	}
}

func TestExecIdentityArgumentPreservesManagementDefaultAndRootsRouters(t *testing.T) {
	if got := execIdentityArgument("", false); got != "" {
		t.Fatalf("management default = %q", got)
	}
	if got := execIdentityArgument("", true); got != "0" {
		t.Fatalf("router default = %q", got)
	}
	if got := execIdentityArgument("alice", true); got != "alice" {
		t.Fatalf("explicit router identity = %q", got)
	}
}

func TestExecCapabilityPolicyRetainsOnlyRootCapabilities(t *testing.T) {
	if shouldClearExecCapabilities(execIdentity{UID: 0}) {
		t.Fatal("root identity would lose required capabilities")
	}
	if !shouldClearExecCapabilities(execIdentity{UID: 1000}) {
		t.Fatal("non-root identity would retain capabilities")
	}
}

type fakeIdentityLookup struct {
	byName map[string]*user.User
	byID   map[string]*user.User
	groups map[string][]string
}

func (f fakeIdentityLookup) Lookup(value string) (*user.User, error) {
	if result := f.byName[value]; result != nil {
		return result, nil
	}
	return nil, errors.New("unknown user")
}
func (f fakeIdentityLookup) LookupID(value string) (*user.User, error) {
	if result := f.byID[value]; result != nil {
		return result, nil
	}
	return nil, errors.New("unknown uid")
}
func (f fakeIdentityLookup) GroupIDs(value *user.User) ([]string, error) {
	return append([]string(nil), f.groups[value.Uid]...), nil
}

func TestResolveExecIdentityPrecedenceAndGroups(t *testing.T) {
	alice := &user.User{Uid: "1000", Gid: "100", Username: "alice", HomeDir: "/home/alice"}
	bob := &user.User{Uid: "2000", Gid: "200", Username: "bob", HomeDir: "/srv/bob"}
	root := &user.User{Uid: "0", Gid: "0", Username: "root", HomeDir: "/root"}
	lookup := fakeIdentityLookup{byName: map[string]*user.User{"bob": bob}, byID: map[string]*user.User{"0": root, "1000": alice, "2000": bob}, groups: map[string][]string{"0": {"0"}, "1000": {"100", "101"}, "2000": {"200", "201"}}}
	env := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	}

	got, err := resolveExecIdentity("", 0, 0, env(map[string]string{"SUDO_UID": "1000", "SUDO_GID": "150"}), lookup)
	if err != nil || got.UID != 1000 || got.GID != 150 || !slices.Equal(got.Groups, []uint32{100, 101, 150}) {
		t.Fatalf("sudo identity = %#v, %v", got, err)
	}
	got, err = resolveExecIdentity("bob", 1000, 100, env(nil), lookup)
	if err != nil || got.UID != 2000 || got.GID != 200 || got.Name != "bob" || got.Home != "/srv/bob" {
		t.Fatalf("explicit identity = %#v, %v", got, err)
	}
	got, err = resolveExecIdentity("", 1000, 100, env(nil), lookup)
	if err != nil || got.UID != 1000 || got.GID != 100 {
		t.Fatalf("real identity = %#v, %v", got, err)
	}
	got, err = resolveExecIdentity("", 0, 0, env(nil), lookup)
	if err != nil || got.UID != 0 || got.GID != 0 {
		t.Fatalf("root identity = %#v, %v", got, err)
	}
	for _, values := range []map[string]string{{"SUDO_UID": "bad", "SUDO_GID": "1"}, {"SUDO_UID": "1"}, {"SUDO_UID": "1", "SUDO_GID": "bad"}} {
		if _, err := resolveExecIdentity("", 0, 0, env(values), lookup); err == nil {
			t.Fatalf("accepted malformed sudo identity %#v", values)
		}
	}
}

func TestSanitizedExecEnvironment(t *testing.T) {
	id := execIdentity{Name: "alice", Home: "/home/alice"}
	got := sanitizedExecEnvironment([]string{"PATH=/bin", "HOME=/root", "USER=root", "LOGNAME=root", "SUDO_UID=1000", "LD_PRELOAD=bad", "TERM=xterm"}, id)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"PATH=/bin", "TERM=xterm", "HOME=/home/alice", "USER=alice", "LOGNAME=alice"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	for _, bad := range []string{"HOME=/root", "SUDO_UID", "LD_PRELOAD"} {
		if strings.Contains(joined, bad) {
			t.Errorf("retained %s in %v", bad, got)
		}
	}
}
