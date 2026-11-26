package vfs

import "io"

// FS is the file-system abstraction for all disk I/O (invariant 1). Names are
// used as given; an implementation adds no root of its own. Missing and
// duplicate files match fs.ErrNotExist and fs.ErrExist under errors.Is.
type FS interface {
	// Create makes a new empty file for reading and appending; fs.ErrExist if it exists.
	Create(name string) (File, error)
	// Open opens an existing file for reading and appending; fs.ErrNotExist if missing.
	Open(name string) (File, error)
	// Remove deletes a file. Open handles keep working.
	Remove(name string) error
	// Rename atomically moves oldName to newName, replacing it. Open handles keep working.
	Rename(oldName, newName string) error
	// List returns the sorted base names in dir.
	List(dir string) ([]string, error)
	// MkdirAll creates dir and missing parents; no error if it exists.
	MkdirAll(dir string) error
	// SyncDir makes creates, renames and removes in dir durable.
	SyncDir(dir string) error
	// Lock takes an exclusive lock on name, creating it if needed. ErrLocked if
	// already held; closing the result releases it.
	Lock(name string) (io.Closer, error)
}

// File is an open file. Write always appends (even after Truncate); reads are
// positional, so there is no file offset.
type File interface {
	io.Writer
	io.ReaderAt
	io.Closer
	// Sync flushes contents to stable storage.
	Sync() error
	// Truncate cuts the file to size bytes.
	Truncate(size int64) error
	// Size returns the current length in bytes.
	Size() (int64, error)
}
