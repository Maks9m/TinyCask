package storage

import (
	"errors"
	"flag"
	"io"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// seedFlag seeds randomized tests; re-run with -seed=<logged seed>.
var seedFlag = flag.Uint64("seed", 1, "seed for randomized storage tests")

func testRand(tb testing.TB) *rand.Rand {
	tb.Helper()
	seed := *seedFlag
	tb.Logf("seed=%d", seed)
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// errInjected is the error faultFS returns for an armed fault.
var errInjected = errors.New("injected fault")

// fsOp names an operation faultFS can fail or observe.
type fsOp string

const (
	opCreate   fsOp = "create"
	opOpen     fsOp = "open"
	opRemove   fsOp = "remove"
	opSyncDir  fsOp = "syncdir"
	opWrite    fsOp = "write"
	opRead     fsOp = "read"
	opSync     fsOp = "sync"
	opTruncate fsOp = "truncate"
	opSize     fsOp = "size"
	opClose    fsOp = "close"
)

// fault is a one-shot failure of the next call of an operation.
type fault struct {
	err error
	// n bytes reach the file (write) or caller (read) before err.
	n int
}

// faultFS wraps OSFS under t.TempDir() and fails chosen operations once
// (stand-in for simfs until T2.1). It logs mutating ops as "<op> <base>" so
// tests can check durability order (invariant 7).
type faultFS struct {
	inner vfs.FS

	// mu protects the fields below.
	mu    sync.Mutex
	armed map[fsOp]fault
	hooks map[fsOp]func(name string)
	log   []string
}

var _ vfs.FS = (*faultFS)(nil)

func newFaultFS() *faultFS {
	return &faultFS{
		inner: vfs.OSFS{},
		armed: make(map[fsOp]fault),
		hooks: make(map[fsOp]func(string)),
	}
}

// arm makes the next call of op fail with f.
func (f *faultFS) arm(op fsOp, flt fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed[op] = flt
}

// onNext runs hook once before the next call of op, with no lock held.
func (f *faultFS) onNext(op fsOp, hook func(name string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[op] = hook
}

// ops returns the operations logged so far.
func (f *faultFS) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// take logs op (reads are not logged) and pops its armed fault and hook.
func (f *faultFS) take(op fsOp, name string) (fault, bool) {
	f.mu.Lock()
	if op != opRead {
		f.log = append(f.log, string(op)+" "+filepath.Base(name))
	}
	flt, ok := f.armed[op]
	delete(f.armed, op)
	hook := f.hooks[op]
	delete(f.hooks, op)
	f.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	return flt, ok
}

func (f *faultFS) Create(name string) (vfs.File, error) {
	if flt, ok := f.take(opCreate, name); ok {
		return nil, flt.err
	}
	file, err := f.inner.Create(name)
	if err != nil {
		return nil, err
	}
	return &faultFile{fs: f, name: name, inner: file}, nil
}

func (f *faultFS) Open(name string) (vfs.File, error) {
	if flt, ok := f.take(opOpen, name); ok {
		return nil, flt.err
	}
	file, err := f.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultFile{fs: f, name: name, inner: file}, nil
}

func (f *faultFS) Remove(name string) error {
	if flt, ok := f.take(opRemove, name); ok {
		return flt.err
	}
	return f.inner.Remove(name)
}

func (f *faultFS) SyncDir(dir string) error {
	if flt, ok := f.take(opSyncDir, dir); ok {
		return flt.err
	}
	return f.inner.SyncDir(dir)
}

func (f *faultFS) Rename(oldName, newName string) error { return f.inner.Rename(oldName, newName) }
func (f *faultFS) List(dir string) ([]string, error)    { return f.inner.List(dir) }
func (f *faultFS) MkdirAll(dir string) error            { return f.inner.MkdirAll(dir) }
func (f *faultFS) Lock(name string) (io.Closer, error)  { return f.inner.Lock(name) }

// faultFile is a File opened through faultFS.
type faultFile struct {
	fs    *faultFS
	name  string
	inner vfs.File
}

func (f *faultFile) Write(p []byte) (int, error) {
	flt, ok := f.fs.take(opWrite, f.name)
	if !ok {
		return f.inner.Write(p)
	}
	n := min(flt.n, len(p))
	if n > 0 {
		if _, err := f.inner.Write(p[:n]); err != nil {
			return 0, err
		}
	}
	return n, flt.err
}

func (f *faultFile) ReadAt(p []byte, off int64) (int, error) {
	flt, ok := f.fs.take(opRead, f.name)
	if !ok {
		return f.inner.ReadAt(p, off)
	}
	n, err := f.inner.ReadAt(p[:min(flt.n, len(p))], off)
	if err != nil {
		return n, err
	}
	return n, flt.err
}

func (f *faultFile) Sync() error {
	if flt, ok := f.fs.take(opSync, f.name); ok {
		return flt.err
	}
	return f.inner.Sync()
}

func (f *faultFile) Truncate(size int64) error {
	if flt, ok := f.fs.take(opTruncate, f.name); ok {
		return flt.err
	}
	return f.inner.Truncate(size)
}

func (f *faultFile) Size() (int64, error) {
	if flt, ok := f.fs.take(opSize, f.name); ok {
		return 0, flt.err
	}
	return f.inner.Size()
}

// Close always releases the real handle, then reports any armed fault.
func (f *faultFile) Close() error {
	flt, ok := f.fs.take(opClose, f.name)
	err := f.inner.Close()
	if ok {
		return flt.err
	}
	return err
}

// diskSize returns a file's length as the OS reports it, bypassing the Store.
func diskSize(tb testing.TB, path string) int64 {
	tb.Helper()
	f, err := vfs.OSFS{}.Open(path)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, f.Close()) }()
	size, err := f.Size()
	require.NoError(tb, err)
	return size
}
