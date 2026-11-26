package engine

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
	"github.com/Maks9m/TinyCask/internal/vfs/simfs"
)

const (
	testDir         = "/db/data"
	testSegmentSize = 4 << 10 // 4 KiB: rolls after a handful of records
)

// testClock is the fixed time every test engine stamps records with.
func testClock() time.Time { return time.Unix(1_700_000_000, 0) }

func testOptions() Options {
	return Options{MaxSegmentSize: testSegmentSize, Now: testClock}
}

// openTest opens an engine on fsys and closes it at the end of the test.
func openTest(tb testing.TB, fsys vfs.FS, opts Options) *Engine {
	tb.Helper()
	e, err := Open(fsys, testDir, opts)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, e.Close()) })
	return e
}

// openSim opens an engine with test options on a fresh SimFS.
func openSim(tb testing.TB) (*Engine, *simfs.SimFS) {
	tb.Helper()
	fsys := simfs.New()
	return openTest(tb, fsys, testOptions()), fsys
}

// dataFiles returns the data files in dir, in ID order.
func dataFiles(tb testing.TB, fsys vfs.FS, dir string) []string {
	tb.Helper()
	names, err := fsys.List(dir)
	require.NoError(tb, err)
	var out []string
	for _, n := range names {
		_, ok, err := storage.ParseFileName(n)
		require.NoError(tb, err)
		if ok {
			out = append(out, n)
		}
	}
	return out
}

// fileSize returns a file's length as the FS reports it, bypassing the engine.
func fileSize(tb testing.TB, fsys vfs.FS, dir, base string) int64 {
	tb.Helper()
	f, err := fsys.Open(filepath.Join(dir, base))
	require.NoError(tb, err)
	defer func() { require.NoError(tb, f.Close()) }()
	size, err := f.Size()
	require.NoError(tb, err)
	return size
}

// totalFileSize sums the sizes of every data file in dir.
func totalFileSize(tb testing.TB, fsys vfs.FS, dir string) int64 {
	tb.Helper()
	var total int64
	for _, n := range dataFiles(tb, fsys, dir) {
		total += fileSize(tb, fsys, dir, n)
	}
	return total
}

// requireLockFree checks that LOCK in dir can be taken, i.e. no engine holds it.
func requireLockFree(tb testing.TB, fsys vfs.FS, dir string) {
	tb.Helper()
	l, err := fsys.Lock(filepath.Join(dir, lockFileName))
	require.NoError(tb, err)
	require.NoError(tb, l.Close())
}

// recordSize is the frame size of a PUT of key and value.
func recordSize(key, value string) int64 {
	return storage.RecordSize(len(key), uint32(len(value)))
}

// errInjected is what faultFS returns for an armed fault.
var errInjected = errors.New("injected fault")

// fsOp names an operation faultFS can fail.
type fsOp string

const (
	opCreate   fsOp = "create"
	opList     fsOp = "list"
	opSyncDir  fsOp = "syncdir"
	opWrite    fsOp = "write"
	opRead     fsOp = "read"
	opSync     fsOp = "sync"
	opTruncate fsOp = "truncate"
)

// fault is a one-shot failure of the next call of an operation.
type fault struct {
	err error
	n   int // bytes applied before err (write only)
}

// faultFS wraps a vfs.FS and fails chosen operations once. It is a minimal
// stand-in for SimFS fault injection, which arrives in T2.4.
type faultFS struct {
	vfs.FS

	// mu protects armed.
	mu    sync.Mutex
	armed map[fsOp]fault
}

func newFaultFS() *faultFS {
	return &faultFS{FS: simfs.New(), armed: make(map[fsOp]fault)}
}

// arm makes the next call of op fail with f.
func (f *faultFS) arm(op fsOp, flt fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed[op] = flt
}

func (f *faultFS) take(op fsOp) (fault, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flt, ok := f.armed[op]
	delete(f.armed, op)
	return flt, ok
}

func (f *faultFS) Create(name string) (vfs.File, error) {
	if flt, ok := f.take(opCreate); ok {
		return nil, flt.err
	}
	file, err := f.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return &faultFile{File: file, fs: f}, nil
}

func (f *faultFS) List(dir string) ([]string, error) {
	if flt, ok := f.take(opList); ok {
		return nil, flt.err
	}
	return f.FS.List(dir)
}

func (f *faultFS) SyncDir(dir string) error {
	if flt, ok := f.take(opSyncDir); ok {
		return flt.err
	}
	return f.FS.SyncDir(dir)
}

// faultFile is a File created through faultFS.
type faultFile struct {
	vfs.File
	fs *faultFS
}

func (f *faultFile) Write(p []byte) (int, error) {
	flt, ok := f.fs.take(opWrite)
	if !ok {
		return f.File.Write(p)
	}
	n := min(flt.n, len(p))
	if n > 0 {
		if _, err := f.File.Write(p[:n]); err != nil {
			return 0, err
		}
	}
	return n, flt.err
}

func (f *faultFile) ReadAt(p []byte, off int64) (int, error) {
	if flt, ok := f.fs.take(opRead); ok {
		return 0, flt.err
	}
	return f.File.ReadAt(p, off)
}

func (f *faultFile) Sync() error {
	if flt, ok := f.fs.take(opSync); ok {
		return flt.err
	}
	return f.File.Sync()
}

func (f *faultFile) Truncate(size int64) error {
	if flt, ok := f.fs.take(opTruncate); ok {
		return flt.err
	}
	return f.File.Truncate(size)
}
