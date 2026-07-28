package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/torob/topolab/internal/state"
	"golang.org/x/sys/unix"
)

type PacketSource interface {
	ReadPacket(context.Context) (Packet, error)
	Close() error
}
type CaptureOptions struct {
	Duration       time.Duration
	Count          uint64
	OwnershipCheck func(context.Context) error
	CheckInterval  time.Duration
}

// Capture multiplexes one or two directional sources into one pcapng writer.
// Cancellation (including SIGINT propagated by the CLI) closes a valid file
// and is considered a successful stop.
func Capture(ctx context.Context, writer *PcapNGWriter, sources []PacketSource, options CaptureOptions) error {
	if writer == nil || len(sources) == 0 {
		return errors.New("capture needs writer and packet source")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if options.Duration > 0 {
		var dcancel context.CancelFunc
		runCtx, dcancel = context.WithTimeout(runCtx, options.Duration)
		defer dcancel()
	}
	ownershipErr := make(chan error, 1)
	if options.OwnershipCheck != nil {
		interval := options.CheckInterval
		if interval <= 0 {
			interval = 250 * time.Millisecond
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-ticker.C:
					if err := options.OwnershipCheck(runCtx); err != nil {
						if runCtx.Err() != nil {
							return
						}
						select {
						case ownershipErr <- fmt.Errorf("capture datapath ownership changed: %w", err):
						default:
						}
						cancel()
						return
					}
				}
			}
		}()
	}
	type result struct {
		packet Packet
		err    error
	}
	ch := make(chan result, len(sources))
	var wg sync.WaitGroup
	for _, source := range sources {
		wg.Add(1)
		go func(s PacketSource) {
			defer wg.Done()
			for {
				p, err := s.ReadPacket(runCtx)
				select {
				case ch <- result{p, err}:
				case <-runCtx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}(source)
	}
	go func() { wg.Wait(); close(ch) }()
	var count uint64
	var captureErr error
	for item := range ch {
		if item.err != nil {
			if runCtx.Err() == nil && !errors.Is(item.err, context.Canceled) && !errors.Is(item.err, io.EOF) {
				captureErr = item.err
			}
			cancel()
			continue
		}
		if options.Count > 0 && count >= options.Count {
			continue
		}
		if err := writer.WritePacket(item.packet); err != nil {
			captureErr = err
			cancel()
			continue
		}
		count++
		if options.Count > 0 && count >= options.Count {
			cancel()
		}
	}
	select {
	case err := <-ownershipErr:
		if captureErr == nil {
			captureErr = err
		}
	default:
	}
	for _, s := range sources {
		_ = s.Close()
	}
	if err := writer.Close(); err != nil && captureErr == nil {
		captureErr = err
	}
	return captureErr
}

// CreateCaptureFile resolves a requested output below workingDir and opens it
// exclusively. Empty output receives a collision-resistant default name.
func CreateCaptureFile(workingDir, topology, link, output string, now time.Time) (*os.File, string, error) {
	if output == "" {
		var nonce [6]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, "", err
		}
		filename := fmt.Sprintf("%s-%s-%s.pcapng", link, now.UTC().Format("20060102T150405.000000000Z"), hex.EncodeToString(nonce[:]))
		output = filepath.Join("captures", topology, filename)
	}
	resolved, err := state.ResolveWithin(workingDir, output)
	if err != nil {
		return nil, "", err
	}
	if !strings.EqualFold(filepath.Ext(resolved), ".pcapng") {
		return nil, "", errors.New("capture output must have .pcapng extension")
	}
	f, err := openCaptureExclusive(workingDir, resolved)
	if err != nil {
		return nil, "", err
	}
	return f, resolved, nil
}

func openCaptureExclusive(base, destination string) (*os.File, error) {
	rel, err := filepath.Rel(base, destination)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("capture path escapes working directory: %s", destination)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 || parts[len(parts)-1] == "" || parts[len(parts)-1] == "." {
		return nil, errors.New("capture output filename is empty")
	}
	dirfd, err := unix.Open(base, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirfd) }()
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid capture path component %q", part)
		}
		if err := unix.Mkdirat(dirfd, part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
		next, err := unix.Openat2(dirfd, part, &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
		})
		if err != nil {
			return nil, err
		}
		_ = unix.Close(dirfd)
		dirfd = next
	}
	fd, err := unix.Openat(dirfd, parts[len(parts)-1], unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), destination), nil
}
