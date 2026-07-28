// Package preflight checks the immutable host contract before reconciliation.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var runtimeCommands = []string{
	"qemu-system-x86_64", "qemu-img", "ip", "bridge", "tc", "nft", "xorriso",
}

type Options struct {
	EUID            func() int
	LookPath        func(string) (string, error)
	Stat            func(string) (os.FileInfo, error)
	OpenKVM         func() error
	CommandOutput   func(string, ...string) ([]byte, error)
	NeedsDNSMasq    bool
	RequireFirmware bool
	CPUModels       []string
	DeviceModels    []string
	CPUPlans        []CPUPlan
	ProbeCPUPlan    func(CPUPlan) error
}

type CPUPlan struct {
	Model                   string
	Sockets, Cores, Threads int
}

func Runtime(opts Options) error {
	if opts.EUID == nil {
		opts.EUID = os.Geteuid
	}
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.Stat == nil {
		opts.Stat = os.Stat
	}
	if opts.OpenKVM == nil {
		opts.OpenKVM = func() error {
			f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return f.Close()
		}
	}
	if opts.CommandOutput == nil {
		opts.CommandOutput = func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		}
	}
	if opts.ProbeCPUPlan == nil {
		opts.ProbeCPUPlan = probeCPUPlan
	}
	if opts.EUID() != 0 {
		return errors.New("runtime commands require effective UID 0")
	}
	commands := append([]string(nil), runtimeCommands...)
	if opts.NeedsDNSMasq {
		commands = append(commands, "dnsmasq")
	}
	for _, command := range commands {
		if _, err := opts.LookPath(command); err != nil {
			return fmt.Errorf("required command %q is unavailable: %w", command, err)
		}
	}
	info, err := opts.Stat("/dev/kvm")
	if err != nil {
		return fmt.Errorf("hardware KVM is required: %w", err)
	}
	if info.Mode()&os.ModeDevice == 0 {
		return errors.New("/dev/kvm is not a device")
	}
	if err := opts.OpenKVM(); err != nil {
		return fmt.Errorf("open /dev/kvm read/write: %w", err)
	}
	if opts.RequireFirmware {
		for _, path := range []string{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"} {
			info, err := opts.Stat(path)
			if err != nil {
				return fmt.Errorf("required OVMF firmware %q: %w", path, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("required OVMF firmware %q is not a regular file", path)
			}
		}
	}
	if len(opts.CPUModels) > 0 {
		out, err := opts.CommandOutput("qemu-system-x86_64", "-cpu", "help")
		if err != nil {
			return fmt.Errorf("probe QEMU CPU models: %w: %s", err, out)
		}
		text := string(out)
		for _, model := range opts.CPUModels {
			if model != "host" && !containsField(text, model) {
				return fmt.Errorf("installed QEMU does not support CPU model %q", model)
			}
		}
	}
	if len(opts.DeviceModels) > 0 {
		out, err := opts.CommandOutput("qemu-system-x86_64", "-device", "help")
		if err != nil {
			return fmt.Errorf("probe QEMU device models: %w: %s", err, out)
		}
		text := string(out)
		for _, model := range opts.DeviceModels {
			if !containsField(text, model) && !strings.Contains(text, `name "`+model+`"`) {
				return fmt.Errorf("installed QEMU does not support device model %q", model)
			}
		}
	}
	for _, plan := range opts.CPUPlans {
		if err := opts.ProbeCPUPlan(plan); err != nil {
			return fmt.Errorf("QEMU/KVM cannot run CPU plan %s sockets=%d cores=%d threads=%d: %w", plan.Model, plan.Sockets, plan.Cores, plan.Threads, err)
		}
	}
	return nil
}

func probeCPUPlan(plan CPUPlan) error {
	if plan.Model == "" || plan.Sockets <= 0 || plan.Cores <= 0 || plan.Threads <= 0 {
		return errors.New("invalid CPU plan")
	}
	total := int64(plan.Sockets) * int64(plan.Cores) * int64(plan.Threads)
	if total <= 0 || total > int64(^uint(0)>>1) {
		return errors.New("CPU plan overflows")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{
		"-machine", "q35,accel=kvm", "-cpu", plan.Model,
		"-smp", "cpus=" + strconv.FormatInt(total, 10) + ",sockets=" + strconv.Itoa(plan.Sockets) + ",cores=" + strconv.Itoa(plan.Cores) + ",threads=" + strconv.Itoa(plan.Threads),
		"-m", "128", "-display", "none", "-nodefaults", "-no-user-config",
		"-serial", "none", "-monitor", "none", "-S", "-qmp", "stdio",
	}
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	cmd.Stdin = strings.NewReader(`{"execute":"qmp_capabilities"}` + "\n" + `{"execute":"quit"}` + "\n")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("bounded probe timed out: %w", ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func containsField(text, want string) bool {
	for _, field := range strings.Fields(text) {
		if strings.Trim(field, `",`) == want {
			return true
		}
	}
	return false
}
