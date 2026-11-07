package blob

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torob/topolab/internal/state"
	"golang.org/x/sys/unix"
)

func qcow2(payload string) []byte {
	h := make([]byte, 24)
	copy(h, "QFI\xfb")
	binary.BigEndian.PutUint32(h[4:8], 3)
	return append(h, payload...)
}

func qcow2WithBacking(name string) []byte {
	h := make([]byte, 24)
	copy(h, "QFI\xfb")
	binary.BigEndian.PutUint32(h[4:8], 3)
	binary.BigEndian.PutUint64(h[8:16], 24)
	binary.BigEndian.PutUint32(h[16:20], uint32(len(name)))
	return append(h, name...)
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func upload(t *testing.T, s *Store, data []byte) string {
	t.Helper()
	digest := digestOf(data)
	slot, err := s.Acquire(context.Background(), digest, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer slot.Release()
	if slot.State == StatePresent {
		return digest
	}
	u, err := slot.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Write(u.Offset(), data[u.Offset():]); err != nil {
		t.Fatal(err)
	}
	if err := u.Finish(); err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestUploadStoresAReadOnlyBlobByDigest(t *testing.T) {
	wd := t.TempDir()
	s := NewStore(wd)
	data := qcow2(strings.Repeat("image data ", 100))
	digest := digestOf(data)

	if st, off, err := s.Status(digest); err != nil || st != StateAbsent || off != 0 {
		t.Fatalf("initial status = %s %d %v", st, off, err)
	}
	slot, err := s.Acquire(context.Background(), digest, int64(len(data)))
	if err != nil || slot.State != StateAbsent {
		t.Fatalf("slot = %+v %v", slot, err)
	}
	u, err := slot.Open()
	if err != nil {
		t.Fatal(err)
	}
	half := len(data) / 2
	if err := u.Write(0, data[:half]); err != nil {
		t.Fatal(err)
	}
	if err := u.Write(int64(half), data[half:]); err != nil {
		t.Fatal(err)
	}
	if err := u.Finish(); err != nil {
		t.Fatal(err)
	}
	slot.Release()

	path := s.Path(digest)
	if path != filepath.Join(wd, ".topolab", "blobs", "sha256", digest) {
		t.Fatalf("path = %s", path)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o444 || info.Size() != int64(len(data)) {
		t.Fatalf("blob: %v %v", info, err)
	}
	if st, _, err := s.Status(digest); err != nil || st != StatePresent {
		t.Fatalf("status after upload = %s %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(wd, ".topolab", "blobs", "partial", digest)); !os.IsNotExist(err) {
		t.Fatalf("partial file remains: %v", err)
	}
	for _, d := range []string{s.Dir(), filepath.Join(s.Dir(), "sha256"), filepath.Join(s.Dir(), "partial"), filepath.Join(s.Dir(), "locks")} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi, err)
		}
	}
	// A present digest needs no upload.
	slot, err = s.Acquire(context.Background(), digest, int64(len(data)))
	if err != nil || slot.State != StatePresent {
		t.Fatalf("second acquire = %+v %v", slot, err)
	}
	slot.Release()
	if _, err := (&Slot{State: StatePresent}).Open(); err == nil {
		t.Fatal("Open on a present slot succeeded")
	}
}

func TestInterruptedUploadResumesFromTheStoredSize(t *testing.T) {
	s := NewStore(t.TempDir())
	data := qcow2(strings.Repeat("resume ", 200))
	digest := digestOf(data)
	slot, err := s.Acquire(context.Background(), digest, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	u, err := slot.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Write(0, data[:500]); err != nil {
		t.Fatal(err)
	}
	u.Abort()
	slot.Release()

	slot, err = s.Acquire(context.Background(), digest, int64(len(data)))
	if err != nil || slot.State != StatePartial || slot.Offset != 500 {
		t.Fatalf("resumed slot = %+v %v", slot, err)
	}
	u, err = slot.Open()
	if err != nil || u.Offset() != 500 {
		t.Fatalf("resumed upload offset = %d %v", u.Offset(), err)
	}
	if err := u.Write(500, data[500:]); err != nil {
		t.Fatal(err)
	}
	if err := u.Finish(); err != nil {
		t.Fatal(err)
	}
	slot.Release()
	got, _ := os.ReadFile(s.Path(digest))
	if string(got) != string(data) {
		t.Fatal("resumed blob differs from the source")
	}
}

func TestUploadRejectionsLeaveNoBlobAndNoPartial(t *testing.T) {
	data := qcow2("payload")
	tests := map[string]struct {
		send   func(t *testing.T, u *Upload)
		digest string
		size   int64
		want   error
	}{
		"digest mismatch": {
			digest: strings.Repeat("0", 64), size: int64(len(data)),
			send: func(t *testing.T, u *Upload) {
				if err := u.Write(0, data); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrDigestMismatch,
		},
		"backing file": {
			digest: digestOf(qcow2WithBacking("/etc/passwd")), size: int64(len(qcow2WithBacking("/etc/passwd"))),
			send: func(t *testing.T, u *Upload) {
				if err := u.Write(0, qcow2WithBacking("/etc/passwd")); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrInvalid,
		},
		"not qcow2": {
			digest: digestOf([]byte("plain text that is long enough to be read as a header")), size: int64(len("plain text that is long enough to be read as a header")),
			send: func(t *testing.T, u *Upload) {
				if err := u.Write(0, []byte("plain text that is long enough to be read as a header")); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrInvalid,
		},
		"incomplete": {
			digest: digestOf(data), size: int64(len(data)),
			send: func(t *testing.T, u *Upload) {
				if err := u.Write(0, data[:10]); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrOffset,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := NewStore(t.TempDir())
			slot, err := s.Acquire(context.Background(), tt.digest, tt.size)
			if err != nil {
				t.Fatal(err)
			}
			defer slot.Release()
			u, err := slot.Open()
			if err != nil {
				t.Fatal(err)
			}
			tt.send(t, u)
			if err := u.Finish(); !errors.Is(err, tt.want) {
				t.Fatalf("Finish err = %v, want %v", err, tt.want)
			}
			if _, err := os.Stat(s.Path(tt.digest)); !os.IsNotExist(err) {
				t.Fatalf("a blob was stored: %v", err)
			}
			if tt.want != ErrOffset {
				if _, err := os.Stat(filepath.Join(s.Dir(), "partial", tt.digest)); !os.IsNotExist(err) {
					t.Fatalf("partial file remains: %v", err)
				}
			}
		})
	}
}

func TestWriteValidatesOffsetAndSize(t *testing.T) {
	s := NewStore(t.TempDir())
	data := qcow2("abc")
	slot, err := s.Acquire(context.Background(), digestOf(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer slot.Release()
	u, err := slot.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Write(5, data); !errors.Is(err, ErrOffset) {
		t.Fatalf("wrong offset err = %v", err)
	}
	if err := u.Write(0, append(append([]byte(nil), data...), 'x')); !errors.Is(err, ErrOffset) {
		t.Fatalf("overflow err = %v", err)
	}
	if u.Offset() != 0 {
		t.Fatalf("offset moved to %d after rejected writes", u.Offset())
	}
}

func TestOversizedPartialIsDiscarded(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	digest := digestOf(qcow2("x"))
	if err := os.WriteFile(filepath.Join(s.Dir(), "partial", digest), make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	slot, err := s.Acquire(context.Background(), digest, 100)
	if err != nil || slot.State != StateAbsent || slot.Offset != 0 {
		t.Fatalf("slot = %+v %v", slot, err)
	}
	slot.Release()
	if _, err := os.Stat(filepath.Join(s.Dir(), "partial", digest)); !os.IsNotExist(err) {
		t.Fatalf("oversized partial remains: %v", err)
	}
}

func TestNoSpaceIsReportedWhenReservationFails(t *testing.T) {
	s := NewStore(t.TempDir())
	s.fallocate = func(int, uint32, int64, int64) error { return unix.ENOSPC }
	data := qcow2("abc")
	slot, err := s.Acquire(context.Background(), digestOf(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer slot.Release()
	if _, err := slot.Open(); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Open err = %v", err)
	}
	// A file system that cannot reserve space does not stop the upload.
	s.fallocate = func(int, uint32, int64, int64) error { return unix.EOPNOTSUPP }
	if _, err := slot.Open(); err != nil {
		t.Fatalf("Open with unsupported fallocate: %v", err)
	}
}

func TestSecondUploaderWaitsForTheFirst(t *testing.T) {
	s := NewStore(t.TempDir())
	data := qcow2("shared")
	digest := digestOf(data)
	first, err := s.Acquire(context.Background(), digest, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *Slot, 1)
	go func() {
		slot, err := s.Acquire(context.Background(), digest, int64(len(data)))
		if err != nil {
			t.Error(err)
		}
		got <- slot
	}()
	select {
	case <-got:
		t.Fatal("second uploader did not wait for the lock")
	case <-time.After(150 * time.Millisecond):
	}
	u, err := first.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Write(0, data); err != nil {
		t.Fatal(err)
	}
	if err := u.Finish(); err != nil {
		t.Fatal(err)
	}
	first.Release()
	select {
	case slot := <-got:
		if slot == nil || slot.State != StatePresent {
			t.Fatalf("second slot = %+v, want present", slot)
		}
		slot.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("second uploader never got the lock")
	}
}

func TestAcquireHonorsContext(t *testing.T) {
	s := NewStore(t.TempDir())
	digest := digestOf(qcow2("c"))
	first, err := s.Acquire(context.Background(), digest, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(ctx, digest, 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Acquire(context.Background(), "nothex", 1); err == nil {
		t.Fatal("invalid digest accepted")
	}
}

func TestListIsOrderedAndTouchUpdatesLastUse(t *testing.T) {
	s := NewStore(t.TempDir())
	a := upload(t, s, qcow2("aaa"))
	b := upload(t, s, qcow2("bbbb"))
	list, err := s.List()
	if err != nil || len(list) != 2 || list[0].Digest > list[1].Digest {
		t.Fatalf("list = %+v %v", list, err)
	}
	if list[0].Digest != min(a, b) {
		t.Fatalf("not ordered: %+v", list)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.Path(a), old, old); err != nil {
		t.Fatal(err)
	}
	s.Touch(a, time.Now())
	list, _ = s.List()
	for _, info := range list {
		if time.Since(info.LastUsed) > time.Minute {
			t.Fatalf("%s last used %v", info.Digest, info.LastUsed)
		}
	}
	if empty, err := NewStore(t.TempDir()).List(); err != nil || len(empty) != 0 {
		t.Fatalf("empty store list = %v %v", empty, err)
	}
}

// topologyWith builds a topology directory with a manifest and optional files.
func topologyWith(t *testing.T, wd, name string, nodes map[string]*state.Node, files map[string][]byte) {
	t.Helper()
	dir := filepath.Join(wd, ".topolab", "topologies", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if nodes != nil {
		m := state.NewManifest(name)
		m.Nodes = nodes
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for rel, data := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func ageBlob(t *testing.T, s *Store, digest string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age)
	if err := os.Chtimes(s.Path(digest), old, old); err != nil {
		t.Fatal(err)
	}
}

func TestReferencesAndPruneProtection(t *testing.T) {
	wd := t.TempDir()
	s := NewStore(wd)
	byManifest := upload(t, s, qcow2("manifest-ref"))
	byOverlay := upload(t, s, qcow2("overlay-ref"))
	byDigestField := upload(t, s, qcow2("digest-field-ref"))
	byJob := upload(t, s, qcow2("job-ref"))
	young := upload(t, s, qcow2("young"))
	unused := upload(t, s, qcow2("unused"))
	for _, d := range []string{byManifest, byOverlay, byDigestField, byJob, unused} {
		ageBlob(t, s, d, 72*time.Hour)
	}
	topologyWith(t, wd, "lab",
		map[string]*state.Node{
			"vm1": {Kind: "vm", TemplatePath: s.Path(byManifest)},
			"vm2": {Kind: "vm", TemplatePath: "/elsewhere/image.qcow2", TemplateDigest: byDigestField},
		},
		map[string][]byte{"nodes/vm3/root.qcow2": qcow2WithBacking(s.Path(byOverlay))},
	)
	// A topology without a manifest, whose overlay is the only reference.
	topologyWith(t, wd, "partial", nil, nil)

	refs, err := s.References(wd)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{byManifest, byOverlay, byDigestField} {
		if !refs[d]["lab"] {
			t.Fatalf("%s is not referenced by lab: %+v", d, refs)
		}
	}
	if refs[unused] != nil || refs[byJob] != nil {
		t.Fatalf("unreferenced blobs have references: %+v", refs)
	}

	res, err := s.Prune(context.Background(), wd, PruneOptions{MinAge: 24 * time.Hour, Protected: map[string]bool{byJob: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != unused {
		t.Fatalf("removed %v, want only %s", res.Removed, unused)
	}
	for _, d := range []string{byManifest, byOverlay, byDigestField, byJob, young} {
		if _, err := os.Stat(s.Path(d)); err != nil {
			t.Fatalf("protected blob %s was removed: %v", d, err)
		}
	}
	if _, err := os.Stat(s.Path(unused)); !os.IsNotExist(err) {
		t.Fatalf("unused blob remains: %v", err)
	}
}

func TestPruneSkipsBusyDigestsAndRemovesStalePartials(t *testing.T) {
	wd := t.TempDir()
	s := NewStore(wd)
	busy := upload(t, s, qcow2("busy"))
	ageBlob(t, s, busy, 72*time.Hour)
	slot, err := s.Acquire(context.Background(), busy, 100)
	if err != nil {
		t.Fatal(err)
	}
	stale := digestOf([]byte("stale"))
	fresh := digestOf([]byte("fresh"))
	for _, d := range []string{stale, fresh} {
		if err := os.WriteFile(filepath.Join(s.Dir(), "partial", d), []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(s.Dir(), "partial", stale), old, old); err != nil {
		t.Fatal(err)
	}
	res, err := s.Prune(context.Background(), wd, PruneOptions{MinAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("a busy blob was removed: %v", res.Removed)
	}
	if len(res.RemovedPartial) != 1 || res.RemovedPartial[0] != stale {
		t.Fatalf("removed partials = %v", res.RemovedPartial)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "partial", fresh)); err != nil {
		t.Fatalf("a fresh partial was removed: %v", err)
	}
	slot.Release()
	res, err = s.Prune(context.Background(), wd, PruneOptions{MinAge: 24 * time.Hour})
	if err != nil || len(res.Removed) != 1 || res.Removed[0] != busy {
		t.Fatalf("after release: %+v %v", res, err)
	}
}

func TestPruneWaitsForUploadsThatHoldTheGCLock(t *testing.T) {
	wd := t.TempDir()
	s := NewStore(wd)
	shared, err := s.GCShared(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GCShared(context.Background())
	if err != nil {
		t.Fatalf("two uploads must hold .gc.lock together: %v", err)
	}
	_ = second.Close()
	_, err = s.Prune(context.Background(), wd, PruneOptions{MinAge: time.Hour, LockWait: 100 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "another upload or prune") {
		t.Fatalf("Prune err = %v", err)
	}
	_ = shared.Close()
	if _, err := s.Prune(context.Background(), wd, PruneOptions{MinAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
}

func TestStatusRejectsNonRegularBlobs(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Ensure(); err != nil {
		t.Fatal(err)
	}
	digest := digestOf(qcow2("link"))
	if err := os.Symlink("/etc/passwd", s.Path(digest)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Status(digest); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("symlink blob err = %v", err)
	}
	if _, _, err := s.Status("zz"); err == nil {
		t.Fatal("invalid digest accepted")
	}
}
