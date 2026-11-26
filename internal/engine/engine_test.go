package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/index"
	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
	"github.com/Maks9m/TinyCask/internal/vfs/simfs"
)

func TestOpen_CreatesDirectoryLockAndFirstSegment(t *testing.T) {
	_, fsys := openSim(t)

	names, err := fsys.List(testDir)
	require.NoError(t, err)
	assert.Equal(t, []string{lockFileName, storage.SegmentName(firstSegmentID)}, names)
	assert.Zero(t, fileSize(t, fsys, testDir, storage.SegmentName(firstSegmentID)))
}

func TestOpen_SecondOpenReturnsErrLocked(t *testing.T) {
	e, fsys := openSim(t)

	_, err := Open(fsys, testDir, testOptions())
	require.ErrorIs(t, err, ErrLocked)
	require.ErrorIs(t, err, vfs.ErrLocked)

	// The failed Open must not disturb the engine that holds the lock.
	require.NoError(t, e.Put([]byte("k"), []byte("v")))
	got, err := e.Get([]byte("k"))
	require.NoError(t, err)
	assert.Equal(t, []byte("v"), got)
}

func TestOpen_NonEmptyDirectory(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantErr error
	}{
		{"segment", storage.SegmentName(7), errRecoveryNotImplemented},
		{"compaction output", storage.CompactName(3), errRecoveryNotImplemented},
		{"compaction tmp", storage.CompactTmpName(3), errRecoveryNotImplemented},
		{"malformed data name", "seg_12.data", storage.ErrBadFileName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := simfs.New()
			require.NoError(t, fsys.MkdirAll(testDir))
			f, err := fsys.Create(filepath.Join(testDir, tc.file))
			require.NoError(t, err)
			require.NoError(t, f.Close())

			_, err = Open(fsys, testDir, testOptions())
			require.ErrorIs(t, err, tc.wantErr)

			requireLockFree(t, fsys, testDir)
			assert.Equal(t, []string{lockFileName, tc.file}, mustList(t, fsys, testDir),
				"a rejected Open must not create files")
		})
	}
}

func TestOpen_IgnoresUnknownFiles(t *testing.T) {
	fsys := simfs.New()
	require.NoError(t, fsys.MkdirAll(testDir))
	f, err := fsys.Create(filepath.Join(testDir, "notes.txt"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	openTest(t, fsys, testOptions())
	assert.Equal(t, []string{storage.SegmentName(firstSegmentID)}, dataFiles(t, fsys, testDir))
}

func TestOpen_RejectsNegativeSegmentSize(t *testing.T) {
	fsys := simfs.New()
	_, err := Open(fsys, testDir, Options{MaxSegmentSize: -1})
	require.Error(t, err)
	assert.Empty(t, mustList(t, fsys, "/"), "invalid options must fail before touching the FS")
}

func TestOpen_Defaults(t *testing.T) {
	e := openTest(t, simfs.New(), Options{})
	assert.Equal(t, DefaultMaxSegmentSize, e.opts.MaxSegmentSize)
	assert.Equal(t, storage.DefaultLimits(), e.limits)
	assert.NotNil(t, e.opts.Now)
}

func TestEngine_PutGetOverwrite(t *testing.T) {
	e, _ := openSim(t)

	require.NoError(t, e.Put([]byte("a"), []byte("1")))
	require.NoError(t, e.Put([]byte("b"), []byte("2")))
	require.NoError(t, e.Put([]byte("a"), []byte("3")))

	assertValue(t, e, "a", "3")
	assertValue(t, e, "b", "2")
	_, err := e.Get([]byte("missing"))
	require.ErrorIs(t, err, ErrKeyNotFound)
}

func TestEngine_EmptyValueIsStored(t *testing.T) {
	e, _ := openSim(t)

	require.NoError(t, e.Put([]byte("k"), nil))
	got, err := e.Get([]byte("k"))
	require.NoError(t, err)
	assert.NotNil(t, got, "an empty value is present, not missing")
	assert.Empty(t, got)
}

func TestEngine_DoesNotRetainCallerSlices(t *testing.T) {
	e, _ := openSim(t)
	key, value := []byte("key"), []byte("value")

	require.NoError(t, e.Put(key, value))
	copy(key, "XXX")
	copy(value, "XXXXX")

	assertValue(t, e, "key", "value")
}

func TestEngine_GetReturnsCallerOwnedCopy(t *testing.T) {
	e, _ := openSim(t)
	require.NoError(t, e.Put([]byte("k"), []byte("value")))

	got, err := e.Get([]byte("k"))
	require.NoError(t, err)
	copy(got, "XXXXX")

	assertValue(t, e, "k", "value")
}

func TestEngine_SizeLimits(t *testing.T) {
	const maxKey, maxValue = 16, 64
	opts := testOptions()
	opts.MaxKeySize, opts.MaxValueSize = maxKey, maxValue
	e := openTest(t, simfs.New(), opts)
	okKey := []byte(strings.Repeat("k", maxKey))
	longKey := []byte(strings.Repeat("k", maxKey+1))
	okValue := []byte(strings.Repeat("v", maxValue))
	longValue := []byte(strings.Repeat("v", maxValue+1))

	tests := []struct {
		name    string
		op      func() error
		wantErr error
	}{
		{"put nil key", func() error { return e.Put(nil, okValue) }, ErrInvalidKey},
		{"put empty key", func() error { return e.Put([]byte{}, okValue) }, ErrInvalidKey},
		{"put key over limit", func() error { return e.Put(longKey, okValue) }, ErrKeyTooLarge},
		{"put value over limit", func() error { return e.Put(okKey, longValue) }, ErrValueTooLarge},
		{"get empty key", func() error { _, err := e.Get(nil); return err }, ErrInvalidKey},
		{"get key over limit", func() error { _, err := e.Get(longKey); return err }, ErrKeyTooLarge},
		{"delete empty key", func() error { return e.Delete(nil) }, ErrInvalidKey},
		{"delete key over limit", func() error { return e.Delete(longKey) }, ErrKeyTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := e.Stats()
			require.ErrorIs(t, tc.op(), tc.wantErr)
			assert.Equal(t, before, e.Stats(), "a rejected call must write nothing")
		})
	}

	t.Run("exact limits are accepted", func(t *testing.T) {
		require.NoError(t, e.Put(okKey, okValue))
		got, err := e.Get(okKey)
		require.NoError(t, err)
		assert.Equal(t, okValue, got)
		require.NoError(t, e.Delete(okKey))
	})
}

func TestEngine_DeleteRemovesKey(t *testing.T) {
	e, fsys := openSim(t)
	require.NoError(t, e.Put([]byte("k"), []byte("v")))
	sizeBefore := totalFileSize(t, fsys, testDir)

	require.NoError(t, e.Delete([]byte("k")))

	_, err := e.Get([]byte("k"))
	require.ErrorIs(t, err, ErrKeyNotFound)
	assert.Equal(t, sizeBefore+storage.RecordSize(len("k"), 0), totalFileSize(t, fsys, testDir),
		"Delete appends exactly one tombstone")
	require.ErrorIs(t, e.Delete([]byte("k")), ErrKeyNotFound)
}

func TestEngine_DeleteMissingKeyWritesNothing(t *testing.T) {
	e, fsys := openSim(t)
	require.NoError(t, e.Put([]byte("present"), []byte("v")))
	sizeBefore := totalFileSize(t, fsys, testDir)
	statsBefore := e.Stats()

	require.ErrorIs(t, e.Delete([]byte("absent")), ErrKeyNotFound)

	assert.Equal(t, sizeBefore, totalFileSize(t, fsys, testDir))
	assert.Equal(t, statsBefore, e.Stats())
}

func TestEngine_SegmentsRollAtMaxSize(t *testing.T) {
	e, fsys := openSim(t)
	value := strings.Repeat("v", 200)
	const keys = 60 // ~13.6 KiB of records: at least 4 segments

	for i := range keys {
		require.NoError(t, e.Put(fmt.Appendf(nil, "key-%03d", i), []byte(value)))
	}

	files := dataFiles(t, fsys, testDir)
	require.GreaterOrEqual(t, len(files), 4, "4 KiB segments must have rolled")
	for i, name := range files {
		want := storage.SegmentName(firstSegmentID + uint32(i))
		assert.Equal(t, want, name, "segment IDs are consecutive")
		size := fileSize(t, fsys, testDir, name)
		assert.LessOrEqual(t, size, int64(testSegmentSize), "%s over the roll threshold", name)
		assert.Positive(t, size, "%s is empty", name)
	}
	for i := range keys {
		assertValue(t, e, fmt.Sprintf("key-%03d", i), value)
	}
	assert.Equal(t, totalFileSize(t, fsys, testDir), e.Stats().DiskBytes)
}

func TestEngine_RollIsExactAtThreshold(t *testing.T) {
	e, fsys := openSim(t)
	// Two records that fill the segment exactly, then one more byte's worth.
	half := testSegmentSize / 2
	value := strings.Repeat("v", half-storage.HeaderSize-len("k1"))
	require.Equal(t, int64(half), recordSize("k1", value))

	require.NoError(t, e.Put([]byte("k1"), []byte(value)))
	require.NoError(t, e.Put([]byte("k2"), []byte(value)))
	assert.Len(t, dataFiles(t, fsys, testDir), 1, "a segment may be filled exactly")

	require.NoError(t, e.Put([]byte("k3"), []byte("x")))
	assert.Len(t, dataFiles(t, fsys, testDir), 2, "the next byte must go to a new segment")
	assert.Equal(t, int64(testSegmentSize), fileSize(t, fsys, testDir, storage.SegmentName(1)))
}

func TestEngine_OversizedRecordGetsOwnSegment(t *testing.T) {
	e, fsys := openSim(t)
	big := strings.Repeat("b", 2*testSegmentSize)

	require.NoError(t, e.Put([]byte("small"), []byte("1")))
	require.NoError(t, e.Put([]byte("big"), []byte(big)))
	require.NoError(t, e.Put([]byte("after"), []byte("2")))

	assert.Equal(t, []string{storage.SegmentName(1), storage.SegmentName(2), storage.SegmentName(3)},
		dataFiles(t, fsys, testDir))
	assert.Equal(t, recordSize("big", big), fileSize(t, fsys, testDir, storage.SegmentName(2)))
	assertValue(t, e, "small", "1")
	assertValue(t, e, "big", big)
	assertValue(t, e, "after", "2")
}

func TestEngine_StatsAfterOverwritesAndDeletes(t *testing.T) {
	e, fsys := openSim(t)
	assert.Equal(t, Stats{}, e.Stats())

	require.NoError(t, e.Put([]byte("a"), []byte("one")))
	require.NoError(t, e.Put([]byte("b"), []byte("two")))
	require.NoError(t, e.Put([]byte("a"), []byte("three"))) // overwrite
	require.NoError(t, e.Put([]byte("c"), []byte("x")))
	require.NoError(t, e.Delete([]byte("b")))
	require.ErrorIs(t, e.Delete([]byte("b")), ErrKeyNotFound)

	tomb := storage.RecordSize(1, 0)
	wantDisk := recordSize("a", "one") + recordSize("b", "two") + recordSize("a", "three") +
		recordSize("c", "x") + tomb
	wantLive := recordSize("a", "three") + recordSize("c", "x")

	st := e.Stats()
	assert.Equal(t, 2, st.Keys)
	assert.Equal(t, wantLive, st.LiveBytes)
	assert.Equal(t, wantDisk, st.DiskBytes)
	assert.InDelta(t, 1-float64(wantLive)/float64(wantDisk), st.GarbageRatio, 1e-12)
	assert.False(t, st.ReadOnly)

	// Cross-check the counters against the files and KeyDir directly.
	assert.Equal(t, totalFileSize(t, fsys, testDir), st.DiskBytes)
	assert.Equal(t, e.store.DiskBytes(), st.DiskBytes)
	assert.Equal(t, keyDirLiveBytes(t, e, "a", "b", "c"), st.LiveBytes)

	require.NoError(t, e.Delete([]byte("a")))
	require.NoError(t, e.Delete([]byte("c")))
	st = e.Stats()
	assert.Zero(t, st.Keys)
	assert.Zero(t, st.LiveBytes)
	assert.InDelta(t, 1.0, st.GarbageRatio, 1e-12)
}

func TestEngine_TimestampComesFromClock(t *testing.T) {
	e, _ := openSim(t)
	require.NoError(t, e.Put([]byte("k"), []byte("v")))

	entry, ok := e.keydir.Get([]byte("k"))
	require.True(t, ok)
	assert.Equal(t, uint64(testClock().UnixNano()), entry.Timestamp)
}

func TestEngine_CloseIsIdempotentAndFinal(t *testing.T) {
	fsys := simfs.New()
	e, err := Open(fsys, testDir, testOptions())
	require.NoError(t, err)
	require.NoError(t, e.Put([]byte("k"), []byte("v")))

	require.NoError(t, e.Close())
	require.NoError(t, e.Close())

	require.ErrorIs(t, e.Put([]byte("k"), []byte("v2")), ErrClosed)
	_, err = e.Get([]byte("k"))
	require.ErrorIs(t, err, ErrClosed)
	require.ErrorIs(t, e.Delete([]byte("k")), ErrClosed)
	require.ErrorIs(t, e.Delete([]byte("absent")), ErrClosed)
	// ErrClosed wins over argument errors.
	require.ErrorIs(t, e.Put(nil, []byte("v")), ErrClosed)
	require.ErrorIs(t, e.Delete(nil), ErrClosed)
	_, err = e.Get(nil)
	require.ErrorIs(t, err, ErrClosed)

	requireLockFree(t, fsys, testDir)
	seg := filepath.Join(testDir, storage.SegmentName(firstSegmentID))
	synced, err := fsys.SyncedSize(seg)
	require.NoError(t, err)
	assert.Equal(t, recordSize("k", "v"), synced, "Close must fsync the active segment")
	assert.Equal(t, 1, e.Stats().Keys, "Stats keeps reporting the final state")
}

func TestEngine_GetDetectsCorruptIndex(t *testing.T) {
	e, _ := openSim(t)
	require.NoError(t, e.Put([]byte("a"), []byte("aaaa")))
	require.NoError(t, e.Put([]byte("b"), []byte("bbbb")))
	entryB, ok := e.keydir.Get([]byte("b"))
	require.True(t, ok)

	tests := []struct {
		name    string
		entry   index.CommandIndex
		wantErr error
	}{
		{"points at another key's record", entryB, ErrCorrupt},
		{"points past the end of the file", index.CommandIndex{
			FileID: entryB.FileID, ValueSize: 4, ValueOffset: entryB.ValueOffset + 100,
		}, ErrCorrupt},
		{"points into the middle of a record", index.CommandIndex{
			FileID: entryB.FileID, ValueSize: 4, ValueOffset: entryB.ValueOffset - 2,
		}, ErrCorrupt},
		{"file that never existed", index.CommandIndex{
			FileID: 99, ValueSize: 4, ValueOffset: entryB.ValueOffset,
		}, storage.ErrSegmentNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.keydir.Put([]byte("a"), tc.entry)
			_, err := e.Get([]byte("a"))
			require.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestEngine_ConcurrentWritersAndReaders(t *testing.T) {
	e, fsys := openSim(t)
	const writers, keysPerWriter, rounds = 4, 20, 5

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for r := range rounds {
				for k := range keysPerWriter {
					key := fmt.Appendf(nil, "w%d-k%02d", w, k)
					assert.NoError(t, e.Put(key, fmt.Appendf(nil, "round-%d", r)))
				}
				// Delete the odd keys every round; the last round deletes them for good.
				for k := 1; k < keysPerWriter; k += 2 {
					assert.NoError(t, e.Delete(fmt.Appendf(nil, "w%d-k%02d", w, k)))
				}
			}
		})
		wg.Go(func() {
			for range rounds * keysPerWriter {
				_, err := e.Get(fmt.Appendf(nil, "w%d-k00", w))
				if err != nil {
					assert.ErrorIs(t, err, ErrKeyNotFound)
				}
			}
		})
	}
	wg.Wait()

	// Model: even keys hold the last round's value, odd keys are gone.
	for w := range writers {
		for k := range keysPerWriter {
			key := fmt.Sprintf("w%d-k%02d", w, k)
			if k%2 == 0 {
				assertValue(t, e, key, fmt.Sprintf("round-%d", rounds-1))
				continue
			}
			_, err := e.Get([]byte(key))
			assert.ErrorIs(t, err, ErrKeyNotFound, key)
		}
	}
	st := e.Stats()
	assert.Equal(t, writers*keysPerWriter/2, st.Keys)
	assert.Equal(t, totalFileSize(t, fsys, testDir), st.DiskBytes)
	assert.Equal(t, int64(st.Keys)*recordSize("w0-k00", "round-4"), st.LiveBytes)
}

func assertValue(tb testing.TB, e *Engine, key, want string) {
	tb.Helper()
	got, err := e.Get([]byte(key))
	if assert.NoError(tb, err, key) {
		assert.Equal(tb, want, string(got), key)
	}
}

// keyDirLiveBytes sums the record sizes KeyDir points at for keys.
func keyDirLiveBytes(tb testing.TB, e *Engine, keys ...string) int64 {
	tb.Helper()
	var total int64
	for _, k := range keys {
		if entry, ok := e.keydir.Get([]byte(k)); ok {
			total += storage.RecordSize(len(k), entry.ValueSize)
		}
	}
	return total
}

func mustList(tb testing.TB, fsys vfs.FS, dir string) []string {
	tb.Helper()
	names, err := fsys.List(dir)
	require.NoError(tb, err)
	return names
}
