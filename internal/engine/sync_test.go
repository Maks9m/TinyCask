package engine

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
	"github.com/Maks9m/TinyCask/internal/vfs/simfs"
)

// deadlockGuard bounds a wait that only a bug can make long. It detects a
// hang; it never orders goroutines.
const deadlockGuard = 10 * time.Second

func TestOpen_RejectsBadSyncOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"unknown policy", Options{Sync: SyncNever + 1}},
		{"negative interval", Options{SyncInterval: -time.Second}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := simfs.New()
			_, err := Open(fsys, testDir, tc.opts)
			require.Error(t, err)
			_, err = fsys.List(testDir)
			require.ErrorIs(t, err, fs.ErrNotExist, "bad options are rejected before touching the disk")
		})
	}
}

func TestSync_PolicyString(t *testing.T) {
	assert.Equal(t, "interval", SyncInterval.String())
	assert.Equal(t, "always", SyncAlways.String())
	assert.Equal(t, "never", SyncNever.String())
	assert.Equal(t, "SyncPolicy(9)", SyncPolicy(9).String())
}

func TestSync_AlwaysSyncsBeforeReturning(t *testing.T) {
	fsys := simfs.New()
	opts := testOptions()
	opts.Sync = SyncAlways
	e := openTest(t, fsys, opts)

	for i := range 20 { // rolls at least once
		require.NoError(t, e.Put([]byte(numKey(i)), []byte(strings.Repeat("a", 200))))
		assertActiveSynced(t, e, fsys)
	}
	require.NoError(t, e.Delete([]byte(numKey(0))))
	assertActiveSynced(t, e, fsys)
}

func TestSync_AlwaysFailedSyncMakesReadOnly(t *testing.T) {
	fsys := simfs.New()
	opts := testOptions()
	opts.Sync = SyncAlways
	e := openTest(t, fsys, opts)
	require.NoError(t, e.Put([]byte("k"), []byte("v")))
	keysBefore, liveBefore := e.Stats().Keys, e.Stats().LiveBytes

	fsys.FailNth(simfs.OpSync, 1, nil)
	err := e.Put([]byte("new"), []byte("v2"))
	require.ErrorIs(t, err, ErrReadOnly)
	require.ErrorIs(t, err, simfs.ErrInjected)

	assert.True(t, e.Stats().ReadOnly)
	assert.Equal(t, keysBefore, e.Stats().Keys, "KeyDir must not point at an unsynced record")
	assert.Equal(t, liveBefore, e.Stats().LiveBytes)
	_, err = e.Get([]byte("new"))
	require.ErrorIs(t, err, ErrKeyNotFound)
	assertValue(t, e, "k", "v")
	require.ErrorIs(t, e.Put([]byte("x"), []byte("y")), ErrReadOnly)
	require.ErrorIs(t, e.Delete([]byte("k")), ErrReadOnly)
}

func TestSync_NeverSyncsOnlyOnRollAndClose(t *testing.T) {
	fsys := simfs.New()
	opts := testOptions()
	opts.Sync = SyncNever
	e, err := Open(fsys, testDir, opts)
	require.NoError(t, err)

	require.NoError(t, e.Put([]byte("first"), []byte("v")))
	assert.Zero(t, syncedSize(t, fsys, storage.SegmentName(1)), "no fsync before a roll")

	for i := 0; activeID(t, e) == 1; i++ {
		require.NoError(t, e.Put([]byte(numKey(i)), []byte(strings.Repeat("n", 200))))
	}
	seg1, seg2 := storage.SegmentName(1), storage.SegmentName(2)
	assert.Equal(t, fileSize(t, fsys, testDir, seg1), syncedSize(t, fsys, seg1), "a roll seals a synced segment")
	assert.Zero(t, syncedSize(t, fsys, seg2), "the new active segment is not synced yet")

	require.NoError(t, e.Close())
	assert.Equal(t, fileSize(t, fsys, testDir, seg2), syncedSize(t, fsys, seg2), "Close syncs the active segment")
}

func TestSync_IntervalTickerSyncsActiveSegment(t *testing.T) {
	fsys := simfs.New()
	tk := newManualTicker()
	opts := testOptions()
	tk.install(&opts)
	e, err := Open(fsys, testDir, opts)
	require.NoError(t, err)
	seg1 := storage.SegmentName(1)

	require.NoError(t, e.Put([]byte("a"), []byte("1")))
	assert.Zero(t, syncedSize(t, fsys, seg1), "nothing is synced before a tick")
	tk.tick()
	assert.Equal(t, fileSize(t, fsys, testDir, seg1), syncedSize(t, fsys, seg1))

	require.NoError(t, e.Put([]byte("b"), []byte("2")))
	assert.Less(t, syncedSize(t, fsys, seg1), fileSize(t, fsys, testDir, seg1))
	tk.tick()
	assert.Equal(t, fileSize(t, fsys, testDir, seg1), syncedSize(t, fsys, seg1))

	require.NoError(t, e.Close())
	assert.True(t, tk.stopped.Load(), "Close stops the ticker")
}

// TestSync_IntervalSyncRunsOutsideWriterLock blocks the background fsync
// inside Sync and shows a Put still completes meanwhile.
func TestSync_IntervalSyncRunsOutsideWriterLock(t *testing.T) {
	fsys := newGateFS()
	tk := newManualTicker()
	opts := testOptions()
	tk.install(&opts)
	e := openTest(t, fsys, opts)
	require.NoError(t, e.Put([]byte("a"), []byte("1")))

	fsys.armed.Store(true)
	tk.ticks <- time.Time{} // returns once syncLoop took the tick
	<-fsys.entered          // the fsync is now blocked inside Sync

	done := make(chan error, 1)
	go func() { done <- e.Put([]byte("b"), []byte("2")) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(deadlockGuard):
		t.Fatal("Put blocked behind the background fsync: the writer lock is held during Sync")
	}
	close(fsys.release)
	<-tk.done // the blocked sync has finished
	assertValue(t, e, "a", "1")
	assertValue(t, e, "b", "2")
}

func TestSync_IntervalFailedSyncMakesReadOnly(t *testing.T) {
	fsys := simfs.New()
	tk := newManualTicker()
	var logs bytes.Buffer
	opts := testOptions()
	tk.install(&opts)
	opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	e := openTest(t, fsys, opts)
	require.NoError(t, e.Put([]byte("k"), []byte("v")))

	fsys.FailNth(simfs.OpSync, 1, nil)
	tk.tick()

	assert.True(t, e.Stats().ReadOnly)
	assert.Contains(t, logs.String(), "background fsync failed")
	require.ErrorIs(t, e.Put([]byte("x"), []byte("y")), ErrReadOnly)
	assertValue(t, e, "k", "v")

	tk.tick() // a read-only engine has nothing to sync: no second failure
	assert.Equal(t, 1, strings.Count(logs.String(), "background fsync failed"))
}

// TestSync_IntervalBoundsPowerLoss: a power loss keeps everything written
// before the last tick and drops what came after it.
func TestSync_IntervalBoundsPowerLoss(t *testing.T) {
	fsys := simfs.New()
	tk := newManualTicker()
	opts := testOptions()
	tk.install(&opts)
	e, err := Open(fsys, testDir, opts)
	require.NoError(t, err)

	h := newHistory()
	for i := range 5 {
		h.put(t, e, numKey(i), "before")
	}
	tk.tick()
	for i := 5; i < 10; i++ {
		require.NoError(t, e.Put([]byte(numKey(i)), []byte("after")))
	}

	fsys.Restart(simfs.LoseUnsynced)
	require.Error(t, e.Close(), "every handle died in the power loss")
	for i := 5; i < 10; i++ {
		h.gone[numKey(i)] = true
	}
	h.check(t, reopen(t, fsys, testDir, opts))
}

func TestSync_AlwaysOnOSFS(t *testing.T) {
	fsys := vfs.OSFS{}
	dir := filepath.Join(t.TempDir(), "data")
	opts := testOptions()
	opts.Sync = SyncAlways
	e, err := Open(fsys, dir, opts)
	require.NoError(t, err)
	h := newHistory()
	for i := range 20 {
		h.put(t, e, numKey(i), fmt.Sprintf("value-%d-%s", i, strings.Repeat("o", 200)))
	}
	h.del(t, e, numKey(4))
	require.NoError(t, e.Close())

	h.check(t, reopen(t, fsys, dir, opts))
}

// manualTicker is an interval ticker the test drives by hand.
type manualTicker struct {
	ticks   chan time.Time // unbuffered: a send returns once syncLoop took it
	done    chan struct{}  // unbuffered: syncLoop reports each handled tick
	stopped atomic.Bool
}

func newManualTicker() *manualTicker {
	return &manualTicker{ticks: make(chan time.Time), done: make(chan struct{})}
}

// install makes opts use this ticker. Every tick must then be followed by a
// receive from done, which tick does.
func (m *manualTicker) install(opts *Options) {
	opts.newTicker = func(time.Duration) (<-chan time.Time, func()) {
		return m.ticks, func() { m.stopped.Store(true) }
	}
	opts.afterTick = func() { m.done <- struct{}{} }
}

// tick delivers one tick and returns after syncLoop has fully handled it.
func (m *manualTicker) tick() {
	m.ticks <- time.Time{}
	<-m.done
}

// gateFS blocks the first file Sync after armed is set until release is
// closed, signaling entered when the Sync starts.
type gateFS struct {
	vfs.FS
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newGateFS() *gateFS {
	return &gateFS{FS: simfs.New(), entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gateFS) Create(name string) (vfs.File, error) {
	f, err := g.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return &gateFile{File: f, fs: g}, nil
}

type gateFile struct {
	vfs.File
	fs *gateFS
}

func (f *gateFile) Sync() error {
	if f.fs.armed.CompareAndSwap(true, false) {
		close(f.fs.entered)
		<-f.fs.release
	}
	return f.File.Sync()
}

// activeID returns the active segment's ID.
func activeID(tb testing.TB, e *Engine) uint32 {
	tb.Helper()
	id, _, ok := e.store.Active()
	require.True(tb, ok)
	return id
}

// syncedSize returns how many bytes of a data file the last Sync made durable.
func syncedSize(tb testing.TB, fsys *simfs.SimFS, base string) int64 {
	tb.Helper()
	n, err := fsys.SyncedSize(filepath.Join(testDir, base))
	require.NoError(tb, err)
	return n
}

// assertActiveSynced checks the active segment is synced to its full size.
func assertActiveSynced(tb testing.TB, e *Engine, fsys *simfs.SimFS) {
	tb.Helper()
	base := storage.SegmentName(activeID(tb, e))
	assert.Equal(tb, fileSize(tb, fsys, testDir, base), syncedSize(tb, fsys, base), base)
}
