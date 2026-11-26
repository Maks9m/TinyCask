package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
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
