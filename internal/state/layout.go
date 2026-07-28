// Package state owns TopoLab's on-disk layout and persistent manifest.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxUnixSocketPath = 107

var resourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// Layout contains every state root used by a topology. Captures deliberately
// live outside TopologyDir so Down can remove state without removing captures.
type Layout struct {
	WorkingDir   string
	Topology     string
	StateRoot    string
	TopologyDir  string
	SharedDir    string
	LocksDir     string
	CaptureDir   string
	ManifestPath string
	LockPath     string
	NodesDir     string
	LogsDir      string
	SocketsDir   string
}

// NewLayout validates and resolves the working directory without creating it.
func NewLayout(workingDir, topology string) (Layout, error) {
	if strings.TrimSpace(workingDir) == "" {
		return Layout{}, errors.New("working directory is required")
	}
	if !resourceName.MatchString(topology) {
		return Layout{}, fmt.Errorf("invalid topology name %q", topology)
	}
	abs, err := filepath.Abs(workingDir)
	if err != nil {
		return Layout{}, fmt.Errorf("resolve working directory: %w", err)
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return Layout{}, errors.New("working directory may not be the filesystem root")
	}
	stateRoot := filepath.Join(abs, ".topolab")
	topologyDir := filepath.Join(stateRoot, "topologies", topology)
	l := Layout{
		WorkingDir:   abs,
		Topology:     topology,
		StateRoot:    stateRoot,
		TopologyDir:  topologyDir,
		SharedDir:    filepath.Join(stateRoot, "shared"),
		CaptureDir:   filepath.Join(abs, "captures", topology),
		ManifestPath: filepath.Join(topologyDir, "manifest.json"),
		LocksDir:     filepath.Join(stateRoot, "locks"),
		LockPath:     filepath.Join(stateRoot, "locks", topology+".lock"),
		NodesDir:     filepath.Join(topologyDir, "nodes"),
		LogsDir:      filepath.Join(topologyDir, "logs"),
		SocketsDir:   filepath.Join(topologyDir, "sockets"),
	}
	// Reserve room for a stable node-derived socket filename. Individual paths
	// are checked again by SocketPath.
	if len(filepath.Join(l.SocketsDir, strings.Repeat("x", 16)+".sock")) > maxUnixSocketPath {
		return Layout{}, fmt.Errorf("working directory is too long for Unix sockets: %q", abs)
	}
	return l, nil
}

// Ensure creates TopoLab state directories with root-only permissions.
func (l Layout) Ensure() error {
	for _, dir := range []string{l.WorkingDir, l.StateRoot, filepath.Dir(l.TopologyDir), l.TopologyDir, l.SharedDir, l.LocksDir, l.NodesDir, l.LogsDir, l.SocketsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure %s: %w", dir, err)
		}
	}
	return nil
}

func (l Layout) NodeDir(node string) (string, error) {
	if !resourceName.MatchString(node) {
		return "", fmt.Errorf("invalid node name %q", node)
	}
	return filepath.Join(l.NodesDir, node), nil
}

// SocketPath returns a deterministic socket path and enforces Linux's sockaddr_un limit.
func (l Layout) SocketPath(node, kind string) (string, error) {
	if !resourceName.MatchString(node) || !resourceName.MatchString(kind) {
		return "", errors.New("invalid socket node or kind")
	}
	name := ShortID(node+"-"+kind, 16) + ".sock"
	p := filepath.Join(l.SocketsDir, name)
	if len(p) > maxUnixSocketPath {
		return "", fmt.Errorf("Unix socket path exceeds %d bytes: %s", maxUnixSocketPath, p)
	}
	return p, nil
}

// ResolveWithin resolves a user supplied path and rejects lexical and symlink
// escapes. Missing final components are allowed; the nearest existing parent
// is evaluated for symlinks.
func ResolveWithin(base, candidate string) (string, error) {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	baseResolved, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return "", fmt.Errorf("resolve base path: %w", err)
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(baseResolved, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	candidate = filepath.Clean(candidate)
	if !isWithin(baseResolved, candidate) {
		return "", fmt.Errorf("path escapes working directory: %s", candidate)
	}

	existing := candidate
	var suffix []string
	for {
		_, statErr := os.Lstat(existing)
		if statErr == nil {
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("no existing parent for %s", candidate)
		}
		suffix = append(suffix, filepath.Base(existing))
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, suffix[i])
	}
	if !isWithin(baseResolved, resolved) {
		return "", fmt.Errorf("path escapes working directory through symlink: %s", candidate)
	}
	return resolved, nil
}

func isWithin(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
