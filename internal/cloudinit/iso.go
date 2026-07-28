package cloudinit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return out, nil
}

type ISOBuilder struct {
	Runner  Runner
	Xorriso string
}

func NewISOBuilder(r Runner, executable string) *ISOBuilder {
	if r == nil {
		r = ExecRunner{}
	}
	if executable == "" {
		executable = "xorriso"
	}
	return &ISOBuilder{Runner: r, Xorriso: executable}
}

// Build writes the three canonical NoCloud files and atomically publishes an
// immutable VM seed ISO without replacing an existing seed.
func (b *ISOBuilder) Build(ctx context.Context, destination string, seed Seed) error {
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("seed already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	sourceDir, err := os.MkdirTemp(filepath.Dir(destination), ".seed-source-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(sourceDir)
	for name, data := range map[string][]byte{"meta-data": seed.MetaData, "user-data": seed.UserData, "network-config": seed.NetworkConfig} {
		if err := os.WriteFile(filepath.Join(sourceDir, name), data, 0o600); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".seed-*.iso")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Remove(tmpPath); err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	args := []string{"-as", "mkisofs", "-output", tmpPath, "-volid", "cidata", "-joliet", "-rock", "-graft-points",
		"meta-data=" + filepath.Join(sourceDir, "meta-data"), "user-data=" + filepath.Join(sourceDir, "user-data"), "network-config=" + filepath.Join(sourceDir, "network-config")}
	if _, err := b.Runner.Run(ctx, b.Xorriso, args...); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, tmpPath, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("seed already exists: %s", destination)
		}
		return err
	}
	d, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
