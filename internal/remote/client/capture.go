package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/torob/topolab/internal/app"
	"github.com/torob/topolab/internal/remote/wire"
)

// splitCaptureArgs validates the capture arguments and returns them without
// --output, which the controller handles itself.
func splitCaptureArgs(args []string) ([]string, app.CaptureArgs, error) {
	parsed, err := app.ParseCaptureArgs(args)
	if err != nil {
		return nil, parsed, err
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--output" {
			i++ // the value
			continue
		}
		rest = append(rest, args[i])
	}
	return rest, parsed, nil
}

// createCaptureFile creates the output file exclusively, never following a
// symbolic link and never overwriting. An explicit path needs an existing
// parent directory; the default path creates its directories.
func createCaptureFile(output, topology, link string, now time.Time) (*os.File, string, error) {
	path := output
	if path == "" {
		name := fmt.Sprintf("%s-%s.pcapng", link, now.UTC().Format("20060102T150405.000000000Z"))
		path = filepath.Join("captures", topology, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, "", err
		}
	} else {
		if !strings.EqualFold(filepath.Ext(path), ".pcapng") {
			return nil, "", errors.New("capture output must have .pcapng extension")
		}
		if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
			return nil, "", fmt.Errorf("the directory of %s does not exist", path)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

// countingFile counts the bytes written to the capture file.
type countingFile struct {
	f *os.File
	n int64
}

func (c *countingFile) Write(p []byte) (int, error) {
	n, err := c.f.Write(p)
	c.n += int64(n)
	return n, err
}

var _ io.Writer = (*countingFile)(nil)

// runCapture streams a capture into a file on the controller. The agent sends
// whole pcapng blocks, so a cut stream leaves complete blocks only. An
// interrupt ends the capture normally and exits with status 0.
func (r *Runner) runCapture(ctx context.Context, inv Invocation) int {
	args, parsed, err := splitCaptureArgs(inv.Args)
	if err != nil {
		Print(inv.Stderr, err)
		return ExitUsage
	}
	topology := ""
	if inv.Topology != nil {
		topology = inv.Topology.Name
	}
	file, path, err := createCaptureFile(parsed.Output, topology, parsed.Link, time.Now())
	if err != nil {
		Print(inv.Stderr, err)
		return ExitFailure
	}
	out := &countingFile{f: file}
	inv.Args = args
	finish := func(code int) int {
		_ = file.Close()
		if out.n == 0 {
			_ = os.Remove(path)
		}
		return code
	}
	req, err := BuildRequest(inv)
	if err != nil {
		Print(inv.Stderr, err)
		return finish(ExitFailure)
	}
	sigs, stop := r.Notify()
	defer stop()
	s, err := r.open(ctx, &inv)
	if err != nil {
		Print(inv.Stderr, err)
		return finish(ExitFailure)
	}
	defer s.close()
	if err := s.sendJSON(wire.TypeRequest, req); err != nil {
		Print(inv.Stderr, fmt.Errorf("send request: %w", err))
		return finish(ExitFailure)
	}
	res := r.pump(ctx, s, sigs, pumpConfig{stdout: out, stderr: inv.Stderr, onSignal: cancelThenAbandon})
	code := r.report(inv, res)
	if code == 0 {
		fmt.Fprintln(inv.Stdout, path)
	}
	return finish(code)
}
