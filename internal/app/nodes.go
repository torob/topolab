package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/torob/topolab/internal/cloudinit"
	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
)

type bootstrapSnapshot struct {
	AuthorizedKeys []string          `json:"authorized_keys"`
	Network        cloudinit.Network `json:"network"`
}

func (a *App) desiredNodes(cfg *config.Config, layout state.Layout, templates map[string]qemu.TemplateInfo) (map[string]*state.Node, error) {
	out := map[string]*state.Node{}
	for _, vm := range cfg.VMs {
		name, tmpl := vm.Name, templates[vm.Template]
		dir, _ := layout.NodeDir(name)
		storage, _ := state.Hash(struct{ Path, Digest string }{tmpl.Path, tmpl.Digest})
		snapshot := bootstrapSnapshot{AuthorizedKeys: append([]string(nil), vm.SSHAuthorizedKeys...), Network: vmCloudNetwork(cfg, name, vm)}
		bootstrap, _ := state.Hash(snapshot)
		restart, _ := state.Hash(vmRestartSnapshot(cfg, name, vm))
		raw, _ := json.Marshal(snapshot)
		rootCapacity := tmpl.CapacityBytes
		if vm.RootDisk.SizeGiB != nil {
			var err error
			rootCapacity, err = qemu.GiBToBytes(*vm.RootDisk.SizeGiB)
			if err != nil {
				return nil, err
			}
			if rootCapacity < tmpl.CapacityBytes {
				return nil, fmt.Errorf("VM %s root disk capacity %d is smaller than template capacity %d", name, rootCapacity, tmpl.CapacityBytes)
			}
		}
		n := &state.Node{Kind: "vm", StorageIdentityHash: storage, RestartHash: restart, BootstrapHash: bootstrap, BootstrapSnapshot: raw, TemplatePath: tmpl.Path, TemplateDigest: tmpl.Digest,
			RootDisk: diskRecord("root", filepath.Join(dir, "root.qcow2"), vm.RootDisk.Bus, rootCapacity), SeedPath: filepath.Join(dir, "seed.iso"), OVMFVarsPath: filepath.Join(dir, "OVMF_VARS.fd"), PIDFile: filepath.Join(dir, "qemu.pid"), SerialLogPath: filepath.Join(layout.LogsDir, name+".jsonl")}
		n.QMPPath, _ = layout.SocketPath(name, "qmp")
		n.SerialSocketPath, _ = layout.SocketPath(name, "serial")
		if vm.GuestAgent {
			n.GuestAgentPath, _ = layout.SocketPath(name, "guest-agent")
		}
		if vm.MemoryBalloon.Enabled {
			target := uint64(*vm.MemoryBalloon.TargetMB)
			n.BalloonTargetMB = &target
		}
		for _, d := range vm.AdditionalDisks {
			dn := d.Name
			size, err := qemu.GiBToBytes(d.SizeGiB)
			if err != nil {
				return nil, err
			}
			n.AdditionalDisks = append(n.AdditionalDisks, *diskRecord(dn, filepath.Join(dir, "disk-"+state.ShortID(dn, 12)+".qcow2"), d.Bus, size))
		}
		out[name] = n
	}
	return out, nil
}

func diskRecord(name, path, bus string, capacity uint64) *state.Disk {
	id := qemu.ID("disk", name)
	return &state.Disk{Name: name, Path: path, Bus: bus, CapacityBytes: capacity, BlockNode: "blk-" + id, Serial: qemu.DiskSerial(name)}
}

// validateNodePlans exhausts deterministic QEMU allocation and argument
// validation before reconciliation is allowed to stop a process or remove a
// resource. TAP names are predictable from the already validated network plan.
func (a *App) validateNodePlans(cfg *config.Config, desired map[string]*state.Node, netCfg network.Config) error {
	netState := &network.State{TAPs: map[string]network.TAPState{}}
	for _, link := range netCfg.Links {
		for _, ep := range link.Endpoints {
			if ep.Kind != network.EndpointVM {
				continue
			}
			name := network.DeviceName("tlt", netCfg.Topology, ep.Node, ep.Interface)
			netState.TAPs[ep.Node+":"+ep.Interface] = network.TAPState{Node: ep.Node, Interface: ep.Interface, Name: name, Queues: ep.Queues}
		}
	}
	for _, name := range cfg.VMNames() {
		spec, err := qemuSpec(cfg, name, desired[name], netState)
		if err != nil {
			return fmt.Errorf("preflight QEMU node %s: %w", name, err)
		}
		fd := 3
		for i := range spec.NICs {
			if spec.NICs[i].Queues > 1 {
				spec.NICs[i].TAPFDs = make([]int, spec.NICs[i].Queues)
				for q := range spec.NICs[i].TAPFDs {
					spec.NICs[i].TAPFDs[q] = fd
					fd++
				}
			}
		}
		if _, err := a.QEMUBuilder.Build(spec); err != nil {
			return fmt.Errorf("preflight QEMU node %s: %w", name, err)
		}
	}
	return nil
}

func vmRestartSnapshot(cfg *config.Config, name string, vm *config.VM) any {
	type nic struct {
		Name, Model string
		Queues      int
	}
	type disk struct{ Name, Bus string }
	r := struct {
		CPU                 *config.CPU
		Memory              int
		Balloon, GuestAgent bool
		RootBus             string
		Boot                config.Boot
		NICs                []nic
		Disks               []disk
	}{vm.CPU, vm.MemoryMB, vm.MemoryBalloon.Enabled, vm.GuestAgent, vm.RootDisk.Bus, vm.Boot, nil, nil}
	for _, i := range vm.Interfaces {
		r.NICs = append(r.NICs, nic{i.Name, i.Model, i.Queues})
	}
	for _, d := range vm.AdditionalDisks {
		r.Disks = append(r.Disks, disk{d.Name, d.Bus})
	}
	return r
}

func (a *App) materializeNodes(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, desired map[string]*state.Node, netState *network.State, templates map[string]qemu.TemplateInfo) error {
	if m.Nodes == nil {
		m.Nodes = map[string]*state.Node{}
	}
	for _, name := range cfg.VMNames() {
		want, old := desired[name], m.Nodes[name]
		if old == nil {
			old = &state.Node{}
		}
		storageNew := old.RootDisk == nil || old.StorageIdentityHash != want.StorageIdentityHash
		seedPending := old.SeedPending
		existing := map[string]state.Disk{}
		for _, d := range old.AdditionalDisks {
			existing[d.Name] = d
		}
		if !storageNew && old.RootDisk != nil {
			if old.RootDisk.Path != "" {
				want.RootDisk.Path = old.RootDisk.Path
			}
			if old.OVMFVarsPath != "" {
				want.OVMFVarsPath = old.OVMFVarsPath
			}
			if old.SerialLogPath != "" {
				want.SerialLogPath = old.SerialLogPath
			}
		}
		for i := range want.AdditionalDisks {
			if have, ok := existing[want.AdditionalDisks[i].Name]; ok {
				if have.Path != "" {
					want.AdditionalDisks[i].Path = have.Path
				}
			}
		}
		want.PID, want.Process, want.CollectorProcess = old.PID, old.Process, old.CollectorProcess
		want.NetworkNamespace = netState.FabricNamespace
		want.TAPs = map[string]state.TAP{}
		for _, tap := range netState.TAPs {
			if tap.Node == name {
				want.TAPs[tap.Interface] = state.TAP{Name: tap.Name, Queues: tap.Queues, OwnerAlias: tap.Alias, Datapath: "userspace"}
			}
		}
		want.SeedPending = seedPending || storageNew || fileMissing(want.SeedPath)
		m.Nodes[name] = want
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}

		if fileMissing(want.RootDisk.Path) {
			if err := a.Images.CreateOverlay(ctx, want.RootDisk.Path, qemu.TemplateInfo{Path: want.TemplatePath, Digest: want.TemplateDigest, CapacityBytes: templates[templateName(cfg, name)].CapacityBytes}, want.RootDisk.CapacityBytes); err != nil {
				return fmt.Errorf("create %s root disk: %w", name, err)
			}
			if err := state.Save(layout.ManifestPath, m); err != nil {
				return err
			}
		}
		if fileMissing(want.OVMFVarsPath) {
			if err := qemu.CopyFileAtomic(ovmfVars, want.OVMFVarsPath, 0o600); err != nil {
				return fmt.Errorf("create %s OVMF variables: %w", name, err)
			}
			if err := state.Save(layout.ManifestPath, m); err != nil {
				return err
			}
		}
		if !storageNew && old.RootDisk != nil {
			if err := a.growDisk(ctx, old, old.RootDisk, want.RootDisk.CapacityBytes); err != nil {
				return err
			}
			if err := state.Save(layout.ManifestPath, m); err != nil {
				return err
			}
		}
		for i := range want.AdditionalDisks {
			d := &want.AdditionalDisks[i]
			if fileMissing(d.Path) {
				if err := a.Images.CreateDataDisk(ctx, d.Path, d.CapacityBytes); err != nil {
					return fmt.Errorf("create %s disk %s: %w", name, d.Name, err)
				}
			} else {
				have := *d
				if prior, ok := existing[d.Name]; ok {
					have = prior
				}
				if err := a.growDisk(ctx, old, &have, d.CapacityBytes); err != nil {
					return err
				}
			}
			if err := state.Save(layout.ManifestPath, m); err != nil {
				return err
			}
		}

		seed, err := seedFor(cfg, name, want)
		if err != nil {
			return err
		}
		if want.SeedPending {
			missing := fileMissing(want.SeedPath)
			// VM seeds are immutable. An existing seed with a pending marker means
			// the prior atomic build completed before its clearing checkpoint.
			if missing {
				if err := a.Seeds.Build(ctx, want.SeedPath, seed); err != nil {
					return fmt.Errorf("build %s seed: %w", name, err)
				}
			}
		}
		want.SeedDigest = seed.Digest()
		want.SeedPending = false
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) growDisk(ctx context.Context, node *state.Node, disk *state.Disk, desired uint64) error {
	info, err := a.Images.Info(ctx, disk.Path)
	if err != nil {
		return err
	}
	growth := qemu.Growth{Path: disk.Path, BlockNode: disk.BlockNode, Actual: info.VirtualSize, Desired: desired}
	if desired == info.VirtualSize {
		return nil
	}
	if node.PID > 0 && a.Processes.Alive(node.PID) {
		client := a.OpenQMP(node.QMPPath)
		defer client.Close()
		return a.Images.ResizeOnline(ctx, client, growth)
	}
	return a.Images.ResizeOffline(ctx, growth)
}

func templateName(cfg *config.Config, node string) string {
	vm, _ := cfg.LookupVM(node)
	return vm.Template
}

func rootDiskNeedsCreate(old, want *state.Node) bool {
	if old == nil || old.RootDisk == nil || old.StorageIdentityHash != want.StorageIdentityHash {
		return true
	}
	return fileMissing(old.RootDisk.Path)
}

func fileMissing(path string) bool { _, err := os.Stat(path); return errors.Is(err, os.ErrNotExist) }

func seedFor(cfg *config.Config, name string, n *state.Node) (cloudinit.Seed, error) {
	var snapshot bootstrapSnapshot
	if err := json.Unmarshal(n.BootstrapSnapshot, &snapshot); err != nil {
		return cloudinit.Seed{}, err
	}
	user := cloudinit.UserData{Hostname: name, AuthorizedKeys: snapshot.AuthorizedKeys}
	return cloudinit.Render(cloudinit.InstanceID(cfg.Name, name, n.StorageIdentityHash, n.BootstrapHash), user, snapshot.Network)
}

func (a *App) startNode(ctx context.Context, cfg *config.Config, layout state.Layout, manifest *state.Manifest, name string, n *state.Node, netState *network.State) error {
	if n == nil {
		return fmt.Errorf("node %s has no state; run up", name)
	}
	if n.PID > 0 && a.Processes.Alive(n.PID) {
		if err := qemu.VerifyProcessIdentity(a.Processes, n.PID, "topolab:"+cfg.Name+":"+name); err != nil {
			return err
		}
		if same, err := a.Namespaces.SameProcess(n.PID, netState.FabricNamespace); err != nil {
			return fmt.Errorf("verify QEMU PID %d fabric namespace: %w", n.PID, err)
		} else if !same {
			return fmt.Errorf("QEMU PID %d is not in fabric namespace %s", n.PID, netState.FabricNamespace)
		}
		if err := a.waitNodeReady(ctx, name, n, nil); err != nil {
			return err
		}
		if n.BalloonTargetMB != nil {
			client := a.OpenQMP(n.QMPPath)
			err := client.Balloon(ctx, *n.BalloonTargetMB<<20)
			_ = client.Close()
			if err != nil {
				fmt.Fprintf(a.Stderr, "warning: node %s balloon target is pending: %v\n", name, err)
			}
		}
		return nil
	}
	checkpoint := func() error { return state.Save(layout.ManifestPath, manifest) }
	if err := a.startCollector(name, cfg.Name, n, checkpoint); err != nil {
		return err
	}
	spec, err := qemuSpec(cfg, name, n, netState)
	if err != nil {
		return err
	}
	applyVhostSelection(spec.NICs, n, a.VhostAvailable())
	files, cleanup, err := prepareNICFiles(a.Namespaces, netState.FabricNamespace, spec.NICs)
	if err != nil {
		return err
	}
	defer cleanup()
	argv, err := a.QEMUBuilder.Build(spec)
	if err != nil {
		return err
	}
	namespacedArgv := append([]string{"ip", "netns", "exec", netState.FabricNamespace}, argv...)
	started, err := a.StartProcess(namespacedArgv, files)
	if err != nil {
		return fmt.Errorf("start QEMU node %s: %w", name, err)
	}
	defer started.DiscardStartupStderr()
	n.PID = started.PID
	n.NetworkNamespace = netState.FabricNamespace
	n.Process = &state.Process{PID: started.PID, StartedAt: a.Now().UTC(), Executable: argv[0], IdentityArg: "topolab:" + cfg.Name + ":" + name}
	if err := checkpoint(); err != nil {
		stopErr := a.stopNode(ctx, cfg.Name, name, n)
		if stopErr != nil {
			return fmt.Errorf("checkpoint QEMU node %s: %v (rollback: %w)", name, err, stopErr)
		}
		return fmt.Errorf("checkpoint QEMU node %s: %w", name, err)
	}
	if err := a.waitNodeReady(ctx, name, n, started); err != nil {
		return err
	}
	if n.BalloonTargetMB != nil {
		client := a.OpenQMP(n.QMPPath)
		bctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := client.Balloon(bctx, *n.BalloonTargetMB<<20)
		cancel()
		_ = client.Close()
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: node %s balloon target is pending: %v\n", name, err)
		}
	}
	return nil
}

func vhostNetAvailable() bool {
	info, err := os.Stat("/dev/vhost-net")
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	f, err := os.OpenFile("/dev/vhost-net", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

func applyVhostSelection(nics []qemu.NICSpec, node *state.Node, available bool) {
	for i := range nics {
		nics[i].Vhost = nics[i].Model == "virtio" && available
		if tap, ok := node.TAPs[nics[i].Name]; ok {
			if nics[i].Vhost {
				tap.Datapath = "vhost-net"
			} else {
				tap.Datapath = "userspace"
			}
			node.TAPs[nics[i].Name] = tap
		}
	}
}

func (a *App) startCollector(name, topology string, n *state.Node, checkpoint func() error) error {
	if n.CollectorProcess != nil && a.Processes.Alive(n.CollectorProcess.PID) {
		return verifyHelperIdentity(a.Processes, n.CollectorProcess)
	}
	exe, err := a.Executable()
	if err != nil {
		return err
	}
	started, err := a.StartProcess([]string{exe, "__serial-collector", n.SerialSocketPath, n.SerialLogPath, name}, nil)
	if err != nil {
		return fmt.Errorf("start serial collector for %s: %w", name, err)
	}
	defer started.DiscardStartupStderr()
	n.CollectorProcess = &state.Process{PID: started.PID, StartedAt: a.Now().UTC(), Executable: exe, IdentityArg: n.SerialSocketPath}
	if err := checkpoint(); err != nil {
		stopErr := stopHelper(a.Processes, n.CollectorProcess)
		n.CollectorProcess = nil
		if stopErr != nil {
			return fmt.Errorf("checkpoint serial collector for %s: %v (rollback: %w)", name, err, stopErr)
		}
		return fmt.Errorf("checkpoint serial collector for %s: %w", name, err)
	}
	if started.Cmd == nil {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(n.SerialSocketPath); err == nil && st.Mode()&os.ModeSocket != 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stderr := started.StartupStderr(); stderr != "" {
		return fmt.Errorf("serial collector for %s did not create its socket: %s", name, stderr)
	}
	return fmt.Errorf("serial collector for %s did not create its socket", name)
}

func (a *App) waitNodeReady(ctx context.Context, name string, n *state.Node, started *qemu.StartedProcess) error {
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if !a.Processes.Alive(n.PID) {
			if stderr := started.StartupStderr(); stderr != "" {
				return fmt.Errorf("QEMU node %s exited before QMP became ready: %s", name, stderr)
			}
			return fmt.Errorf("QEMU node %s exited before QMP became ready", name)
		}
		attempt, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		client := a.OpenQMP(n.QMPPath)
		_, lastErr = client.QueryStatus(attempt)
		_ = client.Close()
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("QEMU node %s did not become ready: %w", name, lastErr)
}

func qemuSpec(cfg *config.Config, name string, n *state.Node, netState *network.State) (qemu.NodeSpec, error) {
	s := qemu.NodeSpec{Topology: cfg.Name, Name: name, Firmware: qemu.Firmware{Code: ovmfCode, Vars: n.OVMFVarsPath}, QMPPath: n.QMPPath, PIDFile: n.PIDFile, SerialSocket: n.SerialSocketPath, GuestAgentSocket: n.GuestAgentPath, SeedPath: n.SeedPath}
	vm, ok := cfg.LookupVM(name)
	if !ok {
		return s, fmt.Errorf("unknown VM %q", name)
	}
	s.CPU = qemu.CPU{Model: vm.CPU.Model, Sockets: *vm.CPU.Sockets, Cores: *vm.CPU.Cores, Threads: *vm.CPU.Threads}
	s.Memory = qemu.Memory{MiB: uint64(vm.MemoryMB), Balloon: vm.MemoryBalloon.Enabled}
	s.Boot = qemu.BootSpec{Order: vm.Boot.Order, Menu: vm.Boot.Menu, Strict: vm.Boot.Strict}
	for _, v := range vm.Interfaces {
		in, tap := v.Name, netState.TAPs[name+":"+v.Name]
		if tap.Name == "" {
			return s, fmt.Errorf("node %s interface %s has no managed TAP", name, in)
		}
		s.NICs = append(s.NICs, qemu.NICSpec{Name: in, Model: v.Model, MAC: v.MAC, TAP: tap.Name, Queues: v.Queues})
	}
	s.Disks = append(s.Disks, qemu.DiskSpec{Name: "root", Path: n.RootDisk.Path, Bus: n.RootDisk.Bus, BlockNode: n.RootDisk.BlockNode, Serial: n.RootDisk.Serial, Boot: true})
	for _, d := range n.AdditionalDisks {
		s.Disks = append(s.Disks, qemu.DiskSpec{Name: d.Name, Path: d.Path, Bus: d.Bus, BlockNode: d.BlockNode, Serial: d.Serial})
	}
	return s, nil
}
