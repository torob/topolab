package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ProbeIOUring verifies that the selected QEMU executable can initialize its
// io_uring file backend on the running kernel. Backend-specific disk and
// filesystem compatibility is still validated when the VM opens its disks.
func ProbeIOUring(ctx context.Context, executable string) error {
	if executable == "" {
		executable = "qemu-system-x86_64"
	}
	probe, err := os.CreateTemp("", "topolab-io-uring-probe-*")
	if err != nil {
		return fmt.Errorf("create io_uring probe file: %w", err)
	}
	path := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close io_uring probe file: %w", closeErr)
	}
	defer os.Remove(path)

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := []string{
		"-machine", "none", "-display", "none", "-nodefaults", "-no-user-config",
		"-serial", "none", "-monitor", "none", "-S", "-qmp", "stdio",
		"-blockdev", "driver=file,node-name=topolab-io-uring-probe,filename=" + escapeComma(path) + ",cache.direct=off,cache.no-flush=off,aio=io_uring",
	}
	cmd := exec.CommandContext(probeCtx, executable, args...)
	cmd.Stdin = strings.NewReader(`{"execute":"qmp_capabilities"}` + "\n" + `{"execute":"quit"}` + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if probeCtx.Err() != nil {
			return fmt.Errorf("probe io_uring AIO: %w", probeCtx.Err())
		}
		return fmt.Errorf("probe io_uring AIO: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
