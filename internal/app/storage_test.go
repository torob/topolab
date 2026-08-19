package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
	storagepkg "github.com/torob/topolab/internal/storage"
)

func TestLVMResolutionIncludesRecordedRemovedStorage(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "lvm-wrapper")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := New(nil, nil)
	a.defaults()
	m := state.NewManifest("lab")
	m.Storages["removed"] = &state.Storage{Name: "removed", Type: "lvm-thick", VolumeGroup: "vg0"}
	if err := a.configureLVM(&config.Config{Name: "lab"}, m, HelperOverrides{LVM: executable}); err != nil {
		t.Fatal(err)
	}
	if a.LVM.Executable != executable {
		t.Fatalf("LVM executable = %q", a.LVM.Executable)
	}
}

func TestZeroBlockDeviceOverwritesEntireExtent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device")
	data := []byte(strings.Repeat("not-zero", 4097))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := zeroBlockDevice(path, uint64(len(data))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range got {
		if value != 0 {
			t.Fatalf("byte %d was not zeroed", i)
		}
	}
}

type storageRunner struct {
	calls                   []string
	vgJSON, lvJSON, lvAfter string
	order                   *[]string
	extended                bool
}

type recoveryImages struct {
	checkpointImages
	conversions int
}

func (f *recoveryImages) CreateCopy(ctx context.Context, path string, _ qemu.TemplateInfo, size uint64) error {
	return f.create(path, size)
}

func (f *recoveryImages) ConvertToRaw(context.Context, string, string) error {
	f.conversions++
	return nil
}

func TestReconcileRefusesToReplaceMissingRecordedPool(t *testing.T) {
	runner := &storageRunner{
		vgJSON: `{"report":[{"vg":[{"vg_name":"vg0","vg_uuid":"vg-id","vg_size":"10737418240","vg_free":"10737418240","pv_count":"1"}]}]}`,
		lvJSON: `{"report":[{"lv":[]}]}`,
	}
	a := New(nil, nil)
	a.LVM = storagepkg.NewLVM(runner, "lvm-test")
	cfg := &config.Config{Name: "lab", Storages: []*config.Storage{{Name: "thin", Type: "lvm-thin", VolumeGroup: "vg0", PoolSizeGiB: 4, RootProvisioning: "copy"}}}
	m := state.NewManifest("lab")
	m.Storages["thin"] = &state.Storage{Name: "thin", Type: "lvm-thin", VolumeGroup: "vg0", VGUUID: "vg-id", PoolName: storagepkg.PoolName("lab", "thin"), PoolUUID: "recorded-pool-id"}
	layout := state.Layout{ManifestPath: filepath.Join(t.TempDir(), "manifest.json")}
	err := a.reconcileStorages(context.Background(), cfg, layout, m)
	if err == nil || !strings.Contains(err.Error(), "recorded thin pool is absent") {
		t.Fatalf("error = %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " lvcreate ") {
			t.Fatalf("missing recorded pool was replaced: %v", runner.calls)
		}
	}
}

func TestSnapshotRecoveryRecordsVerifiedOrigin(t *testing.T) {
	pool := storagepkg.PoolName("lab", "thin")
	base := storagepkg.BaseName("lab", "thin", "template-digest")
	disk := storagepkg.DiskName("lab", "vm", "root")
	baseTags := storagepkg.Tags("lab", "thin", "base")
	diskTags := storagepkg.Tags("lab", "thin", "disk")
	lvJSON := fmt.Sprintf(`{"report":[{"lv":[
		{"lv_name":%q,"lv_uuid":"base-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"4294967296","pool_lv":%q,"origin":"","lv_attr":"Vri-a-tz--","lv_tags":%q,"data_percent":"","metadata_percent":""},
		{"lv_name":%q,"lv_uuid":"disk-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"4294967296","pool_lv":%q,"origin":%q,"lv_attr":"Vwi-a-tz--","lv_tags":%q,"data_percent":"","metadata_percent":""}
	]}]}`, base, pool, strings.Join(baseTags, ","), disk, pool, base, strings.Join(diskTags, ","))
	runner := &storageRunner{lvJSON: lvJSON}
	a := New(nil, nil)
	a.LVM = storagepkg.NewLVM(runner, "lvm-test")
	a.Images = &recoveryImages{checkpointImages: checkpointImages{sizes: map[string]uint64{}}}
	cfg := &config.Config{Name: "lab", Storages: []*config.Storage{{Name: "thin", Type: "lvm-thin", VolumeGroup: "vg0", PoolSizeGiB: 4, RootProvisioning: "snapshot"}}}
	m := state.NewManifest("lab")
	m.Storages["thin"] = &state.Storage{Name: "thin", Type: "lvm-thin", VolumeGroup: "vg0", VGUUID: "vg-id", PoolName: pool, PoolUUID: "pool-id", Bases: []state.LVResource{{Name: base, UUID: "base-id", Tags: baseTags}}}
	d := &state.Disk{Name: "root", Path: storagepkg.DevicePath("vg0", disk), Storage: "thin", StorageType: "lvm-thin", Provisioning: "snapshot", Format: "raw", VolumeGroup: "vg0", VGUUID: "vg-id", LVName: disk, PoolName: pool, OwnershipTags: diskTags, Pending: "create"}
	n := &state.Node{RootDisk: d}
	m.Nodes["vm"] = n
	layout := state.Layout{ManifestPath: filepath.Join(t.TempDir(), "manifest.json")}
	if err := a.createDisk(context.Background(), cfg, layout, m, "vm", n, d, qemu.TemplateInfo{Digest: "template-digest"}, true); err != nil {
		t.Fatal(err)
	}
	if d.Pending != "" || d.OriginName != base || d.OriginUUID != "base-id" || d.LVUUID != "disk-id" {
		t.Fatalf("recovered disk = %#v", d)
	}
}

func TestStagingRecoveryRefusesUUIDMismatchBeforeConversion(t *testing.T) {
	disk := storagepkg.DiskName("lab", "vm", "root")
	stage := storagepkg.StagingName("lab", "vm", "root")
	tags := append(storagepkg.Tags("lab", "thick", "disk"), "topolab.kind.staging")
	runner := &storageRunner{lvJSON: fmt.Sprintf(`{"report":[{"lv":[{"lv_name":%q,"lv_uuid":"actual-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"4294967296","pool_lv":"","origin":"","lv_attr":"-wi-a-----","lv_tags":%q,"data_percent":"","metadata_percent":""}]}]}`, disk, strings.Join(tags, ","))}
	images := &recoveryImages{checkpointImages: checkpointImages{sizes: map[string]uint64{}}}
	a := New(nil, nil)
	a.LVM = storagepkg.NewLVM(runner, "lvm-test")
	a.Images = images
	cfg := &config.Config{Name: "lab", Storages: []*config.Storage{{Name: "thick", Type: "lvm-thick", VolumeGroup: "vg0"}}}
	m := state.NewManifest("lab")
	m.Storages["thick"] = &state.Storage{Name: "thick", Type: "lvm-thick", VolumeGroup: "vg0", VGUUID: "vg-id"}
	d := &state.Disk{Name: "root", Path: storagepkg.DevicePath("vg0", disk), Storage: "thick", StorageType: "lvm-thick", Format: "raw", VolumeGroup: "vg0", VGUUID: "vg-id", LVName: disk, OwnershipTags: storagepkg.Tags("lab", "thick", "disk"), Pending: "create", StagingName: stage, StagingUUID: "different-id"}
	n := &state.Node{RootDisk: d}
	layout := state.Layout{ManifestPath: filepath.Join(t.TempDir(), "manifest.json")}
	err := a.createDisk(context.Background(), cfg, layout, m, "vm", n, d, qemu.TemplateInfo{Path: "/template"}, true)
	if err == nil || !strings.Contains(err.Error(), "staging checkpoint") || images.conversions != 0 {
		t.Fatalf("error=%v conversions=%d", err, images.conversions)
	}
}

func TestStorageRemovalPrevalidatesEveryBase(t *testing.T) {
	pool := storagepkg.PoolName("lab", "thin")
	baseTags := storagepkg.Tags("lab", "thin", "base")
	poolTags := storagepkg.Tags("lab", "thin", "pool")
	runner := &storageRunner{lvJSON: fmt.Sprintf(`{"report":[{"lv":[
		{"lv_name":"base-a","lv_uuid":"a-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"1","pool_lv":%q,"origin":"","lv_attr":"Vri-a-tz--","lv_tags":%q,"data_percent":"","metadata_percent":""},
		{"lv_name":"base-b","lv_uuid":"actual-b-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"1","pool_lv":%q,"origin":"","lv_attr":"Vri-a-tz--","lv_tags":%q,"data_percent":"","metadata_percent":""},
		{"lv_name":%q,"lv_uuid":"pool-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"4","pool_lv":"","origin":"","lv_attr":"twi-a-tz--","lv_tags":%q,"data_percent":"1","metadata_percent":"1"}
	]}]}`, pool, strings.Join(baseTags, ","), pool, strings.Join(baseTags, ","), pool, strings.Join(poolTags, ","))}
	a := New(nil, nil)
	a.LVM = storagepkg.NewLVM(runner, "lvm-test")
	s := &state.Storage{Name: "thin", Type: "lvm-thin", VolumeGroup: "vg0", PoolName: pool, PoolUUID: "pool-id", Bases: []state.LVResource{{Name: "base-a", UUID: "a-id", Tags: baseTags}, {Name: "base-b", UUID: "recorded-b-id", Tags: baseTags}}}
	if err := a.removeStorage(context.Background(), "lab", s); err == nil || !strings.Contains(err.Error(), "unverified template") {
		t.Fatalf("error = %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " lvremove ") {
			t.Fatalf("storage was partially removed: %v", runner.calls)
		}
	}
}

func (r *storageRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if len(args) > 0 && args[0] == "vgs" {
		return []byte(r.vgJSON), nil
	}
	if len(args) > 0 && args[0] == "lvs" {
		if r.extended && r.lvAfter != "" {
			return []byte(r.lvAfter), nil
		}
		return []byte(r.lvJSON), nil
	}
	if len(args) > 0 && args[0] == "lvextend" {
		r.extended = true
		if r.order != nil {
			*r.order = append(*r.order, "lvextend")
		}
	}
	return nil, nil
}

func TestStoragePlanRefusesUnexpectedLVInManagedPool(t *testing.T) {
	topology, storageName, vg := "lab", "thin", "vg0"
	pool := storagepkg.PoolName(topology, storageName)
	tags := strings.Join(storagepkg.Tags(topology, storageName, "pool"), ",")
	runner := &storageRunner{vgJSON: `{"report":[{"vg":[{"vg_name":"vg0","vg_uuid":"vg-id","vg_size":"10737418240","vg_free":"5368709120","pv_count":"1"}]}]}`, lvJSON: fmt.Sprintf(`{"report":[{"lv":[{"lv_name":%q,"lv_uuid":"pool-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"4294967296","pool_lv":"","origin":"","lv_attr":"twi-a-t---","lv_tags":%q,"data_percent":"1","metadata_percent":"1"},{"lv_name":"foreign","lv_uuid":"foreign-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"1048576","pool_lv":%q,"origin":"","lv_attr":"Vwi-a-tz--","lv_tags":"","data_percent":"","metadata_percent":""}]}]}`, pool, tags, pool)}
	a := New(nil, nil)
	a.LVM = storagepkg.NewLVM(runner, "lvm-test")
	cfg := &config.Config{Name: topology, Storages: []*config.Storage{{Name: storageName, Type: "lvm-thin", VolumeGroup: vg, PoolSizeGiB: 4, RootProvisioning: "copy"}}}
	m := state.NewManifest(topology)
	m.Storages[storageName] = &state.Storage{Name: storageName, Type: "lvm-thin", VolumeGroup: vg, VGUUID: "vg-id", PoolName: pool, PoolUUID: "pool-id", PoolSizeBytes: 4 << 30, RootProvisioning: "copy"}
	if _, err := a.planStorages(context.Background(), cfg, state.Layout{}, m); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("plan error = %v", err)
	}
}

func TestStoragePlanRefusesThinPoolZeroModeMismatch(t *testing.T) {
	topology, storageName, vg := "lab", "thin", "vg0"
	pool := storagepkg.PoolName(topology, storageName)
	tags := strings.Join(storagepkg.Tags(topology, storageName, "pool"), ",")
	runner := &storageRunner{
		vgJSON: `{"report":[{"vg":[{"vg_name":"vg0","vg_uuid":"vg-id","vg_size":"10737418240","vg_free":"5368709120","pv_count":"1"}]}]}`,
		lvJSON: fmt.Sprintf(`{"report":[{"lv":[{"lv_name":%q,"lv_uuid":"pool-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"4294967296","pool_lv":"","origin":"","lv_attr":"twi-a-tz--","lv_tags":%q,"data_percent":"1","metadata_percent":"1"}]}]}`, pool, tags),
	}
	a := New(nil, nil)
	a.LVM = storagepkg.NewLVM(runner, "lvm-test")
	cfg := &config.Config{Name: topology, Storages: []*config.Storage{{Name: storageName, Type: "lvm-thin", VolumeGroup: vg, PoolSizeGiB: 4, RootProvisioning: "copy"}}}
	m := state.NewManifest(topology)
	m.Storages[storageName] = &state.Storage{Name: storageName, Type: "lvm-thin", VolumeGroup: vg, VGUUID: "vg-id", PoolName: pool, PoolUUID: "pool-id", PoolSizeBytes: 4 << 30, RootProvisioning: "copy"}
	if _, err := a.planStorages(context.Background(), cfg, state.Layout{}, m); err == nil || !strings.Contains(err.Error(), "thin pool zeroing is true, want zero_new_blocks=false") {
		t.Fatalf("plan error = %v", err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, " lvchange ") {
			t.Fatalf("zero mode mismatch was mutated: %v", runner.calls)
		}
	}
}

func TestThickRecoveryHonorsRecordedWipePolicy(t *testing.T) {
	for _, test := range []struct {
		name      string
		wipe      bool
		wantWipes int
	}{{"disabled", false, 0}, {"enabled", true, 1}} {
		t.Run(test.name, func(t *testing.T) {
			disk := storagepkg.DiskName("lab", "vm", "root")
			tags := storagepkg.Tags("lab", "thick", "disk")
			runner := &storageRunner{lvJSON: fmt.Sprintf(`{"report":[{"lv":[{"lv_name":%q,"lv_uuid":"disk-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"4294967296","pool_lv":"","origin":"","lv_attr":"-wi-a-----","lv_tags":%q,"data_percent":"","metadata_percent":""}]}]}`, disk, strings.Join(tags, ","))}
			images := &recoveryImages{checkpointImages: checkpointImages{sizes: map[string]uint64{}}}
			a := New(nil, nil)
			a.LVM = storagepkg.NewLVM(runner, "lvm-test")
			a.Images = images
			wipes := 0
			a.ZeroBlockDevice = func(string, uint64) error { wipes++; return nil }
			cfg := &config.Config{Name: "lab", Storages: []*config.Storage{{Name: "thick", Type: "lvm-thick", VolumeGroup: "vg0", WipeOnCreate: !test.wipe}}}
			m := state.NewManifest("lab")
			m.Storages["thick"] = &state.Storage{Name: "thick", Type: "lvm-thick", VolumeGroup: "vg0", VGUUID: "vg-id"}
			d := &state.Disk{Name: "root", Path: storagepkg.DevicePath("vg0", disk), Storage: "thick", StorageType: "lvm-thick", Format: "raw", VolumeGroup: "vg0", VGUUID: "vg-id", LVName: disk, LVUUID: "disk-id", OwnershipTags: tags, Pending: "create", WipeOnCreate: test.wipe}
			n := &state.Node{RootDisk: d}
			layout := state.Layout{ManifestPath: filepath.Join(t.TempDir(), "manifest.json")}
			if err := a.createDisk(context.Background(), cfg, layout, m, "vm", n, d, qemu.TemplateInfo{Path: "/template"}, true); err != nil {
				t.Fatal(err)
			}
			if wipes != test.wantWipes || images.conversions != 1 || d.Pending != "" {
				t.Fatalf("wipes=%d conversions=%d disk=%#v", wipes, images.conversions, d)
			}
		})
	}
}

type orderedQMP struct{ calls *[]string }

func (q orderedQMP) BlockResize(_ context.Context, _ string, _ uint64) error {
	*q.calls = append(*q.calls, "qmp-block-resize")
	return nil
}
func (q orderedQMP) Balloon(context.Context, uint64) error        { return nil }
func (q orderedQMP) QueryBalloon(context.Context) (uint64, error) { return 0, nil }
func (q orderedQMP) QueryStatus(context.Context) (string, error)  { return "running", nil }
func (q orderedQMP) Close() error                                 { return nil }

type retryQMP struct {
	calls *int
	fail  bool
}

func (q retryQMP) BlockResize(context.Context, string, uint64) error {
	*q.calls++
	if q.fail {
		return fmt.Errorf("injected QMP failure")
	}
	return nil
}
func (q retryQMP) Balloon(context.Context, uint64) error        { return nil }
func (q retryQMP) QueryBalloon(context.Context) (uint64, error) { return 0, nil }
func (q retryQMP) QueryStatus(context.Context) (string, error)  { return "running", nil }
func (q retryQMP) Close() error                                 { return nil }

func TestRunningLVExtendsBeforeQMPResize(t *testing.T) {
	var order []string
	tags := storagepkg.Tags("lab", "thin", "disk")
	before := fmt.Sprintf(`{"report":[{"lv":[{"lv_name":"disk","lv_uuid":"disk-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"1073741824","pool_lv":"pool","origin":"","lv_attr":"Vwi-a-tz--","lv_tags":%q,"data_percent":"","metadata_percent":""}]}]}`, strings.Join(tags, ","))
	after := strings.Replace(before, "1073741824", "2147483648", 1)
	runner := &storageRunner{lvJSON: before, lvAfter: after, order: &order}
	a := New(nil, nil)
	a.LVM = storagepkg.NewLVM(runner, "lvm-test")
	a.Processes = &fakeProcesses{alive: map[int]bool{10: true}}
	a.OpenQMP = func(string) qmpClient { return orderedQMP{calls: &order} }
	disk := &state.Disk{Name: "data", StorageType: "lvm-thin", VolumeGroup: "vg0", VGUUID: "vg-id", LVName: "disk", LVUUID: "disk-id", PoolName: "pool", OwnershipTags: tags, CapacityBytes: 1 << 30, BlockNode: "blk"}
	if err := a.growDisk(context.Background(), &state.Node{PID: 10, QMPPath: "qmp"}, disk, 2<<30); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "lvextend,qmp-block-resize" {
		t.Fatalf("resize order = %v; LVM calls = %v", order, runner.calls)
	}
}

func TestRunningLVResizeRetriesQMPAfterHostGrowth(t *testing.T) {
	tags := storagepkg.Tags("lab", "thin", "disk")
	before := fmt.Sprintf(`{"report":[{"lv":[{"lv_name":"disk","lv_uuid":"disk-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"1073741824","pool_lv":"pool","origin":"","lv_attr":"Vwi-a-tz--","lv_tags":%q,"data_percent":"","metadata_percent":""}]}]}`, strings.Join(tags, ","))
	after := strings.Replace(before, "1073741824", "2147483648", 1)
	runner := &storageRunner{lvJSON: before, lvAfter: after}
	a := New(nil, nil)
	a.LVM = storagepkg.NewLVM(runner, "lvm-test")
	a.Processes = &fakeProcesses{alive: map[int]bool{10: true}}
	calls, fail := 0, true
	a.OpenQMP = func(string) qmpClient { return retryQMP{calls: &calls, fail: fail} }
	disk := &state.Disk{Name: "data", StorageType: "lvm-thin", VolumeGroup: "vg0", VGUUID: "vg-id", LVName: "disk", LVUUID: "disk-id", PoolName: "pool", OwnershipTags: tags, CapacityBytes: 1 << 30, BlockNode: "blk", Pending: "resize"}
	node := &state.Node{PID: 10, QMPPath: "qmp"}
	if err := a.growDisk(context.Background(), node, disk, 2<<30); err == nil || !strings.Contains(err.Error(), "injected QMP failure") {
		t.Fatalf("first resize error = %v", err)
	}
	if disk.CapacityBytes != 1<<30 || disk.Pending != "resize" || !runner.extended {
		t.Fatalf("failed QMP resize lost checkpoint: disk=%#v extended=%t", disk, runner.extended)
	}
	fail = false
	if err := a.growDisk(context.Background(), node, disk, 2<<30); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || disk.CapacityBytes != 2<<30 || disk.Pending != "" {
		t.Fatalf("retry did not complete QMP checkpoint: calls=%d disk=%#v", calls, disk)
	}
}
