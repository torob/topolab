package preflight

import (
	"errors"
	"os"
	"testing"
	"time"
)

type fakeInfo struct{ mode os.FileMode }

func (f fakeInfo) Name() string       { return "fake" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

func validOptions() Options {
	return Options{
		EUID:     func() int { return 0 },
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Stat: func(path string) (os.FileInfo, error) {
			if path == "/dev/kvm" {
				return fakeInfo{mode: os.ModeDevice | os.ModeCharDevice}, nil
			}
			return fakeInfo{mode: 0o644}, nil
		},
		OpenKVM:         func() error { return nil },
		RequireFirmware: true,
		ProbeCPUPlan:    func(CPUPlan) error { return nil },
	}
}

func TestRuntimeProbesEveryCPUPlan(t *testing.T) {
	opts := validOptions()
	opts.CPUPlans = []CPUPlan{{Model: "host", Sockets: 1, Cores: 2, Threads: 1}}
	called := 0
	opts.ProbeCPUPlan = func(plan CPUPlan) error {
		called++
		if plan.Cores != 2 {
			t.Fatalf("unexpected plan: %#v", plan)
		}
		return nil
	}
	if err := Runtime(opts); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("probe calls = %d", called)
	}
}

func TestRuntimeAcceptsCompleteHost(t *testing.T) {
	if err := Runtime(validOptions()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRejectsNonRootBeforeLookingAtHost(t *testing.T) {
	opts := validOptions()
	opts.EUID = func() int { return 1000 }
	opts.LookPath = func(string) (string, error) { t.Fatal("looked up command"); return "", nil }
	if err := Runtime(opts); err == nil {
		t.Fatal("expected error")
	}
}

func TestRuntimeRequiresDNSMasqOnlyWhenRequested(t *testing.T) {
	opts := validOptions()
	opts.NeedsDNSMasq = true
	opts.LookPath = func(name string) (string, error) {
		if name == "dnsmasq" {
			return "", errors.New("missing")
		}
		return "/tools/" + name, nil
	}
	if err := Runtime(opts); err == nil {
		t.Fatal("expected error")
	}
}

func TestRouterOnlyRuntimeSkipsVMRequirements(t *testing.T) {
	opts := validOptions()
	opts.RequireFirmware = false
	opts.RequireVM = false
	opts.LookPath = func(name string) (string, error) {
		if name == "qemu-system-x86_64" || name == "qemu-img" || name == "xorriso" {
			t.Fatalf("looked up VM-only command %s", name)
		}
		return "/tools/" + name, nil
	}
	opts.Stat = func(string) (os.FileInfo, error) { t.Fatal("stat VM requirement"); return nil, nil }
	opts.OpenKVM = func() error { t.Fatal("opened KVM"); return nil }
	if err := Runtime(opts); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitHelperOverridesMustBeUsableRegularFiles(t *testing.T) {
	paths := HelperPaths{QEMUSystem: "/qemu", QEMUImg: "/img", Xorriso: "/xorriso", OVMFCode: "/code", OVMFVars: "/vars"}
	look := func(name string) (string, error) { return name, nil }
	stat := func(path string) (os.FileInfo, error) { return fakeInfo{mode: 0o755}, nil }
	if got, err := ResolveHelperPaths(true, paths, look, stat); err != nil || got != paths {
		t.Fatalf("valid overrides = %#v, %v", got, err)
	}
	for _, bad := range []struct {
		path string
		mode os.FileMode
	}{{"/qemu", 0o644}, {"/img", os.ModeDir | 0o755}, {"/code", 0o000}} {
		_, err := ResolveHelperPaths(true, paths, look, func(path string) (os.FileInfo, error) {
			if path == bad.path {
				return fakeInfo{mode: bad.mode}, nil
			}
			return fakeInfo{mode: 0o755}, nil
		})
		if err == nil {
			t.Fatalf("accepted %s mode %v", bad.path, bad.mode)
		}
	}
}

func TestRuntimeUsesSelectedQEMUForCapabilityProbes(t *testing.T) {
	opts := validOptions()
	opts.Helpers = HelperPaths{QEMUSystem: "/selected/qemu", QEMUImg: "/selected/img", Xorriso: "/selected/xorriso", OVMFCode: "/selected/code", OVMFVars: "/selected/vars"}
	opts.HelpersResolved = true
	opts.CPUModels = []string{"host"}
	opts.DeviceModels = []string{"virtio-net-pci"}
	var commands []string
	opts.CommandOutput = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+args[0])
		return []byte("virtio-net-pci"), nil
	}
	if err := Runtime(opts); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0] != "/selected/qemu -cpu" || commands[1] != "/selected/qemu -device" {
		t.Fatalf("capability commands = %v", commands)
	}
}
