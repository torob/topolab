package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/torob/topolab/internal/network"
	"github.com/torob/topolab/internal/state"
	storagepkg "github.com/torob/topolab/internal/storage"
)

// verifyTopologyTree prevents down from recursively or implicitly deleting an
// unrecorded file. Every non-directory below the topology directory must be a
// layout file or a path recorded in the manifest.
func verifyTopologyTree(layout state.Layout, m *state.Manifest) error {
	if err := validateManifestPaths(layout, m); err != nil {
		return err
	}
	allowed := map[string]bool{layout.ManifestPath: true}
	for _, n := range m.Nodes {
		if n.RootDisk != nil {
			allowed[n.RootDisk.Path] = true
		}
		for _, d := range n.AdditionalDisks {
			allowed[d.Path] = true
		}
		for _, p := range []string{n.SeedPath, n.OVMFVarsPath, n.PIDFile, n.QMPPath, n.SerialSocketPath, n.SerialLogPath, n.GuestAgentPath} {
			if p != "" {
				allowed[p] = true
			}
		}
	}
	for _, h := range m.Helpers {
		for _, p := range []string{h.ConfigPath, h.PIDPath, h.LeasePath} {
			if p != "" {
				allowed[p] = true
			}
		}
	}
	for _, snapshot := range m.Snapshots {
		if snapshot == nil {
			continue
		}
		for _, vm := range snapshot.VMs {
			if vm != nil && vm.OVMFVarsPath != "" {
				allowed[vm.OVMFVarsPath] = true
			}
		}
	}
	err := filepath.WalkDir(layout.TopologyDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing topology state symlink %s", path)
			}
			return nil
		}
		if !allowed[path] {
			return fmt.Errorf("refusing to remove unrecorded topology state %s", path)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func validateManifestPaths(layout state.Layout, m *state.Manifest) error {
	if m == nil {
		return nil
	}
	for name, storage := range m.Storages {
		if storage == nil || name == "" || storage.Name != name || strings.ContainsAny(name, "/\x00") {
			return fmt.Errorf("manifest storage %q is invalid", name)
		}
		switch storage.Type {
		case "file":
			if storage.VolumeGroup != "" || storage.PoolName != "" {
				return fmt.Errorf("manifest file storage %q contains LVM identity", name)
			}
		case "lvm-thin", "lvm-thick":
			if !storagepkg.ValidName(storage.VolumeGroup) {
				return fmt.Errorf("manifest storage %q has unsafe volume group", name)
			}
			if storage.PoolName != "" && !storagepkg.ValidName(storage.PoolName) {
				return fmt.Errorf("manifest storage %q has unsafe pool name", name)
			}
			for _, base := range storage.Bases {
				if !storagepkg.ValidName(base.Name) {
					return fmt.Errorf("manifest storage %q has unsafe template base", name)
				}
			}
		default:
			return fmt.Errorf("manifest storage %q has invalid type %q", name, storage.Type)
		}
	}
	for name, n := range m.Nodes {
		if n == nil {
			return fmt.Errorf("manifest node %q is null", name)
		}
		dir, err := layout.NodeDir(name)
		if err != nil {
			return fmt.Errorf("manifest node %q: %w", name, err)
		}
		expected := map[string]string{
			"seed": n.SeedPath, "OVMF variables": n.OVMFVarsPath,
			"PID file": n.PIDFile, "serial log": n.SerialLogPath,
		}
		want := map[string]string{
			"seed": filepath.Join(dir, "seed.iso"), "OVMF variables": filepath.Join(dir, "OVMF_VARS.fd"),
			"PID file": filepath.Join(dir, "qemu.pid"), "serial log": filepath.Join(layout.LogsDir, name+".jsonl"),
		}
		qmp, _ := layout.SocketPath(name, "qmp")
		serial, _ := layout.SocketPath(name, "serial")
		expected["QMP socket"], want["QMP socket"] = n.QMPPath, qmp
		expected["serial socket"], want["serial socket"] = n.SerialSocketPath, serial
		if n.GuestAgentPath != "" {
			ga, _ := layout.SocketPath(name, "guest-agent")
			expected["guest-agent socket"], want["guest-agent socket"] = n.GuestAgentPath, ga
		}
		for kind, got := range expected {
			if got != "" && filepath.Clean(got) != want[kind] {
				return fmt.Errorf("manifest node %s has unsafe %s path %s", name, kind, got)
			}
			if got != "" {
				if err := rejectSymlinkComponents(layout.WorkingDir, got); err != nil {
					return fmt.Errorf("manifest node %s %s: %w", name, kind, err)
				}
			}
		}
		if n.RootDisk != nil && (n.RootDisk.StorageType == "file" || n.RootDisk.StorageType == "") && filepath.Clean(n.RootDisk.Path) != filepath.Join(dir, "root.qcow2") {
			return fmt.Errorf("manifest node %s has unsafe root disk path %s", name, n.RootDisk.Path)
		}
		if n.RootDisk != nil && (n.RootDisk.StorageType == "file" || n.RootDisk.StorageType == "") {
			if err := rejectSymlinkComponents(layout.WorkingDir, n.RootDisk.Path); err != nil {
				return fmt.Errorf("manifest node %s root disk: %w", name, err)
			}
		}
		if n.RootDisk != nil {
			if err := validateManifestDiskBackend(m, name, n.RootDisk, true); err != nil {
				return fmt.Errorf("manifest node %s root disk: %w", name, err)
			}
		}
		seenDisks := map[string]bool{}
		for _, disk := range n.AdditionalDisks {
			if disk.Name == "" || seenDisks[disk.Name] {
				return fmt.Errorf("manifest node %s has invalid additional disk name %q", name, disk.Name)
			}
			seenDisks[disk.Name] = true
			if err := validateManifestDiskBackend(m, name, &disk, false); err != nil {
				return fmt.Errorf("manifest node %s disk %s: %w", name, disk.Name, err)
			}
			expectedPath := filepath.Join(dir, "disk-"+state.ShortID(disk.Name, 12)+".qcow2")
			if (disk.StorageType == "file" || disk.StorageType == "") && filepath.Clean(disk.Path) != expectedPath {
				return fmt.Errorf("manifest node %s has unsafe disk path %s", name, disk.Path)
			}
			if disk.StorageType == "file" || disk.StorageType == "" {
				if err := rejectSymlinkComponents(layout.WorkingDir, disk.Path); err != nil {
					return fmt.Errorf("manifest node %s disk %s: %w", name, disk.Name, err)
				}
			}
		}
	}
	if err := validateManifestSnapshots(layout, m); err != nil {
		return err
	}
	for key, h := range m.Helpers {
		if h == nil || h.Kind != "dnsmasq" || key != h.Router+":"+h.Interface {
			return fmt.Errorf("manifest helper %q is invalid", key)
		}
		if _, err := layout.NodeDir(h.Router); err != nil {
			return fmt.Errorf("manifest helper %q: %w", key, err)
		}
		base := filepath.Join(layout.NodesDir, h.Router, "dnsmasq-"+state.ShortID(h.Interface, 10))
		for kind, pair := range map[string][2]string{
			"config": {h.ConfigPath, base + ".conf"},
			"PID":    {h.PIDPath, base + ".pid"},
			"lease":  {h.LeasePath, base + ".leases"},
		} {
			if filepath.Clean(pair[0]) != pair[1] {
				return fmt.Errorf("manifest helper %s has unsafe %s path %s", key, kind, pair[0])
			}
			if err := rejectSymlinkComponents(layout.WorkingDir, pair[0]); err != nil {
				return fmt.Errorf("manifest helper %s %s: %w", key, kind, err)
			}
		}
	}
	return nil
}

func validateManifestDiskBackend(m *state.Manifest, node string, d *state.Disk, root bool) error {
	if d == nil {
		return nil
	}
	switch d.StorageType {
	case "", "file":
		if d.Format != "" && d.Format != "qcow2" {
			return fmt.Errorf("file disk has format %q", d.Format)
		}
		if d.StorageType == "file" {
			s := m.Storages[d.Storage]
			if s == nil || s.Type != "file" || (root && d.Provisioning != s.RootProvisioning) || (!root && d.Provisioning != "") {
				return fmt.Errorf("file disk has inconsistent storage or provisioning identity")
			}
		}
	case "lvm-thin", "lvm-thick":
		s := m.Storages[d.Storage]
		if s == nil || s.Type != d.StorageType || s.VolumeGroup != d.VolumeGroup {
			return fmt.Errorf("LVM disk references inconsistent storage identity")
		}
		if d.Storage == "" || d.Format != "raw" || !storagepkg.ValidName(d.VolumeGroup) || d.LVName != storagepkg.DiskName(m.Topology, node, d.Name) || d.Path != storagepkg.DevicePath(d.VolumeGroup, d.LVName) {
			return fmt.Errorf("LVM disk has inconsistent path, identity, or format")
		}
		expectedTags := storagepkg.Tags(m.Topology, d.Storage, "disk")
		if len(d.OwnershipTags) != len(expectedTags) || !storagepkg.HasTags(d.OwnershipTags, expectedTags) {
			return fmt.Errorf("LVM disk has inconsistent ownership tags")
		}
		if d.StorageType == "lvm-thin" {
			if d.PoolName != storagepkg.PoolName(m.Topology, d.Storage) || (s.PoolName != "" && d.PoolName != s.PoolName) {
				return fmt.Errorf("thin disk has inconsistent pool identity")
			}
		} else if d.PoolName != "" || d.PoolUUID != "" || d.OriginName != "" || d.OriginUUID != "" {
			return fmt.Errorf("thick disk contains thin identity")
		}
		if root {
			if d.Provisioning != s.RootProvisioning {
				return fmt.Errorf("root disk has inconsistent provisioning identity")
			}
		} else if d.Provisioning != "" {
			return fmt.Errorf("data disk contains root provisioning identity")
		}
		if d.StagingName != "" && d.StagingName != storagepkg.StagingName(m.Topology, node, d.Name) {
			return fmt.Errorf("LVM disk has inconsistent staging identity")
		}
		if d.OriginName != "" {
			matched := false
			for _, base := range s.Bases {
				if base.Name == d.OriginName && (d.OriginUUID == "" || base.UUID == d.OriginUUID) {
					matched = true
				}
			}
			// A restored thin disk is a thin snapshot of its snapshot LV.
			if !matched && !snapshotOrigin(m, node, d) {
				return fmt.Errorf("thin snapshot has inconsistent origin identity")
			}
		}
	default:
		return fmt.Errorf("disk has unsupported storage type %q", d.StorageType)
	}
	return nil
}

// validateManifestSnapshots checks that every recorded snapshot path and LV
// identity is the deterministic one, before a command removes or changes it.
func validateManifestSnapshots(layout state.Layout, m *state.Manifest) error {
	for name, snapshot := range m.Snapshots {
		if snapshot == nil || snapshot.Name != name || !state.ValidSnapshotName(name) {
			return fmt.Errorf("manifest snapshot %q is invalid", name)
		}
		switch snapshot.Pending {
		case "", state.SnapshotPendingCreate, state.SnapshotPendingRestore:
		default:
			return fmt.Errorf("manifest snapshot %s has invalid pending operation %q", name, snapshot.Pending)
		}
		if r := snapshot.Restore; r != nil {
			if r.Phase != state.SnapshotRestorePhaseRestore && r.Phase != state.SnapshotRestorePhaseStart {
				return fmt.Errorf("manifest snapshot %s has invalid restore phase %q", name, r.Phase)
			}
			for _, vmName := range append(append(append([]string(nil), r.VMs...), r.Running...), r.Restored...) {
				if snapshot.VMs[vmName] == nil {
					return fmt.Errorf("manifest snapshot %s restores unknown VM %q", name, vmName)
				}
			}
		}
		for vmName, vm := range snapshot.VMs {
			if vm == nil {
				return fmt.Errorf("manifest snapshot %s VM %q is null", name, vmName)
			}
			if vm.Pending != "" && vm.Pending != state.SnapshotPendingDelete {
				return fmt.Errorf("manifest snapshot %s VM %s has invalid pending operation %q", name, vmName, vm.Pending)
			}
			varsPath, err := layout.SnapshotVarsPath(name, vmName)
			if err != nil {
				return fmt.Errorf("manifest snapshot %s: %w", name, err)
			}
			if filepath.Clean(vm.OVMFVarsPath) != varsPath {
				return fmt.Errorf("manifest snapshot %s VM %s has unsafe OVMF variables path %s", name, vmName, vm.OVMFVarsPath)
			}
			if err := rejectSymlinkComponents(layout.WorkingDir, vm.OVMFVarsPath); err != nil {
				return fmt.Errorf("manifest snapshot %s VM %s: %w", name, vmName, err)
			}
			seen := map[string]bool{}
			for _, sd := range vm.Disks {
				if sd.Name == "" || seen[sd.Name] {
					return fmt.Errorf("manifest snapshot %s VM %s has invalid disk name %q", name, vmName, sd.Name)
				}
				seen[sd.Name] = true
				switch sd.StorageType {
				case "", "file":
					if sd.VolumeGroup != "" || sd.LVName != "" || sd.LVUUID != "" || sd.PoolName != "" || len(sd.OwnershipTags) != 0 {
						return fmt.Errorf("manifest snapshot %s VM %s disk %s contains LVM identity", name, vmName, sd.Name)
					}
				case "lvm-thin":
					expectedTags := storagepkg.Tags(m.Topology, sd.Storage, "snapshot")
					if !storagepkg.ValidName(sd.VolumeGroup) || sd.LVName != storagepkg.SnapshotLVName(m.Topology, vmName, sd.Name, name) || sd.PoolName != storagepkg.PoolName(m.Topology, sd.Storage) || len(sd.OwnershipTags) != len(expectedTags) || !storagepkg.HasTags(sd.OwnershipTags, expectedTags) {
						return fmt.Errorf("manifest snapshot %s VM %s disk %s has inconsistent LV identity", name, vmName, sd.Name)
					}
				default:
					return fmt.Errorf("manifest snapshot %s VM %s disk %s has unsupported storage type %q", name, vmName, sd.Name, sd.StorageType)
				}
			}
		}
	}
	return nil
}

func rejectSymlinkComponents(base, path string) error {
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes working directory: %s", path)
	}
	current := base
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path contains symlink %s", current)
		}
	}
	return nil
}

func validateManifestNetwork(layout state.Layout, n *network.State) error {
	if n == nil {
		return nil
	}
	if n.Topology != layout.Topology {
		return fmt.Errorf("manifest network belongs to topology %q", n.Topology)
	}
	if n.SharedDir != "" && filepath.Clean(n.SharedDir) != layout.SharedDir {
		return fmt.Errorf("manifest network has unsafe shared directory %s", n.SharedDir)
	}
	wantAliasPrefix := "topolab:v1:" + layout.Topology + ":"
	for _, resource := range n.Resources {
		if (!strings.HasPrefix(resource.Name, "tl") && !strings.HasPrefix(resource.Name, "topolab-ns")) || strings.ContainsAny(resource.Name, "/\x00") {
			return fmt.Errorf("manifest network has unsafe resource name %q", resource.Name)
		}
		if resource.Alias != "" && !strings.HasPrefix(resource.Alias, wantAliasPrefix) {
			return fmt.Errorf("manifest network resource %s has unsafe ownership alias %q", resource.Name, resource.Alias)
		}
	}
	if n.RootNFTTable != "" && n.RootNFTTable != network.DeviceName("tl", layout.Topology, "nft") {
		return fmt.Errorf("manifest network has unsafe root nft table %q", n.RootNFTTable)
	}
	if n.FabricNamespace != "" && n.FabricNamespace != network.FabricNamespaceName(layout.Topology) {
		return fmt.Errorf("manifest network has unsafe fabric namespace %q", n.FabricNamespace)
	}
	if n.ManagementHostNamespace != "" && n.ManagementHostNamespace != network.ManagementHostNamespaceName(layout.Topology) {
		return fmt.Errorf("manifest network has unsafe management-host namespace %q", n.ManagementHostNamespace)
	}
	for _, rule := range n.FirewallRules {
		if (rule.Family != "ip" && rule.Family != "inet") || rule.Table == "" || rule.Chain == "" || strings.ContainsAny(rule.Table+rule.Chain, "/\x00") {
			return fmt.Errorf("manifest network has unsafe host firewall attachment %q/%q/%q", rule.Family, rule.Table, rule.Chain)
		}
		if !strings.HasPrefix(rule.Comment, wantAliasPrefix) {
			return fmt.Errorf("manifest network host firewall rule has unsafe ownership comment %q", rule.Comment)
		}
	}
	checkDevice := func(device string) error {
		if device == "" || !strings.HasPrefix(device, "tl") || strings.ContainsAny(device, "/\x00") {
			return fmt.Errorf("manifest network has unsafe datapath device %q", device)
		}
		return nil
	}
	for _, link := range n.Links {
		if err := checkDevice(link.Bridge); err != nil {
			return err
		}
		for _, endpoint := range link.Endpoints {
			for _, device := range []string{endpoint.IngressDevice, endpoint.EgressDevice, endpoint.ImpairmentDevice} {
				if err := checkDevice(device); err != nil {
					return err
				}
			}
		}
	}
	for _, tap := range n.TAPs {
		if err := checkDevice(tap.Name); err != nil {
			return err
		}
	}
	for _, iface := range n.ManagementHostInterfaces {
		if err := checkDevice(iface.DeviceName); err != nil {
			return err
		}
	}
	return nil
}

func removeEmptyTopologyDirs(layout state.Layout, m *state.Manifest) error {
	var dirs []string
	_ = filepath.WalkDir(layout.TopologyDir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove topology state directory %s: %w", dir, err)
		}
	}
	for _, dir := range []string{filepath.Dir(layout.TopologyDir), layout.SharedDir} {
		_ = os.Remove(dir)
	}
	return nil
}
