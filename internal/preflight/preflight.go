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

var networkCommands = []string{"ip", "bridge", "tc", "nft"}

const (
	// DefaultOVMFCode and DefaultOVMFVars are the Ubuntu firmware paths. They
	// are the fallback when no directory in FirmwareDirs holds a usable pair.
	DefaultOVMFCode = "/usr/share/OVMF/OVMF_CODE_4M.fd"
	DefaultOVMFVars = "/usr/share/OVMF/OVMF_VARS_4M.fd"
)

// FirmwareDirs lists, in order, the directories that may hold the OVMF code and
// variables images: Ubuntu first, then Proxmox VE (spec.md Section 2).
var FirmwareDirs = []string{"/usr/share/OVMF", "/usr/share/pve-edk2-firmware"}

// DefaultFirmware returns the first directory's OVMF_CODE_4M.fd and
// OVMF_VARS_4M.fd in which both are readable regular files. A code image and a
// variables image from different directories are never combined. When no
// directory qualifies it returns the Ubuntu paths, so that the error names the
// standard location.
func DefaultFirmware(stat func(string) (os.FileInfo, error)) (code, vars string) {
	if stat == nil {
		stat = os.Stat
	}
	usable := func(path string) bool {
		info, err := stat(path)
		return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o444 != 0
	}
	for _, dir := range FirmwareDirs {
		c, v := dir+"/OVMF_CODE_4M.fd", dir+"/OVMF_VARS_4M.fd"
		if usable(c) && usable(v) {
			return c, v
		}
	}
	return DefaultOVMFCode, DefaultOVMFVars
}

type HelperPaths struct {
	QEMUSystem string
	QEMUImg    string
	OVMFCode   string
	OVMFVars   string
	Xorriso    string
}

type Options struct {
	EUID            func() int
	LookPath        func(string) (string, error)
	Stat            func(string) (os.FileInfo, error)
	OpenKVM         func() error
	CommandOutput   func(string, ...string) ([]byte, error)
	NeedsDNSMasq    bool
	RequireVM       bool
	RequireFirmware bool // Deprecated compatibility alias for RequireVM.
	Helpers         HelperPaths
	HelpersResolved bool
	CPUModels       []string
	DeviceModels    []string
	CPUPlans        []CPUPlan
	ProbeCPUPlan    func(CPUPlan) error
}

func ResolveHelperPaths(requireVM bool, paths HelperPaths, lookPath func(string) (string, error), stat func(string) (os.FileInfo, error)) (HelperPaths, error) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if stat == nil {
		stat = os.Stat
	}
	if !requireVM {
		return paths, nil
	}
	resolveExecutable := func(label, value, fallback string) (string, error) {
		explicit := value != ""
		if value == "" {
			value = fallback
		}
		resolved, err := lookPath(value)
		if err != nil {
			return "", fmt.Errorf("required command %q is unavailable: %w", label, err)
		}
		if !explicit {
			return resolved, nil
		}
		info, err := stat(resolved)
		if err != nil {
			return "", fmt.Errorf("required executable %q: %w", resolved, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("required executable %q is not a regular file", resolved)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("required executable %q is not executable", resolved)
		}
		return resolved, nil
	}
	var err error
	if paths.QEMUSystem, err = resolveExecutable("qemu-system-x86_64", paths.QEMUSystem, "qemu-system-x86_64"); err != nil {
		return paths, err
	}
	if paths.QEMUImg, err = resolveExecutable("qemu-img", paths.QEMUImg, "qemu-img"); err != nil {
		return paths, err
	}
	if paths.Xorriso, err = resolveExecutable("xorriso", paths.Xorriso, "xorriso"); err != nil {
		return paths, err
	}
	defaultCode, defaultVars := DefaultFirmware(stat)
	for _, firmware := range []struct {
		label    string
		value    *string
		fallback string
	}{{"OVMF code", &paths.OVMFCode, defaultCode}, {"OVMF variables", &paths.OVMFVars, defaultVars}} {
		if *firmware.value == "" {
			*firmware.value = firmware.fallback
		}
		info, statErr := stat(*firmware.value)
		if statErr != nil {
			return paths, fmt.Errorf("required %s firmware %q: %w", firmware.label, *firmware.value, statErr)
		}
		if !info.Mode().IsRegular() {
			return paths, fmt.Errorf("required %s firmware %q is not a regular file", firmware.label, *firmware.value)
		}
		if info.Mode().Perm()&0o444 == 0 {
			return paths, fmt.Errorf("required %s firmware %q is not readable", firmware.label, *firmware.value)
		}
	}
	return paths, nil
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
	if opts.EUID() != 0 {
		return errors.New("runtime commands require effective UID 0")
	}
	commands := append([]string(nil), networkCommands...)
	if opts.NeedsDNSMasq {
		commands = append(commands, "dnsmasq")
	}
	for _, command := range commands {
		if _, err := opts.LookPath(command); err != nil {
			return fmt.Errorf("required command %q is unavailable: %w", command, err)
		}
	}
	requireVM := opts.RequireVM || opts.RequireFirmware
	if !requireVM {
		return nil
	}
	helpers := opts.Helpers
	if !opts.HelpersResolved {
		var err error
		helpers, err = ResolveHelperPaths(true, helpers, opts.LookPath, opts.Stat)
		if err != nil {
			return err
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
	if requireVM {
		for _, path := range []string{helpers.OVMFCode, helpers.OVMFVars} {
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
		out, err := opts.CommandOutput(helpers.QEMUSystem, "-cpu", "help")
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
		out, err := opts.CommandOutput(helpers.QEMUSystem, "-device", "help")
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
		probe := opts.ProbeCPUPlan
		if probe == nil {
			probe = func(plan CPUPlan) error { return probeCPUPlanWithExecutable(helpers.QEMUSystem, plan) }
		}
		if err := probe(plan); err != nil {
			return fmt.Errorf("QEMU/KVM cannot run CPU plan %s sockets=%d cores=%d threads=%d: %w", plan.Model, plan.Sockets, plan.Cores, plan.Threads, err)
		}
	}
	return nil
}

func probeCPUPlan(plan CPUPlan) error {
	return probeCPUPlanWithExecutable("qemu-system-x86_64", plan)
}

func probeCPUPlanWithExecutable(executable string, plan CPUPlan) error {
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
	cmd := exec.CommandContext(ctx, executable, args...)
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
