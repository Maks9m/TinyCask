//go:build unix

package vfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// File permissions for everything OSFS creates.
const (
	filePerm os.FileMode = 0o644
	dirPerm  os.FileMode = 0o755
)

// Open flags. O_APPEND makes the kernel place every write at the current end
// of the file, which is what File.Write promises, even after a Truncate.
const (
	createFlags = os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_EXCL
	openFlags   = os.O_RDWR | os.O_APPEND
	lockFlags   = os.O_RDWR | os.O_CREATE
)

// OSFS is the FS backed by the real operating-system file system. The zero
// value is ready to use and safe for concurrent use.
type OSFS struct{}

var _ FS = OSFS{}

// Create makes a new file for reading and appending; it fails if name exists.
func (OSFS) Create(name string) (File, error) {
	f, err := os.OpenFile(name, createFlags, filePerm)
	if err != nil {
		return nil, mapErr(err)
	}
	return &osFile{f: f}, nil
}

// Open opens an existing file for reading and appending.
func (OSFS) Open(name string) (File, error) {
	f, err := os.OpenFile(name, openFlags, 0)
	if err != nil {
		return nil, mapErr(err)
	}
	return &osFile{f: f}, nil
}

// Remove deletes the file name.
func (OSFS) Remove(name string) error {
	return mapErr(os.Remove(name))
}

// Rename atomically moves oldName to newName, replacing newName if it exists.
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
	// os.ReadDir already sorts, but the order is part of the FS contract, so
	// it is not left to an implementation detail of the standard library.
	slices.Sort(names)
	return names, nil
}

// MkdirAll creates dir and any missing parents.
func (OSFS) MkdirAll(dir string) error {
	return mapErr(os.MkdirAll(dir, dirPerm))
}

// SyncDir fsyncs dir so that creates, renames and removes in it are durable.
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

// Lock takes an exclusive flock on name, creating the file if needed. It
// returns ErrLocked if another open handle, in this or another process,
// already holds the lock. Closing the returned io.Closer releases the lock;
// the lock file itself is left in place.
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

// osFile adapts *os.File to File, translating ENOSPC on the way out.
type osFile struct {
	f *os.File
}

var _ File = (*osFile)(nil)

// Write appends p to the file. A short write returns the bytes written.
func (o *osFile) Write(p []byte) (int, error) {
	n, err := o.f.Write(p)
	return n, mapErr(err)
}

// ReadAt reads len(p) bytes at off; it returns io.EOF when it reads past the end.
func (o *osFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := o.f.ReadAt(p, off)
	if errors.Is(err, io.EOF) {
		// Callers compare io.EOF with ==; keep it unwrapped.
		return n, io.EOF
	}
	return n, mapErr(err)
}

// Close closes the file.
func (o *osFile) Close() error {
	return mapErr(o.f.Close())
}

// Sync fsyncs the file's contents.
func (o *osFile) Sync() error {
	return mapErr(o.f.Sync())
}

// Truncate cuts the file to size bytes.
func (o *osFile) Truncate(size int64) error {
	return mapErr(o.f.Truncate(size))
}

// Size returns the file's current length.
func (o *osFile) Size() (int64, error) {
	fi, err := o.f.Stat()
	if err != nil {
		return 0, mapErr(err)
	}
	return fi.Size(), nil
}

// mapErr adds ErrNoSpace to the chain of an ENOSPC error and returns every
// other error, including nil, unchanged.
func mapErr(err error) error {
	if err != nil && isNoSpace(err) {
		return fmt.Errorf("%w: %w", ErrNoSpace, err)
	}
	return err
}
