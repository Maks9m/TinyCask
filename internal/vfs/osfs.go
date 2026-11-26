//go:build unix

package vfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

const (
	filePerm os.FileMode = 0o644
	dirPerm  os.FileMode = 0o755
)

// O_APPEND makes every write land at the end, even after Truncate.
const (
	createFlags = os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_EXCL
	openFlags   = os.O_RDWR | os.O_APPEND
	lockFlags   = os.O_RDWR | os.O_CREATE
)

// OSFS is the FS backed by the real file system. The zero value is ready and
// safe for concurrent use.
type OSFS struct{}

var _ FS = OSFS{}

// Create makes a new file; it fails if name exists.
func (OSFS) Create(name string) (File, error) {
	f, err := os.OpenFile(name, createFlags, filePerm)
	if err != nil {
		return nil, mapErr(err)
	}
	return &osFile{f: f}, nil
}

// Open opens an existing file.
func (OSFS) Open(name string) (File, error) {
	f, err := os.OpenFile(name, openFlags, 0)
	if err != nil {
		return nil, mapErr(err)
	}
	return &osFile{f: f}, nil
}

// Remove deletes name.
func (OSFS) Remove(name string) error {
	return mapErr(os.Remove(name))
}

// Rename atomically moves oldName to newName.
func (OSFS) Rename(oldName, newName string) error {
	return mapErr(os.Rename(oldName, newName))
}

// List returns the sorted base names of the entries in dir.
func (OSFS) List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, mapErr(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// os.ReadDir sorts too, but sorting is our contract, so don't rely on it.
	slices.Sort(names)
	return names, nil
}

// MkdirAll creates dir and any missing parents.
func (OSFS) MkdirAll(dir string) error {
	return mapErr(os.MkdirAll(dir, dirPerm))
}

// SyncDir fsyncs dir so its entry changes are durable.
func (OSFS) SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return mapErr(err)
	}
	if err := d.Sync(); err != nil {
		return errors.Join(mapErr(err), d.Close())
	}
	return mapErr(d.Close())
}

// Lock takes an exclusive flock on name (ErrLocked if held by any handle, even
// in this process). Closing the result releases it; the file is left in place.
func (OSFS) Lock(name string) (io.Closer, error) {
	f, err := os.OpenFile(name, lockFlags, filePerm)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := flockExclusive(f); err != nil {
		return nil, errors.Join(fmt.Errorf("vfs: lock %s: %w", name, err), f.Close())
	}
	return f, nil
}

// osFile adapts *os.File to File, mapping ENOSPC.
type osFile struct {
	f *os.File
}

var _ File = (*osFile)(nil)

func (o *osFile) Write(p []byte) (int, error) {
	n, err := o.f.Write(p)
	return n, mapErr(err)
}

func (o *osFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := o.f.ReadAt(p, off)
	if errors.Is(err, io.EOF) {
		// Some callers compare io.EOF with ==; keep it unwrapped.
		return n, io.EOF
	}
	return n, mapErr(err)
}

func (o *osFile) Close() error {
	return mapErr(o.f.Close())
}

func (o *osFile) Sync() error {
	return mapErr(o.f.Sync())
}

func (o *osFile) Truncate(size int64) error {
	return mapErr(o.f.Truncate(size))
}

func (o *osFile) Size() (int64, error) {
	fi, err := o.f.Stat()
	if err != nil {
		return 0, mapErr(err)
	}
	return fi.Size(), nil
}

// mapErr wraps ENOSPC with ErrNoSpace; other errors (and nil) pass through.
func mapErr(err error) error {
	if err != nil && isNoSpace(err) {
		return fmt.Errorf("%w: %w", ErrNoSpace, err)
	}
	return err
}
