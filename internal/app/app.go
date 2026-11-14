// Package app integrates TopoLab's configuration, state, host networking,
// storage, QEMU and observability packages into the public command lifecycle.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/torob/topolab/internal/cloudinit"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/observability"
	"github.com/torob/topolab/internal/preflight"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
	storagepkg "github.com/torob/topolab/internal/storage"
)

type Command struct {
	Name       string
	Args       []string
	Topology   *config.Config
	WorkingDir string
	Helpers    HelperOverrides
}

type HelperOverrides struct {
	QEMUSystem string
	QEMUImg    string
	LVM        string
	OVMFCode   string
	OVMFVars   string
	Xorriso    string
}

type imageManager interface {
	ValidateTemplate(context.Context, string) (qemu.TemplateInfo, error)
	Info(context.Context, string) (qemu.ImageInfo, error)
	CreateOverlay(context.Context, string, qemu.TemplateInfo, uint64) error
	CreateDataDisk(context.Context, string, uint64) error
	ResizeOffline(context.Context, qemu.Growth) error
	ResizeOnline(context.Context, qemu.BlockResizer, qemu.Growth) error
}
type copyImageManager interface {
	CreateCopy(context.Context, string, qemu.TemplateInfo, uint64) error
	ConvertToRaw(context.Context, string, string) error
}
type seedBuilder interface {
	Build(context.Context, string, cloudinit.Seed) error
}
type processStopper interface {
	Stop(context.Context, int, string, string, string) error
}
type qmpClient interface {
	qemu.BlockResizer
	Balloon(context.Context, uint64) error
	QueryBalloon(context.Context) (uint64, error)
	QueryStatus(context.Context) (string, error)
	Close() error
}

type App struct {
	Stdin           io.Reader
	Stdout, Stderr  io.Writer
	Network         network.HostNetwork
	Images          imageManager
	LVM             *storagepkg.LVM
	Seeds           seedBuilder
	QEMUBuilder     *qemu.Builder
	StartProcess    func([]string, []*os.File) (*qemu.StartedProcess, error)
	Namespaces      network.NamespaceOps
	Stopper         processStopper
	Processes       qemu.ProcessOps
	Preflight       func(preflight.Options) error
	OpenQMP         func(string) qmpClient
	Now             func() time.Time
	Executable      func() (string, error)
	VhostAvailable  func() bool
	GuestAgentProbe func(context.Context, string) bool
	ProbeIOUring    func(context.Context, string) error
	ZeroBlockDevice func(string, uint64) error
	OVMFCode        string
	OVMFVars        string
	// Sleep waits for d and returns early with the context error on cancel.
	// It runs the VM start_delay wait.
	Sleep         func(context.Context, time.Duration) error
	imagesDefault bool
	seedsDefault  bool
	qemuDefault   bool
	lvmDefault    bool
	autoAIO       string
}

// LockWaitNotice is the line a lifecycle command writes to stderr when another
// command, for example a running `exec`, holds the topology lock.
const LockWaitNotice = "waiting for the topology lock"

// sleepContext waits for d or until ctx ends, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func New(stdout, stderr io.Writer) *App {
	return &App{Stdout: stdout, Stderr: stderr}
}

func (a *App) defaults() {
	if a.Stdout == nil {
		a.Stdout = io.Discard
	}
	if a.Stderr == nil {
		a.Stderr = io.Discard
	}
	if a.Stdin == nil {
		a.Stdin = os.Stdin
	}
	if a.Network == nil {
		a.Network = network.New(nil, network.Options{})
	}
	if a.Images == nil {
		a.Images = qemu.NewImageManager(nil, "")
		a.imagesDefault = true
	}
	if a.LVM == nil {
		a.LVM = storagepkg.NewLVM(nil, "")
		a.lvmDefault = true
	}
	if a.Seeds == nil {
		a.Seeds = cloudinit.NewISOBuilder(nil, "")
		a.seedsDefault = true
	}
	if a.QEMUBuilder == nil {
		a.QEMUBuilder = qemu.NewBuilder("")
		a.qemuDefault = true
	}
	if a.OVMFCode == "" {
		a.OVMFCode = preflight.DefaultOVMFCode
	}
	if a.OVMFVars == "" {
		a.OVMFVars = preflight.DefaultOVMFVars
	}
	if a.StartProcess == nil {
		a.StartProcess = qemu.Start
	}
	if a.Namespaces == nil {
		a.Namespaces = network.OSNamespaceOps{}
	}
	if a.Stopper == nil {
		a.Stopper = qemu.NewStopper()
	}
	if a.Processes == nil {
		a.Processes = qemu.OSProcessOps{}
	}
	if a.Preflight == nil {
		a.Preflight = preflight.Runtime
	}
	if a.OpenQMP == nil {
		a.OpenQMP = func(path string) qmpClient { return qemu.NewQMPClient(path, nil) }
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.Sleep == nil {
		a.Sleep = sleepContext
	}
	if a.Executable == nil {
		a.Executable = os.Executable
	}
	if a.VhostAvailable == nil {
		a.VhostAvailable = vhostNetAvailable
	}
	if a.GuestAgentProbe == nil {
		a.GuestAgentProbe = qemu.GuestAgentAvailable
	}
	if a.ProbeIOUring == nil {
		a.ProbeIOUring = qemu.ProbeIOUring
	}
	if a.ZeroBlockDevice == nil {
		a.ZeroBlockDevice = zeroBlockDevice
	}
}

func (a *App) Run(ctx context.Context, command Command) error {
	a.defaults()
	if command.Topology == nil {
		return errors.New("nil topology")
	}
	layout, err := state.NewLayout(command.WorkingDir, command.Topology.Name)
	if err != nil {
		return err
	}
	switch command.Name {
	case "ps", "logs", "capture", "counters":
		m, err := state.Load(layout.ManifestPath)
		if err != nil {
			return err
		}
		if command.Name == "ps" {
			if err := a.configureLVM(command.Topology, m, command.Helpers); err != nil {
				return err
			}
		}
		if err := validateManifestPaths(layout, m); err != nil {
			return err
		}
		if networkState, err := manifestNetwork(m); err != nil {
			return err
		} else if err := validateManifestNetwork(layout, networkState); err != nil {
			return err
		}
		return a.runRead(ctx, command, layout, m)
	case "exec":
		m, err := state.Load(layout.ManifestPath)
		if err != nil {
			return err
		}
		if err := validateManifestPaths(layout, m); err != nil {
			return err
		}
		if networkState, err := manifestNetwork(m); err != nil {
			return err
		} else if err := validateManifestNetwork(layout, networkState); err != nil {
			return err
		}
		return a.runExec(ctx, layout, m, command.Args)
	case "router-exec":
		m, err := state.Load(layout.ManifestPath)
		if err != nil {
			return err
		}
		if err := validateManifestPaths(layout, m); err != nil {
			return err
		}
		if networkState, err := manifestNetwork(m); err != nil {
			return err
		} else if err := validateManifestNetwork(layout, networkState); err != nil {
			return err
		}
		return a.runRouterExec(ctx, command.Topology, layout, m, command.Args)
	case "up", "start", "stop", "restart", "down":
		// A missing down is genuinely read-only and idempotent. Start/stop also
		// report absence without manufacturing an empty topology directory.
		if command.Name != "up" {
			m, err := state.Load(layout.ManifestPath)
			if err != nil {
				return err
			}
			if m == nil {
				if command.Name == "down" {
					return nil
				}
				return errors.New("topology is not up")
			}
		}
		if command.Name == "up" || command.Name == "start" || command.Name == "restart" {
			if err := a.runtimePreflight(ctx, command.Topology, command.Helpers); err != nil {
				return err
			}
		}
		if err := layout.Ensure(); err != nil {
			return err
		}
		lock, err := state.AcquireLockNotify(ctx, layout.LockPath, func() { fmt.Fprintln(a.Stderr, LockWaitNotice) })
		if err != nil {
			return err
		}
		defer lock.Close()
		m, err := state.Load(layout.ManifestPath)
		if err != nil {
			return err
		}
		if command.Name != "stop" {
			if err := a.configureLVM(command.Topology, m, command.Helpers); err != nil {
				return err
			}
		}
		if command.Name == "up" && requiresLegacyDatapathDown(m) {
			return errors.New("active topology uses the legacy root datapath; run topolab down before up")
		}
		if err := validateManifestPaths(layout, m); err != nil {
			return err
		}
		if networkState, err := manifestNetwork(m); err != nil {
			return err
		} else if err := validateManifestNetwork(layout, networkState); err != nil {
			return err
		}
		switch command.Name {
		case "up":
			return a.up(ctx, command.Topology, layout, m)
		case "start":
			return a.start(ctx, command.Topology, layout, m, command.Args)
		case "stop":
			return a.stop(ctx, command.Topology, layout, m, command.Args)
		case "restart":
			return a.restart(ctx, command.Topology, layout, m, command.Args)
		case "down":
			if err := a.down(ctx, command.Topology, layout, m); err != nil {
				return err
			}
			return removeEmptyTopologyDirs(layout, m)
		}
	}
	return fmt.Errorf("unknown runtime command %q", command.Name)
}

func requiresLegacyDatapathDown(m *state.Manifest) bool {
	// Storage and disk write-ahead checkpoints can legitimately precede the
	// first network apply. DatapathVersion zero therefore means "not created",
	// not a legacy active datapath.
	return m != nil && m.DatapathVersion > 0 && m.DatapathVersion < network.DatapathVersion
}

func (a *App) runtimePreflight(ctx context.Context, cfg *config.Config, overrides HelperOverrides) error {
	needsDNS := false
	cpuModels := map[string]bool{}
	deviceModels := map[string]bool{"ide-cd": len(cfg.VMs) > 0}
	nicDevice := map[string]string{"virtio": "virtio-net-pci", "e1000e": "e1000e", "e1000": "e1000", "igb": "igb", "rtl8139": "rtl8139", "vmxnet3": "vmxnet3"}
	diskDevices := map[string][]string{"scsi": {"virtio-scsi-pci", "scsi-hd"}, "virtio": {"virtio-blk-pci"}, "sata": {"ich9-ahci", "ide-hd"}, "ide": {"piix3-ide", "ide-hd"}, "nvme": {"nvme"}}
	cpuPlans := map[string]preflight.CPUPlan{}
	for _, vm := range cfg.VMs {
		cpuModels[vm.CPU.Model] = true
		plan := preflight.CPUPlan{Model: vm.CPU.Model, Sockets: *vm.CPU.Sockets, Cores: *vm.CPU.Cores, Threads: *vm.CPU.Threads}
		cpuPlans[fmt.Sprintf("%s/%d/%d/%d", plan.Model, plan.Sockets, plan.Cores, plan.Threads)] = plan
		if vm.MemoryBalloon.Enabled {
			deviceModels["virtio-balloon-pci"] = true
		}
		if vm.GuestAgent {
			deviceModels["virtio-serial-pci"] = true
			deviceModels["virtserialport"] = true
		}
		for _, iface := range vm.Interfaces {
			deviceModels[nicDevice[iface.Model]] = true
		}
		for _, model := range diskDevices[vm.RootDisk.Bus] {
			deviceModels[model] = true
		}
		for _, disk := range vm.AdditionalDisks {
			for _, model := range diskDevices[disk.Bus] {
				deviceModels[model] = true
			}
		}
	}
	for _, r := range cfg.Routers {
		for _, in := range r.Interfaces {
			if in.IPv4 != nil && in.IPv4.DHCPServer != nil {
				needsDNS = true
			}
			for _, v := range in.VLANs {
				if v.IPv4 != nil && v.IPv4.DHCPServer != nil {
					needsDNS = true
				}
			}
		}
	}
	var cpus, devices []string
	var plans []preflight.CPUPlan
	for model := range cpuModels {
		cpus = append(cpus, model)
	}
	for model, needed := range deviceModels {
		if needed && model != "" {
			devices = append(devices, model)
		}
	}
	slices.Sort(cpus)
	slices.Sort(devices)
	planKeys := make([]string, 0, len(cpuPlans))
	for key := range cpuPlans {
		planKeys = append(planKeys, key)
	}
	slices.Sort(planKeys)
	for _, key := range planKeys {
		plans = append(plans, cpuPlans[key])
	}
	paths, err := preflight.ResolveHelperPaths(len(cfg.VMs) > 0, preflight.HelperPaths{QEMUSystem: overrides.QEMUSystem, QEMUImg: overrides.QEMUImg, OVMFCode: overrides.OVMFCode, OVMFVars: overrides.OVMFVars, Xorriso: overrides.Xorriso}, nil, nil)
	if err != nil {
		return err
	}
	if len(cfg.VMs) > 0 {
		if a.Images == nil || a.imagesDefault {
			a.Images = qemu.NewImageManager(nil, paths.QEMUImg)
		}
		if a.Seeds == nil || a.seedsDefault {
			a.Seeds = cloudinit.NewISOBuilder(nil, paths.Xorriso)
		}
		if a.QEMUBuilder == nil || a.qemuDefault {
			a.QEMUBuilder = qemu.NewBuilder(paths.QEMUSystem)
		}
		a.OVMFCode, a.OVMFVars = paths.OVMFCode, paths.OVMFVars
	}
	if err := a.Preflight(preflight.Options{NeedsDNSMasq: needsDNS, RequireVM: len(cfg.VMs) > 0, Helpers: paths, HelpersResolved: len(cfg.VMs) > 0, CPUModels: cpus, DeviceModels: devices, CPUPlans: plans}); err != nil {
		return err
	}
	needsAuto, needsExplicitIOUring := false, false
	for _, vm := range cfg.VMs {
		for _, aio := range append([]string{vm.RootDisk.AIO}, diskAIOs(vm.AdditionalDisks)...) {
			switch aio {
			case "", "auto":
				needsAuto = true
			case "io_uring":
				needsExplicitIOUring = true
			}
		}
	}
	a.autoAIO = "threads"
	if !needsAuto && !needsExplicitIOUring {
		return nil
	}
	probeExecutable := paths.QEMUSystem
	if a.QEMUBuilder != nil && a.QEMUBuilder.Executable != "" {
		probeExecutable = a.QEMUBuilder.Executable
	}
	if err := a.ProbeIOUring(ctx, probeExecutable); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if needsExplicitIOUring {
			return fmt.Errorf("io_uring AIO was explicitly requested but is unavailable: %w", err)
		}
		fmt.Fprintf(a.Stderr, "warning: io_uring AIO is unavailable; aio auto is using threads: %v\n", err)
		return nil
	}
	a.autoAIO = "io_uring"
	return nil
}

func diskAIOs(disks []*config.Disk) []string {
	values := make([]string, 0, len(disks))
	for _, disk := range disks {
		values = append(values, disk.AIO)
	}
	return values
}

func (a *App) configureLVM(cfg *config.Config, manifest *state.Manifest, overrides HelperOverrides) error {
	needsLVM := false
	for _, s := range cfg.Storages {
		if s.Type != "file" {
			needsLVM = true
		}
	}
	if manifest != nil {
		for _, s := range manifest.Storages {
			if s != nil && s.Type != "file" {
				needsLVM = true
			}
		}
	}
	if needsLVM && (a.LVM == nil || a.lvmDefault || overrides.LVM != "") {
		path := overrides.LVM
		var err error
		if path == "" {
			path, err = exec.LookPath("lvm")
		} else {
			path, err = filepath.Abs(path)
		}
		if err != nil {
			return fmt.Errorf("resolve lvm executable: %w", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect lvm executable: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("lvm executable is not an executable regular file: %s", path)
		}
		a.LVM = storagepkg.NewLVM(nil, path)
		a.lvmDefault = false
	}
	return nil
}

func (a *App) up(ctx context.Context, cfg *config.Config, layout state.Layout, old *state.Manifest) error {
	if old != nil && old.Topology != cfg.Name {
		return fmt.Errorf("manifest belongs to topology %q", old.Topology)
	}

	templates, err := a.validateTemplates(ctx, cfg)
	if err != nil {
		return err
	}
	netCfg := translateNetwork(cfg, layout.SharedDir, oldNetwork(old))
	plan, err := a.Network.Plan(ctx, netCfg)
	if err != nil {
		return err
	}
	configHash, _ := state.Hash(cfg)
	networkHash, _ := state.Hash(canonicalNetworkConfig(netCfg, plan.External))
	desired, err := a.desiredNodes(cfg, layout, templates)
	if err != nil {
		return err
	}
	if err := a.validateNodePlans(cfg, desired, netCfg); err != nil {
		return err
	}
	if err := a.validateExisting(ctx, old, desired); err != nil {
		return err
	}
	vgLocks, err := a.planStorages(ctx, cfg, layout, old)
	if err != nil {
		return err
	}
	defer func() {
		for i := len(vgLocks) - 1; i >= 0; i-- {
			_ = vgLocks[i].Close()
		}
	}()
	if err := a.verifyManifestStorage(ctx, old, false); err != nil {
		return err
	}

	if old == nil {
		old = state.NewManifest(cfg.Name)
	}
	if err := a.reconcileStorages(ctx, cfg, layout, old); err != nil {
		return err
	}
	if err := a.updateStorageWarnings(cfg, desired, layout, old); err != nil {
		return err
	}
	netState := oldNetwork(old)
	rebuild := old.HostNetworkHash != networkHash || netState == nil
	if !rebuild {
		if err := a.Network.Verify(ctx, netState); err != nil {
			rebuild = true
		}
	}

	// Stop removed/restarted nodes before changing their TAPs or disks.
	for name, node := range old.Nodes {
		d := desired[name]
		if d == nil || rebuild || d.StorageIdentityHash != node.StorageIdentityHash || d.RestartHash != node.RestartHash {
			stopErr := a.stopNode(ctx, cfg.Name, name, node)
			if err := state.Save(layout.ManifestPath, old); err != nil {
				return err
			}
			if stopErr != nil {
				return stopErr
			}
		}
	}
	if rebuild && netState != nil {
		stopErr := a.stopNamespaceHelpers(old)
		if err := state.Save(layout.ManifestPath, old); err != nil {
			return err
		}
		if stopErr != nil {
			return stopErr
		}
		if err := a.Network.Remove(ctx, netState); err != nil {
			return err
		}
		netState = nil
		old.NetworkState = nil
		old.HostNetworkHash = ""
		old.Links = map[string]*state.Link{}
		old.ManagementHostInterfaces = map[string]*state.ManagementHostInterface{}
		if err := state.Save(layout.ManifestPath, old); err != nil {
			return err
		}
	}

	// Remove nodes and explicitly removed disks only after all preflight checks.
	for name, node := range old.Nodes {
		d := desired[name]
		if d == nil {
			if err := a.removeRecordedNode(ctx, layout, name, node, old.Storages); err != nil {
				return err
			}
			delete(old.Nodes, name)
			if err := state.Save(layout.ManifestPath, old); err != nil {
				return err
			}
			continue
		}
		if d.StorageIdentityHash != node.StorageIdentityHash {
			if node.RootDisk != nil {
				if err := a.removeDisk(ctx, node.RootDisk); err != nil {
					return err
				}
				node.RootDisk = nil
				if err := state.Save(layout.ManifestPath, old); err != nil {
					return err
				}
			}
			for _, item := range []struct {
				path  *string
				clear func()
			}{{&node.SeedPath, func() { node.SeedDigest = ""; node.SeedPending = false }}, {&node.SerialLogPath, func() {}}, {&node.OVMFVarsPath, func() {}}} {
				if *item.path != "" {
					if err := os.Remove(*item.path); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
					*item.path = ""
					item.clear()
					if err := state.Save(layout.ManifestPath, old); err != nil {
						return err
					}
				}
			}
		}
		wanted := map[string]bool{}
		for _, disk := range d.AdditionalDisks {
			wanted[disk.Name] = true
		}
		for i := 0; i < len(node.AdditionalDisks); {
			disk := node.AdditionalDisks[i]
			if !wanted[disk.Name] {
				if err := a.removeDisk(ctx, &disk); err != nil {
					return err
				}
				node.AdditionalDisks = append(node.AdditionalDisks[:i], node.AdditionalDisks[i+1:]...)
				if err := state.Save(layout.ManifestPath, old); err != nil {
					return err
				}
				continue
			}
			i++
		}
	}
	if err := a.removeUndeclaredStorages(ctx, cfg, layout, old); err != nil {
		return err
	}

	if netState == nil {
		if checkpointNetwork, ok := a.Network.(interface {
			ApplyCheckpoint(context.Context, *network.Plan, func(*network.State) error) (*network.State, error)
		}); ok {
			netState, err = checkpointNetwork.ApplyCheckpoint(ctx, plan, func(partial *network.State) error {
				old.NetworkState, _ = json.Marshal(partial)
				copyNetworkSummary(old, partial)
				return state.Save(layout.ManifestPath, old)
			})
		} else {
			netState, err = a.Network.Apply(ctx, plan)
		}
		if err != nil {
			if netState != nil {
				old.NetworkState, _ = json.Marshal(netState)
				_ = state.Save(layout.ManifestPath, old)
			}
			return err
		}
		old.NetworkState, _ = json.Marshal(netState)
		copyNetworkSummary(old, netState)
		if err := state.Save(layout.ManifestPath, old); err != nil {
			return err
		}
	}
	if err := a.reconcileNamespaceHelpers(ctx, cfg, layout, old, netState); err != nil {
		old.NetworkState, _ = json.Marshal(netState)
		_ = state.Save(layout.ManifestPath, old)
		return err
	}
	if err := a.materializeNodes(ctx, cfg, layout, old, desired, netState, templates); err != nil {
		return err
	}
	old.ConfigurationHash, old.HostNetworkHash = configHash, networkHash
	old.NetworkState, _ = json.Marshal(netState)
	copyNetworkSummary(old, netState)
	if err := state.Save(layout.ManifestPath, old); err != nil {
		return err
	}
	run := &startRun{}
	for _, name := range cfg.VMNames() {
		startErr := a.startNode(ctx, cfg, layout, old, name, old.Nodes[name], netState, run)
		if err := state.Save(layout.ManifestPath, old); err != nil {
			return err
		}
		if startErr != nil {
			return startErr
		}
	}
	return state.Save(layout.ManifestPath, old)
}

func (a *App) validateTemplates(ctx context.Context, cfg *config.Config) (map[string]qemu.TemplateInfo, error) {
	used := map[string]bool{}
	for _, vm := range cfg.VMs {
		used[vm.Template] = true
	}
	out := map[string]qemu.TemplateInfo{}
	for name := range used {
		template, ok := cfg.LookupTemplate(name)
		if !ok {
			return nil, fmt.Errorf("template %s is missing", name)
		}
		info, err := a.Images.ValidateTemplate(ctx, template.Path)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		out[name] = info
	}
	return out, nil
}

func oldNetwork(m *state.Manifest) *network.State {
	n, _ := manifestNetwork(m)
	return n
}

func manifestNetwork(m *state.Manifest) (*network.State, error) {
	if m == nil || len(m.NetworkState) == 0 {
		return nil, nil
	}
	var n network.State
	if err := json.Unmarshal(m.NetworkState, &n); err != nil {
		return nil, fmt.Errorf("decode manifest network state: %w", err)
	}
	return &n, nil
}

func copyNetworkSummary(m *state.Manifest, n *network.State) {
	m.Links = map[string]*state.Link{}
	for name, link := range n.Links {
		v := &state.Link{GenerationID: link.GenerationID, GenerationStartedAt: link.GenerationStartedAt}
		for _, ep := range link.Endpoints {
			v.Endpoints = append(v.Endpoints, state.LinkEndpoint{Node: ep.Node, Interface: ep.Interface, IngressDevice: ep.IngressDevice, EgressDevice: ep.EgressDevice, ImpairmentDevice: ep.ImpairmentDevice})
		}
		m.Links[name] = v
	}
	m.DatapathVersion = n.DatapathVersion
	m.FabricNamespace = n.FabricNamespace
	m.ManagementHostNamespace = n.ManagementHostNamespace
	m.ManagementHostInterfaces = map[string]*state.ManagementHostInterface{}
	for name, h := range n.ManagementHostInterfaces {
		m.ManagementHostInterfaces[name] = &state.ManagementHostInterface{LogicalName: name, DeviceName: h.DeviceName, OwnerAlias: h.Alias, MTU: h.MTU, Addresses: h.Addresses, Link: h.Link}
	}
}

func (a *App) validateExisting(ctx context.Context, old *state.Manifest, desired map[string]*state.Node) error {
	if old == nil {
		return nil
	}
	for name, want := range desired {
		got := old.Nodes[name]
		if got == nil {
			continue
		}
		if got.RootDisk != nil && want.RootDisk != nil && got.RootDisk.Storage != "" && (got.RootDisk.Storage != want.RootDisk.Storage || got.RootDisk.StorageType != want.RootDisk.StorageType || got.RootDisk.Provisioning != want.RootDisk.Provisioning || got.RootDisk.VolumeGroup != want.RootDisk.VolumeGroup) {
			return fmt.Errorf("VM %s root disk storage or provisioning change is not allowed", name)
		}
		if want.Kind == "vm" && got.StorageIdentityHash == want.StorageIdentityHash && got.BootstrapHash != "" && got.BootstrapHash != want.BootstrapHash {
			return fmt.Errorf("VM %s bootstrap configuration is write-once; replace its template identity or run down", name)
		}
		var disks []*state.Disk
		if got.RootDisk != nil && got.StorageIdentityHash == want.StorageIdentityHash {
			disks = append(disks, got.RootDisk)
		}
		for i := range got.AdditionalDisks {
			disks = append(disks, &got.AdditionalDisks[i])
		}
		wantCap := map[string]uint64{}
		if want.RootDisk != nil {
			wantCap[want.RootDisk.Name] = want.RootDisk.CapacityBytes
		}
		for _, d := range want.AdditionalDisks {
			wantCap[d.Name] = d.CapacityBytes
		}
		for _, d := range disks {
			cap, ok := wantCap[d.Name]
			if !ok {
				continue
			}
			// A write-ahead disk record may precede the atomic image creation.
			// Materialization owns recreating that missing artifact.
			var desiredDisk *state.Disk
			if d.Name == "root" {
				desiredDisk = want.RootDisk
			} else {
				for i := range want.AdditionalDisks {
					if want.AdditionalDisks[i].Name == d.Name {
						desiredDisk = &want.AdditionalDisks[i]
					}
				}
			}
			if desiredDisk != nil && d.Storage != "" && (d.Storage != desiredDisk.Storage || d.StorageType != desiredDisk.StorageType || d.VolumeGroup != desiredDisk.VolumeGroup) {
				return fmt.Errorf("VM %s disk %s storage change is not allowed", name, d.Name)
			}
			exists, err := a.diskExists(ctx, d)
			if err != nil {
				return fmt.Errorf("inspect disk %s: %w", d.Name, err)
			}
			if !exists {
				continue
			}
			actual := d.CapacityBytes
			if d.StorageType == "file" || d.StorageType == "" {
				info, e := a.Images.Info(ctx, d.Path)
				if e != nil {
					return fmt.Errorf("inspect disk %s: %w", d.Path, e)
				}
				actual = info.VirtualSize
			} else {
				lvs, e := a.LVM.LVs(ctx, d.VolumeGroup)
				if e != nil {
					return e
				}
				lv, _ := storagepkg.FindLV(lvs, d.LVName)
				actual = lv.SizeBytes
			}
			if cap < actual {
				return fmt.Errorf("refusing to shrink disk %s from %d to %d bytes", d.Name, actual, cap)
			}
		}
	}
	return nil
}

func (a *App) removeRecordedNode(ctx context.Context, layout state.Layout, name string, n *state.Node, storages map[string]*state.Storage) error {
	if n == nil {
		return nil
	}
	manifest := state.NewManifest(layout.Topology)
	manifest.Nodes[name] = n
	manifest.Storages = storages
	if err := validateManifestPaths(layout, manifest); err != nil {
		return err
	}
	for _, d := range append([]state.Disk{func() state.Disk {
		if n.RootDisk != nil {
			return *n.RootDisk
		}
		return state.Disk{}
	}()}, n.AdditionalDisks...) {
		if d.Path != "" {
			if err := a.removeDisk(ctx, &d); err != nil {
				return err
			}
		}
	}
	for _, p := range []string{n.SeedPath, n.OVMFVarsPath, n.PIDFile, n.QMPPath, n.SerialSocketPath, n.SerialLogPath, n.GuestAgentPath} {
		if p != "" {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (a *App) start(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, args []string) error {
	if m == nil {
		return errors.New("topology is not up")
	}
	vgLocks, err := a.planStorages(ctx, cfg, layout, m)
	if err != nil {
		return err
	}
	defer func() {
		for i := len(vgLocks) - 1; i >= 0; i-- {
			_ = vgLocks[i].Close()
		}
	}()
	if err := a.verifyManifestStorage(ctx, m, false); err != nil {
		return err
	}
	netState := oldNetwork(m)
	if netState == nil {
		return errors.New("managed network is absent; run up")
	}
	if err := a.Network.Verify(ctx, netState); err != nil {
		return err
	}
	names, err := selectNodes(cfg, args)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := a.verifyNodeDisks(ctx, m.Nodes[name]); err != nil {
			return fmt.Errorf("verify node %s storage: %w", name, err)
		}
	}
	run := &startRun{}
	for _, name := range names {
		startErr := a.startNode(ctx, cfg, layout, m, name, m.Nodes[name], netState, run)
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if startErr != nil {
			return startErr
		}
	}
	return nil
}

func (a *App) stop(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, args []string) error {
	if m == nil {
		return errors.New("topology is not up")
	}
	names, err := selectNodes(cfg, args)
	if err != nil {
		return err
	}
	for _, name := range names {
		stopErr := a.stopNode(ctx, cfg.Name, name, m.Nodes[name])
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if stopErr != nil {
			return stopErr
		}
	}
	return nil
}

func (a *App) restart(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, args []string) error {
	if m == nil {
		return errors.New("topology is not up")
	}
	vgLocks, err := a.planStorages(ctx, cfg, layout, m)
	if err != nil {
		return err
	}
	defer func() {
		for i := len(vgLocks) - 1; i >= 0; i-- {
			_ = vgLocks[i].Close()
		}
	}()
	if err := a.verifyManifestStorage(ctx, m, false); err != nil {
		return err
	}
	netState := oldNetwork(m)
	if netState == nil {
		return errors.New("managed network is absent; run up")
	}
	if err := a.Network.Verify(ctx, netState); err != nil {
		return err
	}
	names, err := selectNodes(cfg, args)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := a.verifyNodeDisks(ctx, m.Nodes[name]); err != nil {
			return fmt.Errorf("verify node %s storage: %w", name, err)
		}
	}
	run := &startRun{}
	for _, name := range names {
		stopErr := a.stopNode(ctx, cfg.Name, name, m.Nodes[name])
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if stopErr != nil {
			return stopErr
		}
		startErr := a.startNode(ctx, cfg, layout, m, name, m.Nodes[name], netState, run)
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if startErr != nil {
			return startErr
		}
	}
	return nil
}

func selectNodes(cfg *config.Config, args []string) ([]string, error) {
	all := cfg.VMNames()
	if len(args) == 0 {
		return all, nil
	}
	valid := map[string]bool{}
	for _, n := range all {
		valid[n] = true
	}
	seen := map[string]bool{}
	out := []string{}
	for _, n := range args {
		if !valid[n] {
			return nil, fmt.Errorf("unknown VM %q", n)
		}
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	return out, nil
}

func (a *App) stopNode(ctx context.Context, topology, name string, n *state.Node) error {
	if n == nil {
		return nil
	}
	if n.PID > 0 && a.Processes.Alive(n.PID) {
		if n.NetworkNamespace != "" {
			same, err := a.Namespaces.SameProcess(n.PID, n.NetworkNamespace)
			if err != nil {
				return fmt.Errorf("verify QEMU PID %d namespace: %w", n.PID, err)
			}
			if !same {
				return fmt.Errorf("QEMU PID %d is outside expected fabric namespace %s", n.PID, n.NetworkNamespace)
			}
		}
		if err := a.Stopper.Stop(ctx, n.PID, topology, name, n.QMPPath); err != nil {
			return err
		}
	}
	n.PID, n.Process = 0, nil
	if n.CollectorProcess != nil && n.CollectorProcess.PID > 0 {
		if err := stopHelper(a.Processes, n.CollectorProcess); err != nil {
			return err
		}
	}
	n.CollectorProcess = nil
	_ = os.Remove(n.PIDFile)
	_ = os.Remove(n.QMPPath)
	_ = os.Remove(n.SerialSocketPath)
	_ = os.Remove(n.GuestAgentPath)
	return nil
}

// stopNodes stops all nodes at the same time and waits for all of them.
// Each stopNode call changes only its own node. The errors keep the sorted
// node order, so the joined error text is stable.
func (a *App) stopNodes(ctx context.Context, topology string, nodes map[string]*state.Node) error {
	names := slices.Sorted(maps.Keys(nodes))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			if err := a.stopNode(ctx, topology, name, nodes[name]); err != nil {
				errs[i] = fmt.Errorf("stop %s: %w", name, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func stopHelper(p qemu.ProcessOps, proc *state.Process) error {
	if !p.Alive(proc.PID) {
		return nil
	}
	args, err := p.CommandLine(proc.PID)
	if err != nil {
		return err
	}
	for _, arg := range args {
		if arg == proc.IdentityArg {
			return p.Signal(proc.PID, os.Interrupt)
		}
	}
	return fmt.Errorf("PID %d does not identify expected helper", proc.PID)
}

func (a *App) down(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest) error {
	if m == nil {
		return nil
	}
	vgLocks, err := a.planStorages(ctx, cfg, layout, m)
	if err != nil {
		return err
	}
	defer func() {
		for i := len(vgLocks) - 1; i >= 0; i-- {
			_ = vgLocks[i].Close()
		}
	}()
	if err := a.verifyManifestStorage(ctx, m, true); err != nil {
		return err
	}
	if err := verifyTopologyTree(layout, m); err != nil {
		return err
	}
	if err := a.stopNodes(ctx, cfg.Name, m.Nodes); err != nil {
		return errors.Join(err, state.Save(layout.ManifestPath, m))
	}
	stopHelpersErr := a.stopNamespaceHelpers(m)
	if err := state.Save(layout.ManifestPath, m); err != nil {
		return err
	}
	if stopHelpersErr != nil {
		return stopHelpersErr
	}
	if n := oldNetwork(m); n != nil {
		if err := a.Network.Remove(ctx, n); err != nil {
			return err
		}
	}
	for name, n := range m.Nodes {
		if err := a.removeRecordedNode(ctx, layout, name, n, m.Storages); err != nil {
			return err
		}
	}
	storageNames := make([]string, 0, len(m.Storages))
	for name := range m.Storages {
		storageNames = append(storageNames, name)
	}
	slices.Sort(storageNames)
	for _, name := range storageNames {
		if s := m.Storages[name]; s != nil {
			if err := a.removeStorage(ctx, cfg.Name, s); err != nil {
				return err
			}
		}
	}
	for _, h := range m.Helpers {
		if err := removeHelperFiles(h, true); err != nil {
			return err
		}
	}
	if err := os.Remove(layout.ManifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func RunSerialCollector(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) != 3 {
		fmt.Fprintln(stderr, "topolab: invalid serial collector invocation")
		return 2
	}
	c := observability.SerialCollector{SocketPath: args[0], LogPath: args[1], Node: args[2]}
	if err := c.Serve(ctx); err != nil {
		fmt.Fprintf(stderr, "topolab: serial collector: %v\n", err)
		return 1
	}
	return 0
}
