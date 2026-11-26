package simfs

import (
	"io"
	"io/fs"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// file is an open handle on an inode. It keeps working after the name it was
// opened under is renamed or removed, because it holds the inode, not the name.
type file struct {
	fs   *SimFS
	name string // name at open time, for error messages only
	ino  *inode
	// closed is guarded by fs.mu.
	closed bool
}

var _ vfs.File = (*file)(nil)

// Write appends a copy of p to the file. The bytes are visible to every reader
// at once but stay unsynced until Sync.
func (f *file) Write(p []byte) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		return 0, pathErr("write", f.name, fs.ErrClosed)
	}
	f.ino.data = append(f.ino.data, p...)
	return len(p), nil
}

// ReadAt copies bytes from off into p, synced or not. Like os.File it returns
// io.EOF, unwrapped, when it reads fewer than len(p) bytes.
func (f *file) ReadAt(p []byte, off int64) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		return 0, pathErr("read", f.name, fs.ErrClosed)
	}
	if off < 0 {
		return 0, pathErr("readat", f.name, errNegativeOffset)
	}
	size := int64(len(f.ino.data))
	if off >= size {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	n := copy(p, f.ino.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Close releases the handle; a second Close returns fs.ErrClosed.
func (f *file) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		return pathErr("close", f.name, fs.ErrClosed)
	}
	f.closed = true
	return nil
}

// Sync marks every byte currently in the file as durable.
func (f *file) Sync() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		return pathErr("sync", f.name, fs.ErrClosed)
	}
	f.ino.synced = int64(len(f.ino.data))
	return nil
}

// Truncate cuts the file to size bytes, or zero-extends it as ftruncate does.
// The synced length never exceeds the new size; extension bytes are unsynced.
func (f *file) Truncate(size int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		return pathErr("truncate", f.name, fs.ErrClosed)
	}
	if size < 0 {
		return pathErr("truncate", f.name, errNegativeSize)
	}
	cur := int64(len(f.ino.data))
	if size <= cur {
		f.ino.data = f.ino.data[:size]
	} else {
		f.ino.data = append(f.ino.data, make([]byte, size-cur)...)
	}
	f.ino.synced = min(f.ino.synced, size)
	return nil
}

// Size returns the current length, including unsynced bytes.
func (f *file) Size() (int64, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		return 0, pathErr("stat", f.name, fs.ErrClosed)
	}
	return int64(len(f.ino.data)), nil
}

// lockHandle releases an inode's lock on Close.
type lockHandle struct {
	fs   *SimFS
	name string // name at lock time, for error messages only
	ino  *inode
	// closed is guarded by fs.mu.
	closed bool
}

// Close releases the lock; a second Close returns fs.ErrClosed.
func (l *lockHandle) Close() error {
	l.fs.mu.Lock()
	defer l.fs.mu.Unlock()
	if l.closed {
		return pathErr("close", l.name, fs.ErrClosed)
	}
	l.closed = true
	l.ino.locked = false
	return nil
}
