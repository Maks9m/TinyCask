package simfs

import (
	"io"
	"io/fs"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// file is an open handle. It holds the inode, not the name, so it survives
// Rename and Remove.
type file struct {
	fs     *SimFS
	name   string // for error messages only
	ino    *inode
	gen    uint64 // generation at open; Restart kills older handles
	closed bool   // guarded by fs.mu
}

var _ vfs.File = (*file)(nil)

// usable returns ErrCrashed (crash/restart since open) or fs.ErrClosed, or
// nil. Caller holds f.fs.mu.
func (f *file) usable(op string) error {
	if err := f.fs.handleAlive(f.gen); err != nil {
		return pathErr(op, f.name, err)
	}
	if f.closed {
		return pathErr(op, f.name, fs.ErrClosed)
	}
	return nil
}

// Write appends p, visible at once but unsynced until Sync. A quota or tear
// may cut it short; the bytes that land are kept and counted.
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

// ReadAt reads at off, synced or not. Like os.File, a short read returns
// unwrapped io.EOF.
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

// Close releases the handle; again gives fs.ErrClosed, after a crash ErrCrashed.
func (f *file) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.usable("close"); err != nil {
		return err
	}
	f.closed = true
	return nil
}

// Sync marks all current bytes durable. An injected failure changes nothing.
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

// Truncate cuts or zero-extends to size, like ftruncate. Synced length is
// capped at size; extension bytes are unsynced.
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
	fs     *SimFS
	name   string // for error messages only
	ino    *inode
	gen    uint64 // generation at lock time; Restart releases the lock
	closed bool   // guarded by fs.mu
}

// Close releases the lock; again gives fs.ErrClosed. After Restart it gives
// ErrCrashed (Restart already released the lock).
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
