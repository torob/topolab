package state

import (
	"context"
	"errors"
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
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"topology":"legacy","host_interfaces":{"management":{"logical_name":"management"}},"network_state":{"topology":"legacy"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unsupported manifest version 1") {
		t.Fatalf("legacy manifest error = %v", err)
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
