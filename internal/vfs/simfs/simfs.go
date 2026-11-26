package simfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"sync"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// Errors for cases where OSFS would surface an errno that has no portable
// fs.Err* equivalent. Callers only need to know that the call failed.
var (
	errIsDir          = errors.New("simfs: is a directory")
	errNotDir         = errors.New("simfs: not a directory")
	errDirNotEmpty    = errors.New("simfs: directory not empty")
	errRemoveRoot     = errors.New("simfs: cannot remove a root directory")
	errNegativeOffset = errors.New("simfs: negative offset")
	errNegativeSize   = errors.New("simfs: negative size")
)

// SimFS is an in-memory vfs.FS that behaves like OSFS: names map to inodes,
// open handles follow the inode across Rename and Remove, reads see unsynced
// bytes, and each file tracks how much of it Sync has made durable.
//
// Create one with New. It is safe for concurrent use and deterministic: it
// reads no clock and draws no random numbers. Namespace operations (create,
// rename, remove, mkdir) are durable as soon as they return, a documented
// simplification (ARCHITECTURE §11), so SyncDir only checks that dir exists.
type SimFS struct {
	// mu guards both maps and the mutable state of every inode, file handle
	// and lock handle. One lock gives every operation a single global order,
	// which the fault injection of T2.4 relies on (for example "the Nth write").
	mu    sync.Mutex
	files map[string]*inode   // cleaned path → file
	dirs  map[string]struct{} // cleaned paths made by MkdirAll; roots are implicit
}

// inode is one file's contents, shared by every handle opened on it.
type inode struct {
	data   []byte
	synced int64 // length of the prefix of data that the last Sync made durable
	locked bool  // held by a handle returned from Lock
}

var _ vfs.FS = (*SimFS)(nil)

// New returns an empty SimFS.
func New() *SimFS {
	return &SimFS{
		files: make(map[string]*inode),
		dirs:  make(map[string]struct{}),
	}
}

// Create makes a new empty file; fs.ErrExist if name exists.
func (s *SimFS) Create(name string) (vfs.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Clean(name)
	if s.isDir(p) || s.files[p] != nil {
		return nil, pathErr("open", name, fs.ErrExist)
	}
	if err := s.checkParent(p); err != nil {
		return nil, pathErr("open", name, err)
	}
	ino := &inode{}
	s.files[p] = ino
	return &file{fs: s, name: name, ino: ino}, nil
}

// Open opens an existing file; fs.ErrNotExist if missing.
func (s *SimFS) Open(name string) (vfs.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ino, err := s.lookupFile(filepath.Clean(name))
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	return &file{fs: s, name: name, ino: ino}, nil
}

// Remove deletes a file or an empty directory. Open handles keep working.
func (s *SimFS) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Clean(name)
	switch {
	case s.files[p] != nil:
		delete(s.files, p)
	case p == "/" || p == ".":
		return pathErr("remove", name, errRemoveRoot)
	case s.isDir(p):
		if len(s.children(p)) > 0 {
			return pathErr("remove", name, errDirNotEmpty)
		}
		delete(s.dirs, p)
	default:
		return pathErr("remove", name, fs.ErrNotExist)
	}
	return nil
}

// Rename atomically moves the file oldName to newName, replacing any file
// there. Open handles keep working. Renaming directories is not supported:
// TinyCask never does it.
func (s *SimFS) Rename(oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldP, newP := filepath.Clean(oldName), filepath.Clean(newName)
	ino, err := s.lookupFile(oldP)
	if err != nil {
		return pathErr("rename", oldName, err)
	}
	if s.isDir(newP) {
		return pathErr("rename", newName, errIsDir)
	}
	if err := s.checkParent(newP); err != nil {
		return pathErr("rename", newName, err)
	}
	if oldP == newP {
		return nil
	}
	s.files[newP] = ino
	delete(s.files, oldP)
	return nil
}

// List returns the sorted base names of the files and directories in dir.
func (s *SimFS) List(dir string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Clean(dir)
	if err := s.checkDir(p); err != nil {
		return nil, pathErr("readdir", dir, err)
	}
	names := s.children(p)
	slices.Sort(names)
	return names, nil
}

// MkdirAll creates dir and any missing parents; no error if it exists.
func (s *SimFS) MkdirAll(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Clean(dir)
	var missing []string
	for !s.isDir(p) {
		if s.files[p] != nil {
			return pathErr("mkdir", p, errNotDir)
		}
		missing = append(missing, p)
		p = filepath.Dir(p)
	}
	for _, d := range missing {
		s.dirs[d] = struct{}{}
	}
	return nil
}

// SyncDir checks that dir exists. Namespace changes are already durable.
func (s *SimFS) SyncDir(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkDir(filepath.Clean(dir)); err != nil {
		return pathErr("sync", dir, err)
	}
	return nil
}

// Lock takes an exclusive lock on the file name, creating it if needed, and
// returns vfs.ErrLocked if any handle holds it. As with flock in OSFS the lock
// belongs to the inode: closing the result releases it and leaves the file.
func (s *SimFS) Lock(name string) (io.Closer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Clean(name)
	if s.isDir(p) {
		return nil, pathErr("open", name, errIsDir)
	}
	ino := s.files[p]
	if ino == nil {
		if err := s.checkParent(p); err != nil {
			return nil, pathErr("open", name, err)
		}
		ino = &inode{}
		s.files[p] = ino
	}
	if ino.locked {
		return nil, fmt.Errorf("simfs: lock %s: %w", name, vfs.ErrLocked)
	}
	ino.locked = true
	return &lockHandle{fs: s, name: name, ino: ino}, nil
}

// SyncedSize reports how many leading bytes of the file name the last Sync
// made durable. It lets tests check the synced length that a simulated power
// loss will cut the file back to.
func (s *SimFS) SyncedSize(name string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ino, err := s.lookupFile(filepath.Clean(name))
	if err != nil {
		return 0, pathErr("stat", name, err)
	}
	return ino.synced, nil
}

// isDir reports whether p is a directory. The roots "/" and "." always exist.
// Caller holds s.mu.
func (s *SimFS) isDir(p string) bool {
	if p == "/" || p == "." {
		return true
	}
	_, ok := s.dirs[p]
	return ok
}

// checkDir returns nil if p is a directory, else why not. Caller holds s.mu.
func (s *SimFS) checkDir(p string) error {
	switch {
	case s.isDir(p):
		return nil
	case s.files[p] != nil:
		return errNotDir
	default:
		return fs.ErrNotExist
	}
}

// checkParent returns nil if p's parent directory exists. Caller holds s.mu.
func (s *SimFS) checkParent(p string) error {
	return s.checkDir(filepath.Dir(p))
}

// lookupFile returns the inode at p, or why there is none. Caller holds s.mu.
func (s *SimFS) lookupFile(p string) (*inode, error) {
	if ino := s.files[p]; ino != nil {
		return ino, nil
	}
	if s.isDir(p) {
		return nil, errIsDir
	}
	return nil, fs.ErrNotExist
}

// children returns the unsorted base names directly inside dir. Caller holds s.mu.
func (s *SimFS) children(dir string) []string {
	var names []string
	for p := range s.files {
		if p != dir && filepath.Dir(p) == dir {
			names = append(names, filepath.Base(p))
		}
	}
	for p := range s.dirs {
		if p != dir && filepath.Dir(p) == dir {
			names = append(names, filepath.Base(p))
		}
	}
	return names
}

func pathErr(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: name, Err: err}
}
