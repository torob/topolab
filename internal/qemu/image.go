package qemu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const GiB = uint64(1) << 30

type ImageInfo struct {
	Format              string `json:"format"`
	VirtualSize         uint64 `json:"virtual-size"`
	BackingFilename     string `json:"backing-filename"`
	FullBackingFilename string `json:"full-backing-filename"`
}

type ImageManager struct {
	Runner  Runner
	QEMUImg string
}

func NewImageManager(r Runner, executable string) *ImageManager {
	if r == nil {
		r = ExecRunner{}
	}
	if executable == "" {
		executable = "qemu-img"
	}
	return &ImageManager{Runner: r, QEMUImg: executable}
}

func (m *ImageManager) Info(ctx context.Context, path string) (ImageInfo, error) {
	// Metadata inspection is read-only and must also work for unchanged images
	// currently held open by a running QEMU process.
	out, err := m.Runner.Run(ctx, m.QEMUImg, "info", "--force-share", "--output=json", path)
	if err != nil {
		return ImageInfo{}, err
	}
	var info ImageInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return ImageInfo{}, fmt.Errorf("decode qemu-img info for %s: %w", path, err)
	}
	if info.Format == "" || info.VirtualSize == 0 {
		return ImageInfo{}, fmt.Errorf("incomplete qemu-img info for %s", path)
	}
	return info, nil
}

type TemplateInfo struct {
	Path          string
	Digest        string
	CapacityBytes uint64
}

func (m *ImageManager) ValidateTemplate(ctx context.Context, path string) (TemplateInfo, error) {
	st, err := os.Stat(path)
	if err != nil {
		return TemplateInfo{}, fmt.Errorf("stat template: %w", err)
	}
	if !st.Mode().IsRegular() {
		return TemplateInfo{}, fmt.Errorf("template is not a regular file: %s", path)
	}
	info, err := m.Info(ctx, path)
	if err != nil {
		return TemplateInfo{}, err
	}
	if info.Format != "qcow2" {
		return TemplateInfo{}, fmt.Errorf("template %s has format %q, want qcow2", path, info.Format)
	}
	if info.BackingFilename != "" || info.FullBackingFilename != "" {
		return TemplateInfo{}, fmt.Errorf("template %s has a backing file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return TemplateInfo{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return TemplateInfo{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return TemplateInfo{}, err
	}
	return TemplateInfo{Path: filepath.Clean(abs), Digest: hex.EncodeToString(h.Sum(nil)), CapacityBytes: info.VirtualSize}, nil
}

func GiBToBytes(size uint64) (uint64, error) {
	if size > math.MaxUint64/GiB {
		return 0, errors.New("disk size overflows bytes")
	}
	return size * GiB, nil
}

// CreateOverlay atomically creates a qcow2 overlay with an absolute backing
// path. An existing destination is never overwritten.
func (m *ImageManager) CreateOverlay(ctx context.Context, destination string, template TemplateInfo, capacity uint64) error {
	if capacity != 0 && capacity < template.CapacityBytes {
		return errors.New("overlay capacity is smaller than template")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	tmp, err := tempImageName(destination)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	args := []string{"create", "-f", "qcow2", "-F", "qcow2", "-b", template.Path, tmp}
	if capacity != 0 {
		args = append(args, fmt.Sprintf("%d", capacity))
	}
	if _, err := m.Runner.Run(ctx, m.QEMUImg, args...); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := renameNoReplace(tmp, destination); err != nil {
		return err
	}
	return syncDir(filepath.Dir(destination))
}

func (m *ImageManager) CreateDataDisk(ctx context.Context, destination string, capacity uint64) error {
	if capacity == 0 {
		return errors.New("disk capacity must be positive")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	tmp, err := tempImageName(destination)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := m.Runner.Run(ctx, m.QEMUImg, "create", "-f", "qcow2", tmp, fmt.Sprintf("%d", capacity)); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := renameNoReplace(tmp, destination); err != nil {
		return err
	}
	return syncDir(filepath.Dir(destination))
}

// CreateCopy atomically creates a standalone sparse qcow2 image from a
// template. Unlike an overlay it never retains a backing-file dependency.
func (m *ImageManager) CreateCopy(ctx context.Context, destination string, template TemplateInfo, capacity uint64) error {
	if capacity < template.CapacityBytes {
		return errors.New("copy capacity is smaller than template")
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	tmp, err := tempImageName(destination)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := m.Runner.Run(ctx, m.QEMUImg, "convert", "-f", "qcow2", "-O", "qcow2", "-S", "4k", template.Path, tmp); err != nil {
		return err
	}
	info, err := m.Info(ctx, tmp)
	if err != nil {
		return err
	}
	if capacity > info.VirtualSize {
		if _, err := m.Runner.Run(ctx, m.QEMUImg, "resize", tmp, fmt.Sprintf("%d", capacity)); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := renameNoReplace(tmp, destination); err != nil {
		return err
	}
	return syncDir(filepath.Dir(destination))
}

// ConvertToRaw writes a qcow2 template into an already allocated block
// device. The destination is explicit and is never created or removed here.
func (m *ImageManager) ConvertToRaw(ctx context.Context, source, destination string) error {
	_, err := m.Runner.Run(ctx, m.QEMUImg, "convert", "-f", "qcow2", "-O", "raw", "-S", "4k", source, destination)
	return err
}

type Growth struct {
	Path, BlockNode string
	Actual, Desired uint64
}

type BlockResizer interface {
	BlockResize(context.Context, string, uint64) error
}

// ResizeOnline grows the QEMU block node and verifies the qcow2 virtual size.
// It never invokes qemu-img resize on an image opened by QEMU.
func (m *ImageManager) ResizeOnline(ctx context.Context, qmp BlockResizer, growth Growth) error {
	if qmp == nil {
		return errors.New("QMP block resizer is required")
	}
	if growth.Desired < growth.Actual {
		return errors.New("refusing disk shrink")
	}
	if growth.Desired == growth.Actual {
		return nil
	}
	if growth.BlockNode == "" {
		return errors.New("block node is required for online growth")
	}
	if err := qmp.BlockResize(ctx, growth.BlockNode, growth.Desired); err != nil {
		return err
	}
	info, err := m.Info(ctx, growth.Path)
	if err != nil {
		return err
	}
	if info.VirtualSize != growth.Desired {
		return fmt.Errorf("online resize verification failed for %s: got %d, want %d", growth.Path, info.VirtualSize, growth.Desired)
	}
	return nil
}

// ValidateGrowthPlan reads every image before returning any operation, so
// callers can honor the all-preflight-before-mutation requirement.
func (m *ImageManager) ValidateGrowthPlan(ctx context.Context, desired map[string]uint64, blockNodes map[string]string) ([]Growth, error) {
	plan := make([]Growth, 0, len(desired))
	for path, want := range desired {
		info, err := m.Info(ctx, path)
		if err != nil {
			return nil, err
		}
		if want < info.VirtualSize {
			return nil, fmt.Errorf("refusing to shrink %s from %d to %d bytes", path, info.VirtualSize, want)
		}
		if want > info.VirtualSize {
			plan = append(plan, Growth{Path: path, BlockNode: blockNodes[path], Actual: info.VirtualSize, Desired: want})
		}
	}
	return plan, nil
}

func (m *ImageManager) ResizeOffline(ctx context.Context, growth Growth) error {
	if growth.Desired < growth.Actual {
		return errors.New("refusing disk shrink")
	}
	if growth.Desired == growth.Actual {
		return nil
	}
	if _, err := m.Runner.Run(ctx, m.QEMUImg, "resize", growth.Path, fmt.Sprintf("%d", growth.Desired)); err != nil {
		return err
	}
	info, err := m.Info(ctx, growth.Path)
	if err != nil {
		return err
	}
	if info.VirtualSize != growth.Desired {
		return fmt.Errorf("resize verification failed for %s: got %d, want %d", growth.Path, info.VirtualSize, growth.Desired)
	}
	return nil
}

func CopyFileAtomic(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(destination), ".copy-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := out.Chmod(mode); err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(destination)); err != nil {
		return err
	}
	ok = true
	return nil
}

func tempImageName(destination string) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(destination), ".image-*.qcow2")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func renameNoReplace(source, destination string) error {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("destination already exists: %s", destination)
	}
	return err
}
