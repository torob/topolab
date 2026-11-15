package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
	storagepkg "github.com/torob/topolab/internal/storage"
)

// Time limits of the snapshot steps. A freeze can take some seconds when a
// guest has much unwritten data.
const (
	snapshotProbeTimeout    = 2 * time.Second
	snapshotFreezeTimeout   = 30 * time.Second
	snapshotQMPTimeout      = 30 * time.Second
	snapshotInternalTimeout = 120 * time.Second
	snapshotThawAttempts    = 3
)

// snapshotQMP is the QMP access that snapshot commands use.
type snapshotQMP interface {
	QueryStatus(context.Context) (string, error)
	Pause(context.Context) error
	Resume(context.Context) error
	SnapshotInternal(context.Context, string, []string) error
	DeleteSnapshotInternal(context.Context, string, string) error
	Close() error
}

// snapshotImageManager changes the internal snapshots of a qcow2 image that
// no process has open.
type snapshotImageManager interface {
	SnapshotCreate(context.Context, string, string) error
	SnapshotApply(context.Context, string, string) error
	SnapshotDelete(context.Context, string, string) error
}

func syncBlockDevice(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// parseSnapshotTarget reads "NAME [NODE...]".
func parseSnapshotTarget(subcommand string, args []string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("snapshot %s requires NAME", subcommand)
	}
	name := args[0]
	if !state.ValidSnapshotName(name) {
		return "", nil, fmt.Errorf("invalid snapshot name %q: use lowercase letters, digits, \".\", \"_\", and \"-\", with at most 63 characters", name)
	}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") {
			return "", nil, fmt.Errorf("snapshot %s: unknown option %q", subcommand, arg)
		}
	}
	return name, args[1:], nil
}

// nodeDisks returns pointers to the root disk and the additional disks.
func nodeDisks(n *state.Node) []*state.Disk {
	if n == nil {
		return nil
	}
	var disks []*state.Disk
	if n.RootDisk != nil {
		disks = append(disks, n.RootDisk)
	}
	for i := range n.AdditionalDisks {
		disks = append(disks, &n.AdditionalDisks[i])
	}
	return disks
}

func findNodeDisk(n *state.Node, name string) *state.Disk {
	for _, d := range nodeDisks(n) {
		if d.Name == name {
			return d
		}
	}
	return nil
}

func (a *App) nodeRunning(n *state.Node) bool {
	return n != nil && n.PID > 0 && a.Processes.Alive(n.PID)
}

// verifyRunningNode makes the ownership checks of stop before TopoLab talks to
// a running QEMU process.
func (a *App) verifyRunningNode(topology, name string, n *state.Node) error {
	if err := a.verifyNodeNamespace(n); err != nil {
		return err
	}
	return qemu.VerifyProcessIdentity(a.Processes, n.PID, "topolab:"+topology+":"+name)
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// incompleteRestore returns the snapshot that has an incomplete restore.
func incompleteRestore(m *state.Manifest) *state.Snapshot {
	for _, name := range slices.Sorted(maps.Keys(m.Snapshots)) {
		if s := m.Snapshots[name]; s != nil && s.Restore != nil {
			return s
		}
	}
	return nil
}

// refuseRestoringDisks rejects a command that uses VM disks while a restore is
// in its restore phase, when disks can be absent or half restored.
func refuseRestoringDisks(m *state.Manifest) error {
	if m == nil {
		return nil
	}
	if s := incompleteRestore(m); s != nil && s.Restore.Phase == state.SnapshotRestorePhaseRestore {
		return fmt.Errorf("the restore of snapshot %s is incomplete; run topolab snapshot restore %s to complete it", s.Name, s.Name)
	}
	return nil
}

func snapshotIncomplete(s *state.Snapshot) bool {
	if s.Pending != "" {
		return true
	}
	for _, vm := range s.VMs {
		if vm != nil && vm.Pending != "" {
			return true
		}
	}
	return false
}

// snapshotOrigin reports whether the thin disk d of node has its origin in a
// recorded snapshot LV of the same disk. A restored thin disk is a thin
// snapshot of that LV.
func snapshotOrigin(m *state.Manifest, node string, d *state.Disk) bool {
	for _, s := range m.Snapshots {
		if s == nil {
			continue
		}
		vm := s.VMs[node]
		if vm == nil {
			continue
		}
		for _, sd := range vm.Disks {
			if sd.Name == d.Name && sd.LVName == d.OriginName && (d.OriginUUID == "" || sd.LVUUID == "" || sd.LVUUID == d.OriginUUID) {
				return true
			}
		}
	}
	return false
}

// validateSnapshotsForUp refuses an up that destroys snapshot data. It runs
// before up changes anything.
func validateSnapshotsForUp(old *state.Manifest, desired map[string]*state.Node) error {
	if old == nil {
		return nil
	}
	if err := refuseRestoringDisks(old); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(old.Snapshots)) {
		s := old.Snapshots[name]
		if s == nil {
			continue
		}
		for _, vmName := range slices.Sorted(maps.Keys(s.VMs)) {
			vm := s.VMs[vmName]
			want := desired[vmName]
			if want == nil {
				return fmt.Errorf("up would remove VM %s, which snapshot %q contains; run \"topolab snapshot delete %s %s\" first", vmName, name, name, vmName)
			}
			if want.StorageIdentityHash != vm.StorageIdentityHash {
				return fmt.Errorf("up would replace the root disk of VM %s, which snapshot %q contains; run \"topolab snapshot delete %s %s\" first", vmName, name, name, vmName)
			}
			wanted := map[string]bool{}
			for _, d := range want.AdditionalDisks {
				wanted[d.Name] = true
			}
			for _, sd := range vm.Disks {
				if sd.Name != "root" && !wanted[sd.Name] {
					return fmt.Errorf("up would remove disk %s of VM %s, which snapshot %q contains; run \"topolab snapshot delete %s %s\" first", sd.Name, vmName, name, name, vmName)
				}
			}
		}
	}
	return nil
}

// snapshotTarget is one VM of a snapshot create.
type snapshotTarget struct {
	name        string
	node        *state.Node
	record      *state.SnapshotVM
	running     bool
	client      snapshotQMP
	status      string
	agent       bool
	frozen      bool
	paused      bool
	pauseErr    error
	snapshotErr error
	thawErr     error
	resumeErr   error
}

func (a *App) snapshotCreate(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, args []string) error {
	name, selected, err := parseSnapshotTarget("create", args)
	if err != nil {
		return err
	}
	if m.Snapshots[name] != nil {
		return fmt.Errorf("snapshot %q already exists", name)
	}
	if s := incompleteRestore(m); s != nil {
		return fmt.Errorf("the restore of snapshot %s is incomplete; run topolab snapshot restore %s to complete it", s.Name, s.Name)
	}
	names, err := selectNodes(cfg, selected)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("the topology has no VMs")
	}
	vgLocks, err := a.planStorages(ctx, cfg, layout, m)
	if err != nil {
		return err
	}
	defer closeLocks(vgLocks)
	if err := a.verifyManifestStorage(ctx, m, false); err != nil {
		return err
	}
	images, _ := a.Images.(snapshotImageManager)
	record := &state.Snapshot{Name: name, CreatedAt: a.Now().UTC(), Pending: state.SnapshotPendingCreate, VMs: map[string]*state.SnapshotVM{}}
	targets := make([]*snapshotTarget, 0, len(names))
	lvsByVG := map[string][]storagepkg.LV{}
	for _, vmName := range names {
		n := m.Nodes[vmName]
		if n == nil || n.RootDisk == nil {
			return fmt.Errorf("VM %s has no recorded disks; run topolab up", vmName)
		}
		varsPath, err := layout.SnapshotVarsPath(name, vmName)
		if err != nil {
			return err
		}
		if n.OVMFVarsPath == "" || fileMissing(n.OVMFVarsPath) {
			return fmt.Errorf("VM %s has no OVMF variables file; run topolab up", vmName)
		}
		t := &snapshotTarget{name: vmName, node: n, running: a.nodeRunning(n)}
		if t.running {
			if err := a.verifyRunningNode(cfg.Name, vmName, n); err != nil {
				return fmt.Errorf("VM %s: %w", vmName, err)
			}
		}
		vm := &state.SnapshotVM{StorageIdentityHash: n.StorageIdentityHash, Running: t.running, OVMFVarsPath: varsPath}
		for _, d := range nodeDisks(n) {
			if d.Pending != "" {
				return fmt.Errorf("disk %s of VM %s has incomplete work %q; run topolab up first", d.Name, vmName, d.Pending)
			}
			sd := state.SnapshotDisk{Name: d.Name, Storage: d.Storage, StorageType: d.StorageType, Format: d.Format, CapacityBytes: d.CapacityBytes}
			switch d.StorageType {
			case "file", "":
				if d.Format != "qcow2" {
					return fmt.Errorf("disk %s of VM %s has format %q; snapshots need qcow2 file disks", d.Name, vmName, d.Format)
				}
				if images == nil && !t.running {
					return errors.New("image manager does not support qcow2 snapshots")
				}
				info, err := a.Images.Info(ctx, d.Path)
				if err != nil {
					return fmt.Errorf("inspect disk %s of VM %s: %w", d.Name, vmName, err)
				}
				if info.HasSnapshot(name) {
					return fmt.Errorf("disk %s of VM %s already has an internal snapshot %q; delete it with qemu-img snapshot -d %s %s", d.Name, vmName, name, name, d.Path)
				}
			case "lvm-thin":
				sd.VolumeGroup, sd.PoolName = d.VolumeGroup, d.PoolName
				sd.LVName = storagepkg.SnapshotLVName(cfg.Name, vmName, d.Name, name)
				sd.OwnershipTags = storagepkg.Tags(cfg.Name, d.Storage, "snapshot")
				lvs, ok := lvsByVG[d.VolumeGroup]
				if !ok {
					if lvs, err = a.LVM.LVs(ctx, d.VolumeGroup); err != nil {
						return err
					}
					lvsByVG[d.VolumeGroup] = lvs
				}
				if _, exists := storagepkg.FindLV(lvs, sd.LVName); exists {
					return fmt.Errorf("snapshot LV %s/%s for disk %s of VM %s already exists", d.VolumeGroup, sd.LVName, d.Name, vmName)
				}
			case "lvm-thick":
				return fmt.Errorf("disk %s of VM %s uses lvm-thick storage %s; snapshots support only file and lvm-thin storage", d.Name, vmName, d.Storage)
			default:
				return fmt.Errorf("disk %s of VM %s has unsupported storage type %q", d.Name, vmName, d.StorageType)
			}
			vm.Disks = append(vm.Disks, sd)
		}
		t.record = vm
		record.VMs[vmName] = vm
		targets = append(targets, t)
	}
	dir, err := layout.SnapshotDir(name)
	if err != nil {
		return err
	}
	for _, d := range []string{layout.SnapshotsDir, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	if m.Snapshots == nil {
		m.Snapshots = map[string]*state.Snapshot{}
	}
	m.Snapshots[name] = record
	if err := state.Save(layout.ManifestPath, m); err != nil {
		return err
	}

	err = a.takeSnapshot(ctx, record, targets, images)
	thawErr := thawErrors(targets)
	if err == nil {
		err = a.finishSnapshotLVs(ctx, record)
	}
	if err != nil {
		cleanup := context.WithoutCancel(ctx)
		cleanupErr := a.removeSnapshotVMs(cleanup, layout, m, record, names)
		if cleanupErr == nil {
			delete(m.Snapshots, name)
			_ = os.Remove(dir)
		}
		if saveErr := state.Save(layout.ManifestPath, m); saveErr != nil && cleanupErr == nil {
			cleanupErr = saveErr
		}
		err = errors.Join(err, thawErr)
		if cleanupErr != nil {
			return fmt.Errorf("snapshot %s failed: %w; the cleanup also failed: %v; run topolab snapshot delete %s", name, err, cleanupErr, name)
		}
		return fmt.Errorf("snapshot %s failed: %w", name, err)
	}
	record.Pending = ""
	if err := state.Save(layout.ManifestPath, m); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "NAME\tSTATE\tCONSISTENCY")
	for _, t := range targets {
		vmState := "stopped"
		if t.running {
			vmState = "running"
		}
		fmt.Fprintf(a.Stdout, "%s\t%s\t%s\n", t.name, vmState, t.record.Consistency)
	}
	if thawErr != nil {
		return fmt.Errorf("snapshot %s is ready, but %w", name, thawErr)
	}
	return nil
}

func closeLocks(locks []*state.Lock) {
	for i := len(locks) - 1; i >= 0; i-- {
		_ = locks[i].Close()
	}
}

// thawErrors returns the thaw failures of a create. A thaw failure does not
// make the snapshot invalid, but the guest cannot write to its disks.
func thawErrors(targets []*snapshotTarget) error {
	var errs []error
	for _, t := range targets {
		if t.thawErr != nil {
			errs = append(errs, fmt.Errorf("the file systems of VM %s can still be frozen: %w; run topolab restart %s", t.name, t.thawErr, t.name))
		}
	}
	return errors.Join(errs...)
}

// takeSnapshot saves the disks and the OVMF variables of every target. Stopped
// VMs come first. Then the running VMs get a freeze of their file systems
// through the guest agent, pause at the same moment, get their snapshots, and
// continue. It records thaw failures in the targets.
func (a *App) takeSnapshot(ctx context.Context, record *state.Snapshot, targets []*snapshotTarget, images snapshotImageManager) error {
	for _, t := range targets {
		if t.running {
			continue
		}
		t.record.Consistency = state.SnapshotStopped
		if err := a.snapshotVMDisks(ctx, record.Name, t, images); err != nil {
			return fmt.Errorf("VM %s: %w", t.name, err)
		}
	}
	var running []*snapshotTarget
	for _, t := range targets {
		if t.running {
			running = append(running, t)
		}
	}
	if len(running) == 0 {
		return nil
	}
	defer func() {
		for _, t := range running {
			if t.client != nil {
				_ = t.client.Close()
			}
		}
	}()
	for _, t := range running {
		t.client = a.OpenSnapshotQMP(t.node.QMPPath)
		qctx, cancel := context.WithTimeout(ctx, snapshotQMPTimeout)
		status, err := t.client.QueryStatus(qctx)
		cancel()
		if err != nil {
			return fmt.Errorf("VM %s: query QEMU status: %w", t.name, err)
		}
		t.status = status
	}

	// Freeze. A VM without a responsive agent gets a crash-consistent snapshot.
	parallel(running, func(t *snapshotTarget) {
		t.record.Consistency = state.SnapshotCrashConsistent
		if t.node.GuestAgentPath == "" {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, snapshotProbeTimeout)
		available := a.GuestAgentProbe(pctx, t.node.GuestAgentPath)
		cancel()
		if !available {
			fmt.Fprintf(a.Stderr, "warning: %s: the guest agent does not answer; the snapshot of %s is crash-consistent\n", t.name, t.name)
			return
		}
		t.agent = true
		fctx, cancel := context.WithTimeout(ctx, snapshotFreezeTimeout)
		_, err := a.GuestAgentExec(fctx, t.node.GuestAgentPath, "guest-fsfreeze-freeze")
		cancel()
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: %s: the guest agent did not freeze the file systems; the snapshot of %s is crash-consistent: %v\n", t.name, t.name, err)
			return
		}
		t.frozen = true
		t.record.Consistency = state.SnapshotFrozen
	})

	// Pause all running VMs at the same moment. QEMU flushes the disks first.
	parallel(running, func(t *snapshotTarget) {
		if t.status != "running" {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, snapshotQMPTimeout)
		t.pauseErr = t.client.Pause(pctx)
		cancel()
		t.paused = t.pauseErr == nil
	})
	var errs []error
	for _, t := range running {
		if t.pauseErr != nil {
			errs = append(errs, fmt.Errorf("VM %s: pause: %w", t.name, t.pauseErr))
		}
	}
	if len(errs) == 0 {
		parallel(running, func(t *snapshotTarget) {
			t.snapshotErr = a.snapshotVMDisks(ctx, record.Name, t, images)
		})
		for _, t := range running {
			if t.snapshotErr != nil {
				errs = append(errs, fmt.Errorf("VM %s: %w", t.name, t.snapshotErr))
			}
		}
	}

	// Resume and thaw also after an error or a cancel.
	release := context.WithoutCancel(ctx)
	parallel(running, func(t *snapshotTarget) {
		if !t.paused {
			return
		}
		rctx, cancel := context.WithTimeout(release, snapshotQMPTimeout)
		t.resumeErr = t.client.Resume(rctx)
		cancel()
	})
	parallel(running, func(t *snapshotTarget) {
		if !t.agent {
			return
		}
		t.thawErr = a.thaw(release, t.node.GuestAgentPath)
	})
	for _, t := range running {
		if t.resumeErr != nil {
			errs = append(errs, fmt.Errorf("VM %s did not continue after the pause: %w; run topolab restart %s", t.name, t.resumeErr, t.name))
		}
	}
	return errors.Join(errs...)
}

// thaw thaws the guest file systems. It tries again, because a frozen guest
// cannot write to its disks.
func (a *App) thaw(ctx context.Context, path string) error {
	var err error
	for attempt := 0; attempt < snapshotThawAttempts; attempt++ {
		tctx, cancel := context.WithTimeout(ctx, snapshotFreezeTimeout)
		_, err = a.GuestAgentExec(tctx, path, "guest-fsfreeze-thaw")
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

// parallel runs fn for each target at the same time and waits for all.
func parallel(targets []*snapshotTarget, fn func(*snapshotTarget)) {
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Go(func() { fn(t) })
	}
	wg.Wait()
}

// snapshotVMDisks takes the snapshot of each disk of one VM and copies its
// OVMF variables. A running VM must be paused, or have no CPU activity.
func (a *App) snapshotVMDisks(ctx context.Context, name string, t *snapshotTarget, images snapshotImageManager) error {
	var blockNodes []string
	for _, sd := range t.record.Disks {
		d := findNodeDisk(t.node, sd.Name)
		if d == nil {
			return fmt.Errorf("disk %s is not recorded", sd.Name)
		}
		switch sd.StorageType {
		case "file", "":
			if t.running {
				blockNodes = append(blockNodes, d.BlockNode)
				continue
			}
			if err := images.SnapshotCreate(ctx, d.Path, name); err != nil {
				return fmt.Errorf("snapshot disk %s: %w", d.Name, err)
			}
		case "lvm-thin":
			if t.running {
				// With cache writeback or unsafe, data can be in the host page
				// cache of the LV. A thin snapshot sees only the device.
				if err := a.SyncBlockDevice(d.Path); err != nil {
					return fmt.Errorf("flush disk %s: %w", d.Name, err)
				}
			}
			if err := a.LVM.Snapshot(ctx, d.VolumeGroup, d.LVName, sd.LVName, sd.OwnershipTags); err != nil {
				return fmt.Errorf("snapshot disk %s: %w", d.Name, err)
			}
		}
	}
	if len(blockNodes) > 0 {
		sctx, cancel := context.WithTimeout(ctx, snapshotInternalTimeout)
		err := t.client.SnapshotInternal(sctx, name, blockNodes)
		cancel()
		if err != nil {
			return fmt.Errorf("snapshot qcow2 disks: %w", err)
		}
	}
	if err := qemu.CopyFileAtomic(t.node.OVMFVarsPath, t.record.OVMFVarsPath, 0o600); err != nil {
		return fmt.Errorf("copy OVMF variables: %w", err)
	}
	return nil
}

// finishSnapshotLVs makes the new snapshot LVs read-only and records their
// identities and the digests of the OVMF variables copies.
func (a *App) finishSnapshotLVs(ctx context.Context, record *state.Snapshot) error {
	byVG := map[string][]storagepkg.LV{}
	for _, vmName := range slices.Sorted(maps.Keys(record.VMs)) {
		vm := record.VMs[vmName]
		digest, err := fileDigest(vm.OVMFVarsPath)
		if err != nil {
			return fmt.Errorf("VM %s: read OVMF variables copy: %w", vmName, err)
		}
		vm.OVMFVarsDigest = digest
		for i := range vm.Disks {
			sd := &vm.Disks[i]
			if sd.StorageType != "lvm-thin" {
				continue
			}
			if err := a.LVM.SetReadOnly(ctx, sd.VolumeGroup, sd.LVName); err != nil {
				return fmt.Errorf("VM %s disk %s: %w", vmName, sd.Name, err)
			}
			lvs, ok := byVG[sd.VolumeGroup]
			if !ok {
				if lvs, err = a.LVM.LVs(ctx, sd.VolumeGroup); err != nil {
					return err
				}
				byVG[sd.VolumeGroup] = lvs
			}
			lv, found := storagepkg.FindLV(lvs, sd.LVName)
			if !found {
				return fmt.Errorf("VM %s disk %s: snapshot LV %s/%s is absent after creation", vmName, sd.Name, sd.VolumeGroup, sd.LVName)
			}
			if !storagepkg.HasTags(lv.Tags, sd.OwnershipTags) || lv.Pool != sd.PoolName {
				return fmt.Errorf("VM %s disk %s: snapshot LV %s/%s failed ownership verification", vmName, sd.Name, sd.VolumeGroup, sd.LVName)
			}
			sd.LVUUID = lv.UUID
		}
	}
	return nil
}

// removeSnapshotVMs removes the parts of record that belong to the named VMs:
// the internal qcow2 snapshots, the snapshot LVs, and the OVMF variables
// copies. A part that does not exist is skipped, so a retry completes an
// interrupted removal. It saves the manifest after each VM.
func (a *App) removeSnapshotVMs(ctx context.Context, layout state.Layout, m *state.Manifest, record *state.Snapshot, names []string) error {
	images, _ := a.Images.(snapshotImageManager)
	for _, vmName := range names {
		vm := record.VMs[vmName]
		if vm == nil {
			continue
		}
		n := m.Nodes[vmName]
		running := a.nodeRunning(n)
		var client snapshotQMP
		for _, sd := range vm.Disks {
			var err error
			switch sd.StorageType {
			case "file", "":
				d := findNodeDisk(n, sd.Name)
				if d == nil || fileMissing(d.Path) {
					continue
				}
				var info qemu.ImageInfo
				if info, err = a.Images.Info(ctx, d.Path); err != nil {
					return fmt.Errorf("VM %s: inspect disk %s: %w", vmName, sd.Name, err)
				}
				if !info.HasSnapshot(record.Name) {
					continue
				}
				if running {
					if client == nil {
						if err := a.verifyRunningNode(layout.Topology, vmName, n); err != nil {
							return fmt.Errorf("VM %s: %w", vmName, err)
						}
						client = a.OpenSnapshotQMP(n.QMPPath)
						defer client.Close()
					}
					qctx, cancel := context.WithTimeout(ctx, snapshotInternalTimeout)
					err = client.DeleteSnapshotInternal(qctx, d.BlockNode, record.Name)
					cancel()
				} else if images == nil {
					err = errors.New("image manager does not support qcow2 snapshots")
				} else {
					err = images.SnapshotDelete(ctx, d.Path, record.Name)
				}
			case "lvm-thin":
				err = a.removeSnapshotLV(ctx, m, sd)
			}
			if err != nil {
				return fmt.Errorf("VM %s: remove the snapshot of disk %s: %w", vmName, sd.Name, err)
			}
		}
		if err := os.Remove(vm.OVMFVarsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("VM %s: %w", vmName, err)
		}
		delete(record.VMs, vmName)
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
	}
	return nil
}

// removeSnapshotLV removes one verified snapshot LV. A disk whose origin is
// that LV loses its origin in LVM, so the disk record loses it too.
func (a *App) removeSnapshotLV(ctx context.Context, m *state.Manifest, sd state.SnapshotDisk) error {
	lvs, err := a.LVM.LVs(ctx, sd.VolumeGroup)
	if err != nil {
		return err
	}
	lv, found := storagepkg.FindLV(lvs, sd.LVName)
	if found {
		if (sd.LVUUID != "" && lv.UUID != sd.LVUUID) || !storagepkg.HasTags(lv.Tags, sd.OwnershipTags) || lv.Pool != sd.PoolName {
			return fmt.Errorf("refusing removal of unverified snapshot LV %s/%s", sd.VolumeGroup, sd.LVName)
		}
		if err := a.LVM.Remove(ctx, sd.VolumeGroup, sd.LVName); err != nil {
			return err
		}
	}
	for _, n := range m.Nodes {
		for _, d := range nodeDisks(n) {
			if d.VolumeGroup == sd.VolumeGroup && d.OriginName == sd.LVName {
				d.OriginName, d.OriginUUID = "", ""
			}
		}
	}
	return nil
}

// removeAllSnapshots removes every snapshot. down calls it after the VMs
// stopped and before it removes the disks and the storages.
func (a *App) removeAllSnapshots(ctx context.Context, layout state.Layout, m *state.Manifest) error {
	for _, name := range slices.Sorted(maps.Keys(m.Snapshots)) {
		record := m.Snapshots[name]
		if record == nil {
			delete(m.Snapshots, name)
			continue
		}
		for _, vm := range record.VMs {
			if vm != nil {
				vm.Pending = state.SnapshotPendingDelete
			}
		}
		for _, vmName := range slices.Sorted(maps.Keys(record.VMs)) {
			vm := record.VMs[vmName]
			for _, sd := range vm.Disks {
				if sd.StorageType == "lvm-thin" {
					if err := a.removeSnapshotLV(ctx, m, sd); err != nil {
						return fmt.Errorf("snapshot %s VM %s disk %s: %w", name, vmName, sd.Name, err)
					}
				}
			}
			if err := os.Remove(vm.OVMFVarsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			delete(record.VMs, vmName)
			if err := state.Save(layout.ManifestPath, m); err != nil {
				return err
			}
		}
		delete(m.Snapshots, name)
		if dir, err := layout.SnapshotDir(name); err == nil {
			_ = os.Remove(dir)
		}
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) snapshotDelete(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, args []string) error {
	name, selected, err := parseSnapshotTarget("delete", args)
	if err != nil {
		return err
	}
	record := m.Snapshots[name]
	if record == nil {
		return fmt.Errorf("snapshot %q does not exist", name)
	}
	if record.Restore != nil {
		return fmt.Errorf("the restore of snapshot %s is incomplete; run topolab snapshot restore %s to complete it first", name, name)
	}
	names, err := snapshotSelection(cfg, record, selected)
	if err != nil {
		return err
	}
	vgLocks, err := a.planStorages(ctx, cfg, layout, m)
	if err != nil {
		return err
	}
	defer closeLocks(vgLocks)
	if err := a.verifyManifestStorage(ctx, m, false); err != nil {
		return err
	}
	if record.Pending == state.SnapshotPendingCreate {
		a.releaseInterruptedCreate(ctx, layout.Topology, m, record, names)
	}
	for _, vmName := range names {
		record.VMs[vmName].Pending = state.SnapshotPendingDelete
	}
	if err := state.Save(layout.ManifestPath, m); err != nil {
		return err
	}
	if err := a.removeSnapshotVMs(ctx, layout, m, record, names); err != nil {
		return err
	}
	if len(record.VMs) == 0 {
		delete(m.Snapshots, name)
		if dir, err := layout.SnapshotDir(name); err == nil {
			if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return state.Save(layout.ManifestPath, m)
}

// releaseInterruptedCreate lets the VMs of an interrupted create continue. A
// create that ended without its cleanup, for example after SIGKILL, can leave
// running VMs paused and their file systems frozen.
func (a *App) releaseInterruptedCreate(ctx context.Context, topology string, m *state.Manifest, record *state.Snapshot, names []string) {
	for _, vmName := range names {
		vm, n := record.VMs[vmName], m.Nodes[vmName]
		if vm == nil || !vm.Running || !a.nodeRunning(n) || a.verifyRunningNode(topology, vmName, n) != nil {
			continue
		}
		client := a.OpenSnapshotQMP(n.QMPPath)
		qctx, cancel := context.WithTimeout(ctx, snapshotQMPTimeout)
		status, err := client.QueryStatus(qctx)
		if err == nil && status == "paused" {
			if err := client.Resume(qctx); err == nil {
				fmt.Fprintf(a.Stderr, "VM %s continues after the interrupted snapshot create\n", vmName)
			}
		}
		cancel()
		_ = client.Close()
		if n.GuestAgentPath != "" {
			pctx, cancel := context.WithTimeout(ctx, snapshotProbeTimeout)
			available := a.GuestAgentProbe(pctx, n.GuestAgentPath)
			cancel()
			if available {
				_ = a.thaw(ctx, n.GuestAgentPath)
			}
		}
	}
}

// snapshotSelection returns the named VMs in the given order, or all VMs of
// the snapshot in topology order. VMs that are not in the topology file come
// last, in name order.
func snapshotSelection(cfg *config.Config, record *state.Snapshot, selected []string) ([]string, error) {
	if len(selected) == 0 {
		var names []string
		for _, vmName := range cfg.VMNames() {
			if record.VMs[vmName] != nil {
				names = append(names, vmName)
			}
		}
		for _, vmName := range slices.Sorted(maps.Keys(record.VMs)) {
			if !slices.Contains(names, vmName) {
				names = append(names, vmName)
			}
		}
		return names, nil
	}
	var names []string
	seen := map[string]bool{}
	for _, vmName := range selected {
		if record.VMs[vmName] == nil {
			return nil, fmt.Errorf("VM %s is not in snapshot %s", vmName, record.Name)
		}
		if !seen[vmName] {
			seen[vmName] = true
			names = append(names, vmName)
		}
	}
	return names, nil
}

func (a *App) snapshotRestore(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, args []string) error {
	name, selected, err := parseSnapshotTarget("restore", args)
	if err != nil {
		return err
	}
	record := m.Snapshots[name]
	if record == nil {
		return fmt.Errorf("snapshot %q does not exist", name)
	}
	if record.Pending == state.SnapshotPendingCreate {
		return fmt.Errorf("snapshot %s is incomplete; run topolab snapshot delete %s", name, name)
	}
	for _, vmName := range slices.Sorted(maps.Keys(record.VMs)) {
		if record.VMs[vmName].Pending != "" {
			return fmt.Errorf("the delete of VM %s from snapshot %s is incomplete; run topolab snapshot delete %s %s", vmName, name, name, vmName)
		}
	}
	if other := incompleteRestore(m); other != nil && other != record {
		return fmt.Errorf("the restore of snapshot %s is incomplete; run topolab snapshot restore %s to complete it", other.Name, other.Name)
	}
	vgLocks, err := a.planStorages(ctx, cfg, layout, m)
	if err != nil {
		return err
	}
	defer closeLocks(vgLocks)
	if err := a.verifyManifestStorage(ctx, m, false); err != nil {
		return err
	}
	r := record.Restore
	if r != nil {
		if len(selected) > 0 && !sameNames(selected, r.VMs) {
			return fmt.Errorf("the restore of snapshot %s is incomplete for VMs %s; run topolab snapshot restore %s", name, strings.Join(r.VMs, " "), name)
		}
	} else {
		names, err := snapshotSelection(cfg, record, selected)
		if err != nil {
			return err
		}
		if err := a.validateSnapshotRestore(ctx, cfg, m, record, names); err != nil {
			return err
		}
		r = &state.SnapshotRestore{Phase: state.SnapshotRestorePhaseRestore, VMs: names}
		for _, vmName := range names {
			if a.nodeRunning(m.Nodes[vmName]) {
				r.Running = append(r.Running, vmName)
			}
		}
		record.Restore, record.Pending = r, state.SnapshotPendingRestore
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
	}
	if r.Phase == state.SnapshotRestorePhaseRestore {
		if _, err := a.hardStopVMs(ctx, cfg, layout, m, r.VMs); err != nil {
			return fmt.Errorf("snapshot restore %s: %w", name, err)
		}
		for _, vmName := range r.VMs {
			if slices.Contains(r.Restored, vmName) {
				continue
			}
			if err := a.restoreVM(ctx, cfg, layout, m, record, vmName); err != nil {
				return fmt.Errorf("snapshot restore %s: VM %s: %w; run topolab snapshot restore %s again", name, vmName, err, name)
			}
			r.Restored = append(r.Restored, vmName)
			if err := state.Save(layout.ManifestPath, m); err != nil {
				return err
			}
		}
		r.Phase = state.SnapshotRestorePhaseStart
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
	}
	if err := a.startRestoredVMs(ctx, cfg, layout, m, r); err != nil {
		record.Restore, record.Pending = nil, ""
		return errors.Join(err, state.Save(layout.ManifestPath, m))
	}
	record.Restore, record.Pending = nil, ""
	if err := state.Save(layout.ManifestPath, m); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "NAME\tBEFORE\tRESULT\tAFTER")
	for _, vmName := range r.VMs {
		before := "stopped"
		if slices.Contains(r.Running, vmName) {
			before = "running"
		}
		after := "stopped"
		if a.nodeRunning(m.Nodes[vmName]) {
			after = "running"
		}
		fmt.Fprintf(a.Stdout, "%s\t%s\trestored\t%s\n", vmName, before, after)
	}
	return nil
}

func sameNames(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// startRestoredVMs starts the VMs that ran before the restore. The disks are
// already restored, so a start failure does not undo the restore.
func (a *App) startRestoredVMs(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, r *state.SnapshotRestore) error {
	if len(r.Running) == 0 {
		return nil
	}
	hint := fmt.Sprintf("the disks are restored; run topolab start %s", strings.Join(r.Running, " "))
	netState := oldNetwork(m)
	if netState == nil {
		return fmt.Errorf("managed network is absent; %s after topolab up", hint)
	}
	if err := a.Network.Verify(ctx, netState); err != nil {
		return fmt.Errorf("%w; %s", err, hint)
	}
	run := &startRun{}
	for _, vmName := range r.Running {
		if err := a.verifyNodeDisks(ctx, m.Nodes[vmName]); err != nil {
			return fmt.Errorf("verify node %s storage: %w; %s", vmName, err, hint)
		}
		startErr := a.startNode(ctx, cfg, layout, m, vmName, m.Nodes[vmName], netState, run)
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if startErr != nil {
			return fmt.Errorf("VM %s did not start after the restore: %w; %s", vmName, startErr, hint)
		}
	}
	return nil
}

// validateSnapshotRestore checks a new restore before it stops any VM.
func (a *App) validateSnapshotRestore(ctx context.Context, cfg *config.Config, m *state.Manifest, record *state.Snapshot, names []string) error {
	images, _ := a.Images.(snapshotImageManager)
	for _, vmName := range names {
		vm := record.VMs[vmName]
		if _, ok := cfg.LookupVM(vmName); !ok {
			return fmt.Errorf("VM %s is not in the topology file", vmName)
		}
		n := m.Nodes[vmName]
		if n == nil || n.RootDisk == nil {
			return fmt.Errorf("VM %s has no recorded disks; run topolab up", vmName)
		}
		if a.nodeRunning(n) {
			if err := a.verifyRunningNode(cfg.Name, vmName, n); err != nil {
				return fmt.Errorf("VM %s: %w", vmName, err)
			}
		}
		if n.StorageIdentityHash != vm.StorageIdentityHash {
			return fmt.Errorf("the root disk of VM %s changed after snapshot %s", vmName, record.Name)
		}
		current := nodeDisks(n)
		if len(current) != len(vm.Disks) {
			return fmt.Errorf("the disks of VM %s changed after snapshot %s", vmName, record.Name)
		}
		for _, sd := range vm.Disks {
			d := findNodeDisk(n, sd.Name)
			if d == nil || d.Storage != sd.Storage || d.StorageType != sd.StorageType || d.VolumeGroup != sd.VolumeGroup {
				return fmt.Errorf("the disks of VM %s changed after snapshot %s", vmName, record.Name)
			}
			if d.Pending != "" {
				return fmt.Errorf("disk %s of VM %s has incomplete work %q; run topolab up first", d.Name, vmName, d.Pending)
			}
			if d.CapacityBytes < sd.CapacityBytes {
				return fmt.Errorf("disk %s of VM %s is smaller than in snapshot %s", d.Name, vmName, record.Name)
			}
			switch sd.StorageType {
			case "file", "":
				if images == nil {
					return errors.New("image manager does not support qcow2 snapshots")
				}
				info, err := a.Images.Info(ctx, d.Path)
				if err != nil {
					return fmt.Errorf("inspect disk %s of VM %s: %w", d.Name, vmName, err)
				}
				if !info.HasSnapshot(record.Name) {
					return fmt.Errorf("disk %s of VM %s has no internal snapshot %q", d.Name, vmName, record.Name)
				}
			case "lvm-thin":
				if sd.LVUUID == "" {
					return fmt.Errorf("snapshot LV %s/%s of VM %s has no recorded identity", sd.VolumeGroup, sd.LVName, vmName)
				}
			default:
				return fmt.Errorf("disk %s of VM %s has unsupported storage type %q", d.Name, vmName, sd.StorageType)
			}
		}
		digest, err := fileDigest(vm.OVMFVarsPath)
		if err != nil {
			return fmt.Errorf("VM %s: read OVMF variables copy: %w", vmName, err)
		}
		if vm.OVMFVarsDigest != "" && digest != vm.OVMFVarsDigest {
			return fmt.Errorf("VM %s: the OVMF variables copy of snapshot %s changed", vmName, record.Name)
		}
	}
	return nil
}

// restoreVM restores the disks and the OVMF variables of one stopped VM. Each
// step can run again after an interruption.
func (a *App) restoreVM(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, record *state.Snapshot, vmName string) error {
	n, vm := m.Nodes[vmName], record.VMs[vmName]
	if n == nil || vm == nil {
		return errors.New("VM is not recorded")
	}
	if a.nodeRunning(n) {
		return errors.New("VM still runs")
	}
	images, _ := a.Images.(snapshotImageManager)
	for _, sd := range vm.Disks {
		d := findNodeDisk(n, sd.Name)
		if d == nil {
			return fmt.Errorf("disk %s is not recorded", sd.Name)
		}
		switch sd.StorageType {
		case "file", "":
			if images == nil {
				return errors.New("image manager does not support qcow2 snapshots")
			}
			if err := images.SnapshotApply(ctx, d.Path, record.Name); err != nil {
				return fmt.Errorf("restore disk %s: %w", d.Name, err)
			}
			info, err := a.Images.Info(ctx, d.Path)
			if err != nil {
				return fmt.Errorf("inspect disk %s: %w", d.Name, err)
			}
			if info.VirtualSize > d.CapacityBytes {
				return fmt.Errorf("restored disk %s has %d bytes, more than the recorded %d bytes", d.Name, info.VirtualSize, d.CapacityBytes)
			}
			if info.VirtualSize < d.CapacityBytes {
				if err := a.Images.ResizeOffline(ctx, qemu.Growth{Path: d.Path, BlockNode: d.BlockNode, Actual: info.VirtualSize, Desired: d.CapacityBytes}); err != nil {
					return fmt.Errorf("grow restored disk %s: %w", d.Name, err)
				}
			}
		case "lvm-thin":
			if err := a.restoreThinDisk(ctx, cfg, layout, m, vmName, d, sd); err != nil {
				return fmt.Errorf("restore disk %s: %w", d.Name, err)
			}
		default:
			return fmt.Errorf("disk %s has unsupported storage type %q", d.Name, sd.StorageType)
		}
	}
	digest, err := fileDigest(vm.OVMFVarsPath)
	if err != nil {
		return fmt.Errorf("read OVMF variables copy: %w", err)
	}
	if vm.OVMFVarsDigest != "" && digest != vm.OVMFVarsDigest {
		return errors.New("the OVMF variables copy changed")
	}
	if err := qemu.CopyFileAtomic(vm.OVMFVarsPath, n.OVMFVarsPath, 0o600); err != nil {
		return fmt.Errorf("restore OVMF variables: %w", err)
	}
	return nil
}

// restoreThinDisk replaces a thin disk LV with a new thin snapshot of the
// snapshot LV. The steps use the existing staging checkpoint of a disk:
//
//  1. Mark the disk as pending "restore".
//  2. Remove the current disk LV. Record the staging name and the new origin.
//  3. Create the staging LV as a thin snapshot of the snapshot LV, record its
//     UUID, and grow it to the disk capacity.
//  4. Rename the staging LV to the disk LV name and remove the staging tag.
//
// A retry finds the state of each step from the LVs and continues.
func (a *App) restoreThinDisk(ctx context.Context, cfg *config.Config, layout state.Layout, m *state.Manifest, vmName string, d *state.Disk, sd state.SnapshotDisk) error {
	stage := storagepkg.StagingName(cfg.Name, vmName, d.Name)
	stageTags := storagepkg.Tags(cfg.Name, d.Storage, "staging")
	const stagingTag = "topolab.kind.staging"
	lvs, err := a.LVM.LVs(ctx, d.VolumeGroup)
	if err != nil {
		return err
	}
	snapshotLV, found := storagepkg.FindLV(lvs, sd.LVName)
	if !found || snapshotLV.UUID != sd.LVUUID || !storagepkg.HasTags(snapshotLV.Tags, sd.OwnershipTags) || snapshotLV.Pool != sd.PoolName {
		return fmt.Errorf("snapshot LV %s/%s is absent or failed ownership verification", sd.VolumeGroup, sd.LVName)
	}
	if d.Pending != "restore" {
		d.Pending = "restore"
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
	}
	current, currentFound := storagepkg.FindLV(lvs, d.LVName)
	restored := currentFound && d.StagingUUID != "" && current.UUID == d.StagingUUID
	if currentFound && !restored {
		if d.LVUUID == "" || current.UUID != d.LVUUID || !storagepkg.HasTags(current.Tags, d.OwnershipTags) || current.Pool != d.PoolName {
			return fmt.Errorf("refusing removal of unverified LV %s/%s", d.VolumeGroup, d.LVName)
		}
		if err := a.LVM.Remove(ctx, d.VolumeGroup, d.LVName); err != nil {
			return err
		}
		currentFound = false
	}
	if !currentFound && d.StagingName == "" {
		d.LVUUID = ""
		d.OriginName, d.OriginUUID = sd.LVName, sd.LVUUID
		d.StagingName, d.StagingUUID = stage, ""
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
	}
	if !currentFound {
		if lvs, err = a.LVM.LVs(ctx, d.VolumeGroup); err != nil {
			return err
		}
		staged, stagedFound := storagepkg.FindLV(lvs, stage)
		if !stagedFound {
			if err := a.LVM.Snapshot(ctx, d.VolumeGroup, sd.LVName, stage, uniqueStrings(append(append([]string(nil), stageTags...), d.OwnershipTags...))); err != nil {
				return err
			}
			if lvs, err = a.LVM.LVs(ctx, d.VolumeGroup); err != nil {
				return err
			}
			if staged, stagedFound = storagepkg.FindLV(lvs, stage); !stagedFound {
				return fmt.Errorf("staging LV %s/%s is absent after creation", d.VolumeGroup, stage)
			}
		}
		if !storagepkg.HasTags(staged.Tags, stageTags) || !storagepkg.HasTags(staged.Tags, d.OwnershipTags) || staged.Pool != d.PoolName || staged.Origin != sd.LVName {
			return fmt.Errorf("staging LV %s/%s failed ownership verification", d.VolumeGroup, stage)
		}
		if d.StagingUUID != "" && staged.UUID != d.StagingUUID {
			return fmt.Errorf("staging LV %s/%s UUID changed", d.VolumeGroup, stage)
		}
		d.StagingUUID = staged.UUID
		if err := state.Save(layout.ManifestPath, m); err != nil {
			return err
		}
		if d.CapacityBytes > staged.SizeBytes {
			if err := a.LVM.Extend(ctx, d.VolumeGroup, stage, d.CapacityBytes); err != nil {
				return err
			}
		}
		if err := a.LVM.Rename(ctx, d.VolumeGroup, stage, d.LVName); err != nil {
			return err
		}
	}
	if lvs, err = a.LVM.LVs(ctx, d.VolumeGroup); err != nil {
		return err
	}
	current, currentFound = storagepkg.FindLV(lvs, d.LVName)
	if !currentFound || current.UUID != d.StagingUUID {
		return fmt.Errorf("restored LV %s/%s failed identity verification", d.VolumeGroup, d.LVName)
	}
	if current.SizeBytes < d.CapacityBytes {
		if err := a.LVM.Extend(ctx, d.VolumeGroup, d.LVName, d.CapacityBytes); err != nil {
			return err
		}
	}
	if storagepkg.HasTags(current.Tags, []string{stagingTag}) {
		if err := a.LVM.DeleteTag(ctx, d.VolumeGroup, d.LVName, stagingTag); err != nil {
			return err
		}
	}
	d.LVUUID, d.VGUUID = current.UUID, current.VGUUID
	d.OriginName, d.OriginUUID = sd.LVName, sd.LVUUID
	d.StagingName, d.StagingUUID, d.Pending = "", "", ""
	return state.Save(layout.ManifestPath, m)
}

// snapshotListEntry is one snapshot in the JSON output of snapshot list.
type snapshotListEntry struct {
	Name      string               `json:"name"`
	State     string               `json:"state"`
	Pending   string               `json:"pending,omitempty"`
	CreatedAt string               `json:"created_at"`
	VMs       []snapshotListVM     `json:"vms"`
	Restore   *snapshotListRestore `json:"restore,omitempty"`
}

type snapshotListRestore struct {
	Phase   string   `json:"phase"`
	VMs     []string `json:"vms"`
	Running []string `json:"running"`
}

type snapshotListVM struct {
	Name        string             `json:"name"`
	Running     bool               `json:"running"`
	Consistency string             `json:"consistency"`
	Pending     string             `json:"pending,omitempty"`
	Disks       []snapshotListDisk `json:"disks"`
}

type snapshotListDisk struct {
	Name          string `json:"name"`
	Storage       string `json:"storage"`
	StorageType   string `json:"storage_type"`
	CapacityBytes uint64 `json:"capacity_bytes"`
	VolumeGroup   string `json:"volume_group,omitempty"`
	LVName        string `json:"lv_name,omitempty"`
}

type snapshotListOutput struct {
	SchemaVersion int                 `json:"schema_version"`
	Topology      string              `json:"topology"`
	Snapshots     []snapshotListEntry `json:"snapshots"`
}

func parseSnapshotList(args []string) (string, error) {
	format, set := "table", false
	for i := 0; i < len(args); i++ {
		if args[i] != "--format" || i+1 >= len(args) {
			return "", errors.New("snapshot list accepts only --format table|json")
		}
		i++
		if set || (args[i] != "table" && args[i] != "json") {
			return "", errors.New("snapshot list --format must be table or json and may be specified only once")
		}
		format, set = args[i], true
	}
	return format, nil
}

func (a *App) snapshotList(m *state.Manifest, args []string) error {
	format, err := parseSnapshotList(args)
	if err != nil {
		return err
	}
	out := snapshotListOutput{SchemaVersion: 1, Topology: m.Topology, Snapshots: []snapshotListEntry{}}
	for _, name := range slices.Sorted(maps.Keys(m.Snapshots)) {
		s := m.Snapshots[name]
		if s == nil {
			continue
		}
		e := snapshotListEntry{Name: name, State: "ready", Pending: s.Pending, CreatedAt: formatHardStopTime(s.CreatedAt), VMs: []snapshotListVM{}}
		if snapshotIncomplete(s) {
			e.State = "pending"
			if e.Pending == "" {
				e.Pending = state.SnapshotPendingDelete
			}
		}
		if s.Restore != nil {
			e.Restore = &snapshotListRestore{Phase: s.Restore.Phase, VMs: s.Restore.VMs, Running: append([]string{}, s.Restore.Running...)}
		}
		for _, vmName := range slices.Sorted(maps.Keys(s.VMs)) {
			vm := s.VMs[vmName]
			v := snapshotListVM{Name: vmName, Running: vm.Running, Consistency: vm.Consistency, Pending: vm.Pending, Disks: []snapshotListDisk{}}
			for _, sd := range vm.Disks {
				v.Disks = append(v.Disks, snapshotListDisk{Name: sd.Name, Storage: sd.Storage, StorageType: sd.StorageType, CapacityBytes: sd.CapacityBytes, VolumeGroup: sd.VolumeGroup, LVName: sd.LVName})
			}
			e.VMs = append(e.VMs, v)
		}
		out.Snapshots = append(out.Snapshots, e)
	}
	if format == "json" {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Fprintln(a.Stdout, "NAME\tSTATE\tCREATED\tVMS\tCONSISTENCY")
	for _, e := range out.Snapshots {
		counts := map[string]int{}
		for _, vm := range e.VMs {
			counts[vm.Consistency]++
		}
		var summary []string
		for _, kind := range []string{state.SnapshotFrozen, state.SnapshotCrashConsistent, state.SnapshotStopped} {
			if counts[kind] > 0 {
				summary = append(summary, fmt.Sprintf("%d %s", counts[kind], kind))
			}
		}
		if len(summary) == 0 {
			summary = []string{"-"}
		}
		snapshotState := e.State
		if e.Pending != "" {
			snapshotState += " (" + e.Pending + ")"
		}
		fmt.Fprintf(a.Stdout, "%s\t%s\t%s\t%d\t%s\n", e.Name, snapshotState, e.CreatedAt, len(e.VMs), strings.Join(summary, ", "))
	}
	return nil
}
