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
	gen  uint64 // SimFS generation at open time; Restart kills older handles
	// closed is guarded by fs.mu.
	closed bool
}

var _ vfs.File = (*file)(nil)

// usable returns why the handle cannot serve op, wrapped for the caller: the
// FS crashed or restarted since the handle was opened (ErrCrashed), or the
// handle is closed. Caller holds f.fs.mu.
func (f *file) usable(op string) error {
	if err := f.fs.handleAlive(f.gen); err != nil {
		return pathErr(op, f.name, err)
	}
	if f.closed {
		return pathErr(op, f.name, fs.ErrClosed)
	}
	return nil
}

// Write appends a copy of p to the file. The bytes are visible to every reader
// at once but stay unsynced until Sync. An armed quota or tear can cut the
// write short (faults.go); the bytes that land are kept and counted.
func (f *file) Write(p []byte) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.usable("write"); err != nil {
		return 0, err
	}
	if err := f.fs.inject(OpWrite); err != nil {
		return 0, pathErr("write", f.name, err)
	}
	n, stop := f.fs.admitWrite(len(p))
	f.ino.data = append(f.ino.data, p[:n]...)
	f.fs.flt.written += int64(n)
	if stop != nil {
		return n, pathErr("write", f.name, stop)
	}
	return n, nil
}

// ReadAt copies bytes from off into p, synced or not. Like os.File it returns
// io.EOF, unwrapped, when it reads fewer than len(p) bytes.
func (f *file) ReadAt(p []byte, off int64) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.usable("read"); err != nil {
		return 0, err
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

// Close releases the handle; a second Close returns fs.ErrClosed. A handle
// killed by a crash or Restart cannot be closed: it returns ErrCrashed.
func (f *file) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.usable("close"); err != nil {
		return err
	}
	f.closed = true
	return nil
}

// Sync marks every byte currently in the file as durable. A failed Sync,
// injected by FailNth, leaves the synced length unchanged.
func (f *file) Sync() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.usable("sync"); err != nil {
		return err
	}
	if err := f.fs.inject(OpSync); err != nil {
		return pathErr("sync", f.name, err)
	}
	f.ino.synced = int64(len(f.ino.data))
	return nil
}

// Truncate cuts the file to size bytes, or zero-extends it as ftruncate does.
// The synced length never exceeds the new size; extension bytes are unsynced.
func (f *file) Truncate(size int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.usable("truncate"); err != nil {
		return err
	}
	if err := f.fs.inject(OpTruncate); err != nil {
		return pathErr("truncate", f.name, err)
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
	if err := f.usable("stat"); err != nil {
		return 0, err
	}
	return int64(len(f.ino.data)), nil
}

// lockHandle releases an inode's lock on Close.
type lockHandle struct {
	fs   *SimFS
	name string // name at lock time, for error messages only
	ino  *inode
	gen  uint64 // SimFS generation at lock time; Restart releases the lock
	// closed is guarded by fs.mu.
	closed bool
}

// Close releases the lock; a second Close returns fs.ErrClosed. After a crash
// or Restart it returns ErrCrashed; Restart has already released the lock.
func (l *lockHandle) Close() error {
	l.fs.mu.Lock()
	defer l.fs.mu.Unlock()
	if err := l.fs.handleAlive(l.gen); err != nil {
		return pathErr("close", l.name, err)
	}
	if l.closed {
		return pathErr("close", l.name, fs.ErrClosed)
	}
	l.closed = true
	l.ino.locked = false
	return nil
}
