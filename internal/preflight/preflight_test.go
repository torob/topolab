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
