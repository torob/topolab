package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManifestAtomicRoundTrip(t *testing.T) {
	l, err := NewLayout(t.TempDir(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	m := NewManifest("lab")
	m.Nodes["vm"] = &Node{Kind: "vm", RootDisk: &Disk{Path: "root.qcow2", CapacityBytes: 42}}
	if err := Save(l.ManifestPath, m); err != nil {
		t.Fatal(err)
	}
	got, err := Load(l.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes["vm"].RootDisk.CapacityBytes != 42 {
		t.Fatalf("unexpected manifest: %#v", got)
	}
	if mode := mustStat(t, l.ManifestPath).Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o", mode)
	}
}

func TestLockHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := AcquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = AcquireLock(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestSharedLocksCoexistAndBlockExclusiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := AcquireSharedLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := AcquireSharedLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireLock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exclusive lock error = %v", err)
	}
}

func TestLegacyManifestIsRejectedWithoutMigration(t *testing.T) {
	for _, version := range []int{1, 2} {
		path := filepath.Join(t.TempDir(), "manifest.json")
		data := fmt.Sprintf(`{"version":%d,"topology":"legacy"}`, version)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported manifest version %d", version)) {
			t.Fatalf("legacy manifest version %d error = %v", version, err)
		}
	}
}

func TestResolveWithinRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWithin(base, "escape/file"); err == nil {
		t.Fatal("expected symlink escape rejection")
	}
	p, err := ResolveWithin(base, "safe/new")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(base, "safe/new") {
		t.Fatalf("got %s", p)
	}
}

func TestSocketPathLimit(t *testing.T) {
	_, err := NewLayout(filepath.Join(t.TempDir(), string(make([]byte, 80))), "lab")
	if err == nil {
		t.Fatal("expected long socket path failure")
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	s, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAcquireLockNotifyReportsContentionOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology.lock")
	first, err := AcquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	waiting := 0
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_, err = AcquireLockNotify(ctx, path, func() { waiting++ })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if waiting != 1 {
		t.Fatalf("waiting callback ran %d times, want 1", waiting)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	free := 0
	lock, err := AcquireLockNotify(context.Background(), path, func() { free++ })
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if free != 0 {
		t.Fatalf("callback ran %d times for an uncontended lock", free)
	}
}

func TestAcquireSharedLockNotifyCoexistsAndWaitsForExclusiveHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology.lock")
	shared, err := AcquireSharedLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	waiting := 0
	second, err := AcquireSharedLockNotify(context.Background(), path, func() { waiting++ })
	if err != nil {
		t.Fatal(err)
	}
	if waiting != 0 {
		t.Fatalf("callback ran %d times beside another shared holder", waiting)
	}
	if err := errors.Join(second.Close(), shared.Close()); err != nil {
		t.Fatal(err)
	}
	exclusive, err := AcquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_, err = AcquireSharedLockNotify(ctx, path, func() { waiting++ })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if waiting != 1 {
		t.Fatalf("callback ran %d times, want 1", waiting)
	}
}

func TestLayoutSeparatesNetworkAndVMLocks(t *testing.T) {
	// NewLayout does not create the directory.
	layout, err := NewLayout("/tmp/tl-layout", "lab")
	if err != nil {
		t.Fatal(err)
	}
	if layout.LockPath != filepath.Join(layout.LocksDir, "lab.lock") || layout.VMLockPath != filepath.Join(layout.LocksDir, "lab.vms.lock") {
		t.Fatalf("lock paths = %q, %q", layout.LockPath, layout.VMLockPath)
	}
}

func TestVersion3ManifestIsReadAndSavedAsVersion4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{"version":3,"topology":"lab","updated_at":"2026-11-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != 3 || len(m.Snapshots) != 0 {
		t.Fatalf("loaded manifest = %#v", m)
	}
	if err := Save(path, m); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version": 4`) {
		t.Fatalf("saved manifest:\n%s", data)
	}
	if err := os.WriteFile(path, []byte(`{"version":5,"topology":"lab"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unsupported manifest version 5") {
		t.Fatalf("future manifest error = %v", err)
	}
}

func TestSnapshotNamesAndPaths(t *testing.T) {
	for _, name := range []string{"base", "a", "after-install.2", "x_y", strings.Repeat("a", 63)} {
		if !ValidSnapshotName(name) {
			t.Errorf("ValidSnapshotName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "Base", "-base", ".base", "a/b", "a b", strings.Repeat("a", 64), "base\n"} {
		if ValidSnapshotName(name) {
			t.Errorf("ValidSnapshotName(%q) = true", name)
		}
	}
	layout, err := NewLayout(t.TempDir(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	path, err := layout.SnapshotVarsPath("base", "vm1")
	if err != nil || path != filepath.Join(layout.TopologyDir, "snapshots", "base", "vm1.OVMF_VARS.fd") {
		t.Fatalf("SnapshotVarsPath = %q, %v", path, err)
	}
	if _, err := layout.SnapshotVarsPath("../x", "vm1"); err == nil {
		t.Fatal("an unsafe snapshot name was accepted")
	}
	if _, err := layout.SnapshotVarsPath("base", "../vm1"); err == nil {
		t.Fatal("an unsafe node name was accepted")
	}
}
