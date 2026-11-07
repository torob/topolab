package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, size, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if digest != want || size != 3 {
		t.Fatalf("HashFile = %s, %d", digest, size)
	}
	if _, _, err := HashFile(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v", err)
	}
}

func TestHashReader(t *testing.T) {
	digest, size, err := HashReader(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if digest != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" || size != 0 {
		t.Fatalf("HashReader = %s, %d", digest, size)
	}
}

func TestRenameNoReplace(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	other := filepath.Join(dir, "other")
	destination := filepath.Join(dir, "destination")
	for path, content := range map[string]string{source: "one", other: "two"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RenameNoReplace(source, destination); err != nil {
		t.Fatal(err)
	}
	err := RenameNoReplace(other, destination)
	if err == nil || !errors.Is(err, os.ErrExist) || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("second rename error = %v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "one" {
		t.Fatalf("destination = %q, %v", data, err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("rejected source vanished: %v", err)
	}
}

func TestSyncDir(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("SyncDir of a missing directory succeeded")
	}
}
