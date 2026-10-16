package jsonl

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

// FS is the file system JsonlStorage writes through: the part of pi's
// env FileSystem the storage uses. Paths are absolute. An operation that
// fails returns an error; ReadFile of a missing file returns one that
// errors.Is fs.ErrNotExist.
type FS interface {
	// MkdirAll creates a directory and its missing parents.
	MkdirAll(ctx context.Context, path string) error
	// ReadDir lists a directory.
	ReadDir(ctx context.Context, path string) ([]fs.DirEntry, error)
	// ReadFile reads a whole file.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// AppendFile appends to a file, creating it when missing.
	AppendFile(ctx context.Context, path string, data []byte) error
	// WriteFile replaces a file's content, creating it when missing.
	WriteFile(ctx context.Context, path string, data []byte) error
	// Flush makes a file's content durable (fsync).
	Flush(ctx context.Context, path string) error
	// Rename replaces newPath with oldPath.
	Rename(ctx context.Context, oldPath, newPath string) error
	// Remove removes a file; a missing file is not an error.
	Remove(ctx context.Context, path string) error
	// Truncate cuts a file to size bytes.
	Truncate(ctx context.Context, path string, size int64) error
}

// OSFS is the local file system.
type OSFS struct{}

var _ FS = OSFS{}

func (OSFS) MkdirAll(_ context.Context, path string) error { return os.MkdirAll(path, 0o755) }

func (OSFS) ReadDir(_ context.Context, path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }

func (OSFS) ReadFile(_ context.Context, path string) ([]byte, error) { return os.ReadFile(path) }

func (OSFS) AppendFile(_ context.Context, path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

func (OSFS) WriteFile(_ context.Context, path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func (OSFS) Flush(_ context.Context, path string) error {
	// Windows syncs only a handle opened for writing.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func (OSFS) Rename(_ context.Context, oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}

func (OSFS) Remove(_ context.Context, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (OSFS) Truncate(_ context.Context, path string, size int64) error {
	return os.Truncate(path, size)
}
