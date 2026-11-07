package config

import (
	"io/fs"
	"os"
)

// FileSystem is the read-only file access that topology parsing needs. Remote
// operation supplies an implementation that serves files from a request, so
// the agent never reads controller paths.
type FileSystem interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
}

type osFileSystem struct{}

func (osFileSystem) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

func (osFileSystem) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// OSFileSystem returns the file system of the running process.
func OSFileSystem() FileSystem { return osFileSystem{} }
