// Package blob is the host-side store of uploaded templates, addressed by their
// SHA-256 digest (spec.md Section 12.5).
package blob

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/torob/topolab/internal/config"
	"github.com/torob/topolab/internal/fsx"
	"github.com/torob/topolab/internal/state"
	"golang.org/x/sys/unix"
)

// States of a digest.
const (
	StatePresent = "present"
	StatePartial = "partial"
	StateAbsent  = "absent"
)

// Errors that the agent maps to protocol error codes.
var (
	ErrDigestMismatch = errors.New("uploaded data does not match its digest")
	ErrInvalid        = errors.New("uploaded file is not a qcow2 image without a backing file")
	ErrNoSpace        = errors.New("not enough space for the upload")
	ErrCorrupt        = errors.New("stored blob does not match its name")
	ErrOffset         = errors.New("upload offset does not match the stored data")
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidDigest reports whether s is a lowercase hexadecimal SHA-256 digest.
func ValidDigest(s string) bool { return digestPattern.MatchString(s) }

// Store is the blob store of one working directory.
type Store struct {
	dir string
	// fallocate reserves space; tests replace it.
	fallocate func(fd int, mode uint32, off, length int64) error
}

// NewStore returns the store under <workingDir>/.topolab/blobs.
func NewStore(workingDir string) *Store {
	return &Store{dir: filepath.Join(workingDir, ".topolab", "blobs"), fallocate: unix.Fallocate}
}

// Dir is the store directory.
func (s *Store) Dir() string { return s.dir }

// Path is the path of a completed blob. It depends only on the digest.
func (s *Store) Path(digest string) string { return filepath.Join(s.dir, "sha256", digest) }

func (s *Store) partialPath(digest string) string { return filepath.Join(s.dir, "partial", digest) }
func (s *Store) lockPath(digest string) string    { return filepath.Join(s.dir, "locks", digest+".lock") }
func (s *Store) gcLockPath() string               { return filepath.Join(s.dir, ".gc.lock") }

// Ensure creates the store directories with root-only permissions.
func (s *Store) Ensure() error {
	for _, d := range []string{filepath.Dir(s.dir), s.dir, filepath.Join(s.dir, "sha256"), filepath.Join(s.dir, "partial"), filepath.Join(s.dir, "locks")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return fmt.Errorf("protect %s: %w", d, err)
		}
	}
	return nil
}

// Status reports whether a digest is present, partly uploaded, or absent. For a
// partial upload the offset is the size of the partial file.
func (s *Store) Status(digest string) (string, int64, error) {
	if !ValidDigest(digest) {
		return "", 0, fmt.Errorf("invalid digest %q", digest)
	}
	if info, err := os.Lstat(s.Path(digest)); err == nil {
		if !info.Mode().IsRegular() {
			return "", 0, fmt.Errorf("%w: %s is not a regular file", ErrCorrupt, s.Path(digest))
		}
		return StatePresent, info.Size(), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", 0, err
	}
	if info, err := os.Lstat(s.partialPath(digest)); err == nil {
		if !info.Mode().IsRegular() {
			return "", 0, fmt.Errorf("%w: %s is not a regular file", ErrCorrupt, s.partialPath(digest))
		}
		return StatePartial, info.Size(), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", 0, err
	}
	return StateAbsent, 0, nil
}

// Touch records that a blob was used.
func (s *Store) Touch(digest string, now time.Time) {
	if ValidDigest(digest) {
		_ = os.Chtimes(s.Path(digest), now, now)
	}
}

// Slot is the exclusive claim on one digest. Holding it serializes uploaders.
type Slot struct {
	store  *Store
	Digest string
	Size   int64
	State  string
	Offset int64
	lock   *state.Lock
}

// Acquire takes the per-digest lock, waiting while another uploader holds it,
// and reports the digest's state. A partial file that is larger than the
// declared size cannot be a prefix of the blob and is discarded.
func (s *Store) Acquire(ctx context.Context, digest string, size int64) (*Slot, error) {
	if !ValidDigest(digest) {
		return nil, fmt.Errorf("invalid digest %q", digest)
	}
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	lock, err := state.AcquireLock(ctx, s.lockPath(digest))
	if err != nil {
		return nil, err
	}
	st, off, err := s.Status(digest)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if st == StatePartial && off > size {
		if err := os.Remove(s.partialPath(digest)); err != nil {
			_ = lock.Close()
			return nil, err
		}
		st, off = StateAbsent, 0
	}
	return &Slot{store: s, Digest: digest, Size: size, State: st, Offset: off, lock: lock}, nil
}

// Release gives up the claim. A partial file stays for a later resume.
func (sl *Slot) Release() {
	if sl.lock != nil {
		_ = sl.lock.Close()
		sl.lock = nil
	}
}

// Open starts or resumes an upload. The slot must be partial or absent.
func (sl *Slot) Open() (*Upload, error) {
	if sl.State == StatePresent {
		return nil, errors.New("blob is already present")
	}
	f, err := os.OpenFile(sl.store.partialPath(sl.Digest), os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if sl.Size > sl.Offset {
		if err := sl.store.fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, sl.Size); err != nil {
			switch {
			case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
				_ = f.Close()
				return nil, fmt.Errorf("%w: %v", ErrNoSpace, err)
			case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL):
				// The file system cannot reserve space; the upload goes on.
			default:
				_ = f.Close()
				return nil, err
			}
		}
	}
	return &Upload{slot: sl, f: f, offset: sl.Offset}, nil
}

// Upload receives the bytes of one blob.
type Upload struct {
	slot   *Slot
	f      *os.File
	offset int64
}

// Offset is the number of bytes stored so far.
func (u *Upload) Offset() int64 { return u.offset }

// Write stores p at offset, which must equal the current size.
func (u *Upload) Write(offset int64, p []byte) error {
	if offset != u.offset {
		return fmt.Errorf("%w: got offset %d, want %d", ErrOffset, offset, u.offset)
	}
	if u.offset+int64(len(p)) > u.slot.Size {
		return fmt.Errorf("%w: data runs past the declared size of %d bytes", ErrOffset, u.slot.Size)
	}
	if _, err := u.f.WriteAt(p, offset); err != nil {
		if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
			return fmt.Errorf("%w: %v", ErrNoSpace, err)
		}
		return err
	}
	u.offset += int64(len(p))
	return nil
}

// Abort closes the file and keeps the partial data for a later resume.
func (u *Upload) Abort() { _ = u.f.Close() }

// Finish checks the data and moves it into place. On a digest mismatch or an
// invalid image it deletes the partial file.
func (u *Upload) Finish() error {
	s, digest := u.slot.store, u.slot.Digest
	partial := s.partialPath(digest)
	if u.offset != u.slot.Size {
		_ = u.f.Close()
		return fmt.Errorf("%w: received %d of %d bytes", ErrOffset, u.offset, u.slot.Size)
	}
	if err := u.f.Sync(); err != nil {
		_ = u.f.Close()
		return err
	}
	if err := u.f.Close(); err != nil {
		return err
	}
	got, _, err := fsx.HashFile(partial)
	if err != nil {
		return err
	}
	if got != digest {
		_ = os.Remove(partial)
		return fmt.Errorf("%w: got %s", ErrDigestMismatch, got)
	}
	if err := config.ValidateTemplateFiles([]*config.Template{{Name: digest[:12], Path: partial}}); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := os.Chmod(partial, 0o444); err != nil {
		return err
	}
	if err := fsx.RenameNoReplace(partial, s.Path(digest)); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another copy completed first; the data is identical.
			_ = os.Remove(partial)
			return nil
		}
		return err
	}
	if err := fsx.SyncDir(filepath.Dir(partial)); err != nil {
		return err
	}
	return fsx.SyncDir(filepath.Dir(s.Path(digest)))
}

// GCShared takes .gc.lock in shared mode. An `up` holds it from its blob check
// until its job is registered, so that prune cannot remove a blob it selected.
func (s *Store) GCShared(ctx context.Context) (*state.Lock, error) {
	if err := s.Ensure(); err != nil {
		return nil, err
	}
	return state.AcquireSharedLock(ctx, s.gcLockPath())
}

// Info describes one stored blob.
type Info struct {
	Digest   string
	Size     int64
	LastUsed time.Time
}

// List returns the completed blobs ordered by digest.
func (s *Store) List() ([]Info, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "sha256"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !ValidDigest(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, Info{Digest: e.Name(), Size: info.Size(), LastUsed: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}

// digestOf returns the blob digest that path names, or "".
func (s *Store) digestOf(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	if filepath.Dir(clean) != filepath.Join(s.dir, "sha256") || !ValidDigest(filepath.Base(clean)) {
		return ""
	}
	return filepath.Base(clean)
}

// References maps each referenced digest to the topologies that reference it.
// A reference is a manifest's template path or digest, or the backing file in
// the qcow2 header of any file under a topology directory (which also covers an
// overlay that exists before its manifest entry does).
func (s *Store) References(workingDir string) (map[string]map[string]bool, error) {
	refs := map[string]map[string]bool{}
	add := func(digest, topology string) {
		if !ValidDigest(digest) {
			return
		}
		if refs[digest] == nil {
			refs[digest] = map[string]bool{}
		}
		refs[digest][topology] = true
	}
	topologies := filepath.Join(workingDir, ".topolab", "topologies")
	entries, err := os.ReadDir(topologies)
	if errors.Is(err, fs.ErrNotExist) {
		return refs, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dir := filepath.Join(topologies, name)
		manifest, err := state.Load(filepath.Join(dir, "manifest.json"))
		if err != nil {
			return nil, fmt.Errorf("read manifest of %s: %w", name, err)
		}
		if manifest != nil {
			for _, node := range manifest.Nodes {
				if node == nil {
					continue
				}
				add(s.digestOf(node.TemplatePath), name)
				add(node.TemplateDigest, name)
			}
		}
		err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".qcow2") {
				return nil
			}
			add(s.digestOf(backingFile(path)), name)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// backingFile reads the backing file name from a qcow2 header, or "".
func backingFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	header := make([]byte, 20)
	if _, err := f.ReadAt(header, 0); err != nil || string(header[:4]) != "QFI\xfb" {
		return ""
	}
	offset := binary.BigEndian.Uint64(header[8:16])
	size := binary.BigEndian.Uint32(header[16:20])
	if offset == 0 || size == 0 || size > 1023 {
		return ""
	}
	name := make([]byte, size)
	if _, err := f.ReadAt(name, int64(offset)); err != nil {
		return ""
	}
	return string(name)
}

// PruneOptions control Prune.
type PruneOptions struct {
	// MinAge is how long a blob or partial file must be unused to be removed.
	MinAge time.Duration
	Now    time.Time
	// Protected holds digests that a running job's request names.
	Protected map[string]bool
	// LockWait bounds the wait for .gc.lock.
	LockWait time.Duration
}

// PruneResult lists what Prune did.
type PruneResult struct {
	Removed        []string
	RemovedPartial []string
}

// Prune removes unprotected blobs and stale partial files.
func (s *Store) Prune(ctx context.Context, workingDir string, opts PruneOptions) (PruneResult, error) {
	var res PruneResult
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	wait := opts.LockWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	if err := s.Ensure(); err != nil {
		return res, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, wait)
	gc, err := state.AcquireLock(lockCtx, s.gcLockPath())
	cancel()
	if err != nil {
		return res, fmt.Errorf("another upload or prune holds the blob store: %w", err)
	}
	defer gc.Close()
	refs, err := s.References(workingDir)
	if err != nil {
		return res, err
	}
	blobs, err := s.List()
	if err != nil {
		return res, err
	}
	for _, b := range blobs {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if refs[b.Digest] != nil || opts.Protected[b.Digest] {
			continue
		}
		if opts.Now.Sub(b.LastUsed) < opts.MinAge {
			continue
		}
		lock, ok := s.tryLock(b.Digest)
		if !ok {
			continue
		}
		err := os.Remove(s.Path(b.Digest))
		if err == nil {
			_ = os.Remove(s.lockPath(b.Digest))
			res.Removed = append(res.Removed, b.Digest)
		}
		_ = lock.Close()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
	}
	partials, err := os.ReadDir(filepath.Join(s.dir, "partial"))
	if err != nil {
		return res, err
	}
	for _, e := range partials {
		if !ValidDigest(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || opts.Now.Sub(info.ModTime()) < opts.MinAge {
			continue
		}
		lock, ok := s.tryLock(e.Name())
		if !ok {
			continue
		}
		if err := os.Remove(s.partialPath(e.Name())); err == nil {
			res.RemovedPartial = append(res.RemovedPartial, e.Name())
		}
		_ = lock.Close()
	}
	return res, nil
}

// tryLock takes a digest lock without waiting.
func (s *Store) tryLock(digest string) (*state.Lock, bool) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lock, err := state.AcquireLock(ctx, s.lockPath(digest))
	return lock, err == nil
}
