// Package fsx holds small, durable file-system helpers shared by the image,
// blob, and job code.
package fsx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// HashFile returns the lowercase hexadecimal SHA-256 digest of a file and its
// size in bytes.
func HashFile(path string) (digest string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return HashReader(f)
}

// HashReader returns the SHA-256 digest of everything that r yields and the
// number of bytes read.
func HashReader(r io.Reader) (digest string, size int64, err error) {
	h := sha256.New()
	size, err = io.Copy(h, r)
	if err != nil {
		return "", size, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// SyncDir flushes a directory's entries to stable storage.
func SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// RenameNoReplace renames source to destination and fails when destination
// exists. The error then wraps os.ErrExist.
func RenameNoReplace(source, destination string) error {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("destination already exists: %s: %w", destination, os.ErrExist)
	}
	return err
}
