package engine

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
	"github.com/Maks9m/TinyCask/internal/vfs/simfs"
)

// openFault opens a test engine on a fresh faultFS.
func openFault(tb testing.TB) (*Engine, *faultFS) {
	tb.Helper()
	fsys := newFaultFS()
	return openTest(tb, fsys, testOptions()), fsys
}

func TestFault_FailedAppendIsRolledBack(t *testing.T) {
	tests := []struct {
		name    string
		flt     fault
		wantErr error
	}{
		{"write error", fault{err: errInjected}, errInjected},
		{"short write", fault{err: errInjected, n: 10}, errInjected},
		{"no space after a partial write", fault{err: vfs.ErrNoSpace, n: 5}, ErrNoSpace},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, fsys := openFault(t)
			require.NoError(t, e.Put([]byte("old"), []byte("v1")))
			sizeBefore := totalFileSize(t, fsys, testDir)
			statsBefore := e.Stats()

			fsys.arm(opWrite, tc.flt)
			require.ErrorIs(t, e.Put([]byte("new"), []byte("v2")), tc.wantErr)

			assert.Equal(t, sizeBefore, totalFileSize(t, fsys, testDir), "partial bytes must be truncated away")
			assert.Equal(t, statsBefore, e.Stats(), "KeyDir and counters must be unchanged")
			_, err := e.Get([]byte("new"))
			require.ErrorIs(t, err, ErrKeyNotFound)

			// The engine stays writable and the next record lands where the failed one would have.
			require.NoError(t, e.Put([]byte("new"), []byte("v2")))
			assertValue(t, e, "old", "v1")
			assertValue(t, e, "new", "v2")
			assert.Equal(t, sizeBefore+recordSize("new", "v2"), totalFileSize(t, fsys, testDir))
		})
	}
}

func TestFault_FailedTombstoneKeepsKey(t *testing.T) {
	e, fsys := openFault(t)
	require.NoError(t, e.Put([]byte("k"), []byte("v")))
	statsBefore := e.Stats()

	fsys.arm(opWrite, fault{err: errInjected, n: 3})
	require.ErrorIs(t, e.Delete([]byte("k")), errInjected)

	assertValue(t, e, "k", "v")
	assert.Equal(t, statsBefore, e.Stats())
	require.NoError(t, e.Delete([]byte("k")))
}

func TestFault_FailedTruncateMakesReadOnly(t *testing.T) {
	e, fsys := openFault(t)
	require.NoError(t, e.Put([]byte("k"), []byte("v")))
	statsBefore := e.Stats()

	fsys.arm(opWrite, fault{err: errInjected, n: 4})
	fsys.arm(opTruncate, fault{err: errInjected})
	err := e.Put([]byte("new"), []byte("v2"))
	require.ErrorIs(t, err, ErrReadOnly)
	require.ErrorIs(t, err, errInjected)

	assert.True(t, e.Stats().ReadOnly)
	require.ErrorIs(t, e.Put([]byte("x"), []byte("y")), ErrReadOnly)
	require.ErrorIs(t, e.Delete([]byte("k")), ErrReadOnly)
	require.ErrorIs(t, e.Delete([]byte("absent")), ErrReadOnly, "read-only wins over not-found")

	assertValue(t, e, "k", "v")
	statsBefore.ReadOnly = true
	assert.Equal(t, statsBefore, e.Stats(), "KeyDir and counters must be unchanged")
}

func TestFault_FailedRollSyncMakesReadOnly(t *testing.T) {
	e, fsys := openFault(t)
	fillFirstSegment(t, e)
	statsBefore := e.Stats()

	fsys.arm(opSync, fault{err: errInjected})
	require.ErrorIs(t, e.Put([]byte("roll"), []byte("v")), ErrReadOnly)

	assert.True(t, e.Stats().ReadOnly)
	assert.Equal(t, []string{storage.SegmentName(1)}, dataFiles(t, fsys, testDir),
		"no new segment after a failed seal")
	_, err := e.Get([]byte("roll"))
	require.ErrorIs(t, err, ErrKeyNotFound)
	assertValue(t, e, "fill", strings.Repeat("f", fillValueLen))
	statsBefore.ReadOnly = true
	assert.Equal(t, statsBefore, e.Stats())
}

func TestFault_FailedRollCreateKeepsEngineWritable(t *testing.T) {
	e, fsys := openFault(t)
	fillFirstSegment(t, e)
	statsBefore := e.Stats()

	fsys.arm(opCreate, fault{err: errInjected})
	require.ErrorIs(t, e.Put([]byte("roll"), []byte("v")), errInjected)

	assert.False(t, e.Stats().ReadOnly)
	assert.Equal(t, statsBefore, e.Stats())

	require.NoError(t, e.Put([]byte("roll"), []byte("v")))
	assert.Equal(t, []string{storage.SegmentName(1), storage.SegmentName(2)}, dataFiles(t, fsys, testDir))
	assertValue(t, e, "roll", "v")
}

func TestFault_FailedRollSyncDirMakesReadOnly(t *testing.T) {
	e, fsys := openFault(t)
	fillFirstSegment(t, e)

	fsys.arm(opSyncDir, fault{err: errInjected})
	require.ErrorIs(t, e.Put([]byte("roll"), []byte("v")), ErrReadOnly)

	assert.True(t, e.Stats().ReadOnly)
	_, err := e.Get([]byte("roll"))
	require.ErrorIs(t, err, ErrKeyNotFound)
}

func TestFault_ReadErrorIsReturned(t *testing.T) {
	e, fsys := openFault(t)
	require.NoError(t, e.Put([]byte("k"), []byte("v")))

	fsys.arm(opRead, fault{err: errInjected})
	_, err := e.Get([]byte("k"))
	require.ErrorIs(t, err, errInjected)
	require.NotErrorIs(t, err, ErrCorrupt, "an I/O error is not corruption")

	assertValue(t, e, "k", "v")
}

func TestFault_OpenFailureReleasesLock(t *testing.T) {
	tests := []struct {
		name    string
		op      fsOp
		wantErr error
	}{
		{"list", opList, errInjected},
		{"create first segment", opCreate, errInjected},
		{"sync directory", opSyncDir, ErrReadOnly},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newFaultFS()
			fsys.arm(tc.op, fault{err: errInjected})

			_, err := Open(fsys, testDir, testOptions())
			require.ErrorIs(t, err, tc.wantErr)
			require.ErrorIs(t, err, errInjected)

			requireLockFree(t, fsys, testDir)
		})
	}
}

func TestFault_CloseSyncFailure(t *testing.T) {
	fsys := newFaultFS()
	e, err := Open(fsys, testDir, testOptions())
	require.NoError(t, err)
	require.NoError(t, e.Put([]byte("k"), []byte("v")))

	fsys.arm(opSync, fault{err: errInjected})
	err = e.Close()
	require.ErrorIs(t, err, errInjected)

	requireLockFree(t, fsys, testDir)
	require.NoError(t, e.Close(), "a second Close is a no-op even after a failed one")
	require.ErrorIs(t, e.Put([]byte("k"), []byte("v")), ErrClosed)
	_, err = e.Get([]byte("k"))
	require.ErrorIs(t, err, ErrClosed)
}

// TestCrash_PartialWrite is design-doc scenario 1: a Put torn part-way
// through its record, then a restart. The torn record must vanish and every
// earlier acknowledged write must survive.
func TestCrash_PartialWrite(t *testing.T) {
	const tornKey = "torn"
	tornValue := strings.Repeat("t", 100)
	tornSize := recordSize(tornKey, tornValue)
	tearPoints := []struct {
		name string
		at   int64 // bytes of the torn record that land
	}{
		{"inside the header", storage.HeaderSize / 2},
		{"right after the header", storage.HeaderSize},
		{"inside the value", tornSize - 10},
	}
	restarts := []struct {
		name string
		sync SyncPolicy
		opts []simfs.RestartOption
	}{
		{"process crash", SyncInterval, nil},
		{"power loss with sync=always", SyncAlways, []simfs.RestartOption{simfs.LoseUnsynced}},
	}
	for _, rs := range restarts {
		for _, tp := range tearPoints {
			t.Run(rs.name+"/"+tp.name, func(t *testing.T) {
				fsys := simfs.New()
				opts := testOptions()
				opts.Sync = rs.sync
				e, err := Open(fsys, testDir, opts)
				require.NoError(t, err)
				h := newHistory()
				for i := range 20 { // ~4.6 KiB: at least one roll
					h.put(t, e, numKey(i), strings.Repeat("x", 200))
				}
				h.put(t, e, numKey(0), "overwritten")
				h.del(t, e, numKey(3))
				statsBefore := e.Stats()

				fsys.TearAt(fsys.BytesWritten() + tp.at)
				err = e.Put([]byte(tornKey), []byte(tornValue))
				require.ErrorIs(t, err, simfs.ErrCrashed)
				require.True(t, fsys.Crashed())
				// The truncate failed too, so the engine refuses further writes.
				statsBefore.ReadOnly = true
				assert.Equal(t, statsBefore, e.Stats(), "KeyDir and counters must be unchanged")
				require.ErrorIs(t, e.Put([]byte("after"), []byte("crash")), ErrReadOnly)
				require.Error(t, e.Close(), "every handle died in the crash")

				fsys.Restart(rs.opts...)
				e2 := reopen(t, fsys, testDir, opts)
				_, err = e2.Get([]byte(tornKey))
				require.ErrorIs(t, err, ErrKeyNotFound)
				h.check(t, e2)
				assert.Equal(t, statsBefore.LiveBytes, e2.Stats().LiveBytes)
				assert.Equal(t, statsBefore.DiskBytes, totalFileSize(t, fsys, testDir),
					"the torn bytes are gone from disk")

				h.put(t, e2, tornKey, tornValue)
				h.check(t, e2)
			})
		}
	}
}

// TestFault_NoSpace is design-doc scenario 3: a write that does not fit on a
// nearly full disk fails cleanly and leaves the engine fully usable.
func TestFault_NoSpace(t *testing.T) {
	const (
		quota      = 1_000_000 // 1 MB
		fillTarget = 999_000   // about 999 KB
		bigValue   = 10 << 10  // 10 KB: cannot fit
	)
	fsys := simfs.New()
	opts := testOptions()
	opts.MaxSegmentSize = 64 << 10
	e := openTest(t, fsys, opts)
	fsys.SetQuota(quota)

	h := newHistory()
	value := strings.Repeat("v", 1000)
	for i := 0; e.Stats().DiskBytes+recordSize(numKey(i), value) <= fillTarget; i++ {
		h.put(t, e, numKey(i), value)
	}
	require.Greater(t, e.Stats().DiskBytes, int64(fillTarget-2*len(value)), "disk must be nearly full")
	statsBefore := e.Stats()
	sizeBefore := totalFileSize(t, fsys, testDir)

	err := e.Put([]byte("big"), make([]byte, bigValue))
	require.ErrorIs(t, err, ErrNoSpace)
	require.ErrorIs(t, err, vfs.ErrNoSpace, "the cause stays in the chain")

	assert.Equal(t, statsBefore, e.Stats(), "KeyDir and counters unchanged, engine still writable")
	assert.Equal(t, sizeBefore, totalFileSize(t, fsys, testDir), "the partial write was truncated away")
	_, err = e.Get([]byte("big"))
	require.ErrorIs(t, err, ErrKeyNotFound)

	h.put(t, e, "small", "s")
	h.check(t, e)

	require.NoError(t, e.Close())
	h.check(t, reopen(t, fsys, testDir, opts))
}

// TestCrash_PowerLoss restarts with every unsynced byte dropped. sync=always
// must lose no acknowledged write; sync=never may lose a suffix of the
// history but must recover exactly some prefix of it.
func TestCrash_PowerLoss(t *testing.T) {
	t.Run("sync=always keeps every acknowledged write", func(t *testing.T) {
		fsys := simfs.New()
		opts := testOptions()
		opts.Sync = SyncAlways
		e, err := Open(fsys, testDir, opts)
		require.NoError(t, err)
		h := newHistory()
		for _, o := range mixedOps() {
			if o.del {
				h.del(t, e, o.key)
			} else {
				h.put(t, e, o.key, o.value)
			}
		}

		fsys.Restart(simfs.LoseUnsynced)
		require.Error(t, e.Close(), "every handle died in the power loss")
		h.check(t, reopen(t, fsys, testDir, opts))
	})

	t.Run("sync=never recovers a prefix", func(t *testing.T) {
		fsys := simfs.New()
		opts := testOptions()
		opts.Sync = SyncNever
		e, err := Open(fsys, testDir, opts)
		require.NoError(t, err)
		ops := mixedOps()
		for _, o := range ops {
			if o.del {
				require.NoError(t, e.Delete([]byte(o.key)))
			} else {
				require.NoError(t, e.Put([]byte(o.key), []byte(o.value)))
			}
		}
		require.Greater(t, len(dataFiles(t, fsys, testDir)), 2, "rolls must have synced some segments")

		fsys.Restart(simfs.LoseUnsynced)
		require.Error(t, e.Close(), "every handle died in the power loss")
		e2 := reopen(t, fsys, testDir, opts)

		got := recoveredState(t, e2, ops)
		p := longestMatchingPrefix(ops, got)
		require.GreaterOrEqual(t, p, 0, "recovered state %v is no prefix of the history", got)
		assert.Positive(t, p, "segments sealed by a roll were synced and must survive")
		assert.Less(t, p, len(ops), "the unsynced active segment must be lost")
	})
}

// op is one acknowledged write in a power-loss history.
type op struct {
	key, value string
	del        bool
}

// mixedOps is a deterministic history of puts, overwrites and deletes over
// 15 keys, long enough to roll 4 KiB segments several times.
func mixedOps() []op {
	var ops []op
	live := make(map[string]bool)
	for i := range 60 {
		key := numKey(i % 15)
		if i%7 == 6 && live[key] {
			ops = append(ops, op{key: key, del: true})
			delete(live, key)
			continue
		}
		ops = append(ops, op{key: key, value: fmt.Sprintf("v%02d-%s", i, strings.Repeat("p", 150))})
		live[key] = true
	}
	return ops
}

// recoveredState reads every key the history touched back from e and checks
// e holds no other keys.
func recoveredState(tb testing.TB, e *Engine, ops []op) map[string]string {
	tb.Helper()
	got := make(map[string]string)
	for _, o := range ops {
		v, err := e.Get([]byte(o.key))
		if errors.Is(err, ErrKeyNotFound) {
			continue
		}
		require.NoError(tb, err)
		got[o.key] = string(v)
	}
	require.Equal(tb, len(got), e.Stats().Keys)
	return got
}

// longestMatchingPrefix returns the largest p such that applying ops[:p] to
// an empty store yields want, or -1 if no prefix does.
func longestMatchingPrefix(ops []op, want map[string]string) int {
	model := make(map[string]string)
	best := -1
	for p := 0; ; p++ {
		if maps.Equal(model, want) {
			best = p
		}
		if p == len(ops) {
			return best
		}
		if o := ops[p]; o.del {
			delete(model, o.key)
		} else {
			model[o.key] = o.value
		}
	}
}

// fillValueLen makes one "fill" record take the whole 4 KiB first segment.
const fillValueLen = testSegmentSize - storage.HeaderSize - len("fill")

// fillFirstSegment fills segment 1 exactly, so the next Put must roll.
func fillFirstSegment(tb testing.TB, e *Engine) {
	tb.Helper()
	require.NoError(tb, e.Put([]byte("fill"), []byte(strings.Repeat("f", fillValueLen))))
	_, size, ok := e.store.Active()
	require.True(tb, ok)
	require.Equal(tb, int64(testSegmentSize), size)
}
