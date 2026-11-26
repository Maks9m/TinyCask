package vfs

import "io"

// FS is the file-system abstraction every TinyCask package uses for disk I/O
// (ARCHITECTURE §11, invariant 1). Names are paths as the caller builds them
// with path/filepath; an implementation does not prepend a root of its own.
//
// Missing and duplicate files are reported with errors that match
// fs.ErrNotExist and fs.ErrExist under errors.Is.
type FS interface {
	// Create makes a new, empty file opened for reading and appending.
	// It fails with fs.ErrExist if the file already exists.
	Create(name string) (File, error)
	// Open opens an existing file for reading and appending.
	// It fails with fs.ErrNotExist if the file does not exist.
	Open(name string) (File, error)
	// Remove deletes a file. Handles that are already open keep working.
	Remove(name string) error
	// Rename moves oldName to newName, atomically replacing newName if it
	// exists. Handles that are already open keep working.
	Rename(oldName, newName string) error
	// List returns the base names of the entries in dir, sorted.
	List(dir string) ([]string, error)
	// MkdirAll creates dir and any missing parents. It succeeds if dir
	// already exists.
	MkdirAll(dir string) error
	// SyncDir makes the directory's entries (creates, renames, removes)
	// durable.
	SyncDir(dir string) error
	// Lock takes an exclusive lock on the file name, creating it if needed.
	// It fails with ErrLocked while another holder has the lock; closing the
	// returned io.Closer releases it.
	Lock(name string) (io.Closer, error)
}

// File is an open file. Write always appends at the current end of the file,
// including after Truncate; reads are positional, so a File has no offset.
type File interface {
	io.Writer
	io.ReaderAt
	io.Closer
	// Sync flushes the file's contents to stable storage.
	Sync() error
	// Truncate cuts the file to size bytes.
	Truncate(size int64) error
	// Size returns the current length of the file in bytes.
	Size() (int64, error)
}
