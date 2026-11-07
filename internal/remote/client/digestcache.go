package client

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/torob/topolab/internal/fsx"
	"github.com/torob/topolab/internal/state"
)

// DigestCache remembers file digests between runs, keyed by path, device,
// inode, size, modification time, and change time. A cached digest is used only
// when every one of those values is unchanged. The cache is optional
// performance data (spec.md Section 12.5).
type DigestCache struct {
	// Path is the cache file; empty means no persistence.
	Path string

	mu      sync.Mutex
	loaded  bool
	entries map[string]cacheEntry
}

type cacheEntry struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// DefaultDigestCachePath returns $XDG_CACHE_HOME/topolab/digests.json, falling
// back to ~/.cache/topolab/digests.json, or "" when neither can be found.
func DefaultDigestCachePath() string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return filepath.Join(dir, "topolab", "digests.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cache", "topolab", "digests.json")
}

func cacheKey(path string, info os.FileInfo) (string, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s|%d|%d|%d|%d|%d", path, st.Dev, st.Ino, st.Size, st.Mtim.Nano(), st.Ctim.Nano()), true
}

func (c *DigestCache) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	c.entries = map[string]cacheEntry{}
	if c.Path == "" {
		return
	}
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		return
	}
	var entries map[string]cacheEntry
	if json.Unmarshal(raw, &entries) == nil && entries != nil {
		c.entries = entries
	}
}

// Digest returns the SHA-256 digest and size of a file, hashing it unless the
// cache holds a still-valid value. progress receives a line before a slow hash.
func (c *DigestCache) Digest(path string, progress io.Writer) (string, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	key, keyed := cacheKey(path, info)
	if c != nil && keyed {
		c.mu.Lock()
		c.load()
		entry, ok := c.entries[key]
		c.mu.Unlock()
		if ok && entry.Size == info.Size() {
			return entry.Digest, entry.Size, nil
		}
	}
	if progress != nil && info.Size() > 256<<20 {
		fmt.Fprintf(progress, "topolab: hashing %s (%d MiB)...\n", path, info.Size()>>20)
	}
	digest, size, err := fsx.HashFile(path)
	if err != nil {
		return "", 0, err
	}
	if c != nil && keyed {
		after, err := os.Stat(path)
		if err == nil {
			if k2, ok := cacheKey(path, after); ok && k2 == key {
				c.mu.Lock()
				c.load()
				c.entries[key] = cacheEntry{Digest: digest, Size: size}
				c.mu.Unlock()
				c.save()
			}
		}
	}
	return digest, size, nil
}

// Forget drops every cached value for a path.
func (c *DigestCache) Forget(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.load()
	for k := range c.entries {
		if strings.HasPrefix(k, path+"|") {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
	c.save()
}

// save writes the cache; a failure is ignored because the cache is optional.
func (c *DigestCache) save() {
	if c == nil || c.Path == "" {
		return
	}
	c.mu.Lock()
	raw, err := json.Marshal(c.entries)
	c.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return
	}
	_ = state.AtomicWrite(c.Path, raw, 0o600)
}
