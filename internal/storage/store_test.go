package storage

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// testLimits keeps records small: max frame is HeaderSize + 16 + 64 bytes.
var testLimits = Limits{MaxKeySize: 16, MaxValueSize: 64}

// newTestStore returns a Store over a faultFS in a temp dir, closed at cleanup
// (Close is idempotent, so tests may close it too).
func newTestStore(tb testing.TB) (*Store, *faultFS, string) {
	tb.Helper()
	dir := tb.TempDir()
	ffs := newFaultFS()
	s := NewStore(ffs, dir, StoreOptions{Limits: testLimits})
	tb.Cleanup(func() { require.NoError(tb, s.Close()) })
	return s, ffs, dir
}

// testFrame encodes a PUT of key with a value of valueLen bytes.
func testFrame(tb testing.TB, key string, valueLen int) []byte {
	tb.Helper()
	value := bytes.Repeat([]byte{byte('a' + valueLen%26)}, valueLen)
	frame, err := Encode(Record{Timestamp: 1, Type: TypePut, Key: []byte(key), Value: value}, testLimits)
	require.NoError(tb, err)
	return frame
}

// appendFrame appends frame and checks it landed in wantID.
func appendFrame(tb testing.TB, s *Store, frame []byte, wantID uint32) int64 {
	tb.Helper()
	id, off, err := s.Append(frame)
	require.NoError(tb, err)
	require.Equal(tb, wantID, id)
	return off
}

func segPath(dir string, id uint32) string { return filepath.Join(dir, SegmentName(id)) }

// opsSince returns what ffs logged after the first n operations.
func opsSince(ffs *faultFS, n int) []string { return ffs.ops()[n:] }

func listDir(tb testing.TB, dir string) []string {
	tb.Helper()
	names, err := vfs.OSFS{}.List(dir)
	require.NoError(tb, err)
	return names
}

func TestStore_AppendThenReadAtSeesBytesImmediately(t *testing.T) {
	t.Parallel()
	s, _, dir := newTestStore(t)
	require.NoError(t, s.CreateActive(1))

	var end int64
	for i := range 6 {
		frame := testFrame(t, fmt.Sprintf("key-%d", i), i*12)
		off := appendFrame(t, s, frame, 1)
		assert.Equal(t, end, off, "records are packed back to back")
		end += int64(len(frame))

		got, err := s.ReadAt(1, off, int64(len(frame)))
		require.NoError(t, err)
		assert.Equal(t, frame, got)
		rec, err := Decode(got, testLimits)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("key-%d", i), string(rec.Key))
	}

	id, size, ok := s.Active()
	assert.True(t, ok)
	assert.Equal(t, uint32(1), id)
	assert.Equal(t, end, size)
	assert.Equal(t, end, s.DiskBytes())
	assert.Equal(t, end, diskSize(t, segPath(dir, 1)))
}

func TestStore_CreateActiveCreatesThenSyncsDir(t *testing.T) {
	t.Parallel()
	s, ffs, dir := newTestStore(t)
	require.NoError(t, s.CreateActive(1))
	assert.Equal(t, []string{"create " + SegmentName(1), "syncdir " + filepath.Base(dir)}, ffs.ops())
	assert.Equal(t, []string{SegmentName(1)}, listDir(t, dir))
}

func TestStore_CreateActiveRejectsBadIDs(t *testing.T) {
	t.Parallel()

	t.Run("second active segment", func(t *testing.T) {
		t.Parallel()
		s, _, dir := newTestStore(t)
		require.NoError(t, s.CreateActive(1))
		require.ErrorIs(t, s.CreateActive(2), ErrSegmentOrder)
		assert.Equal(t, []string{SegmentName(1)}, listDir(t, dir))
	})

	t.Run("file from an earlier run", func(t *testing.T) {
		t.Parallel()
		s, _, dir := newTestStore(t)
		f, err := vfs.OSFS{}.Create(segPath(dir, 5))
		require.NoError(t, err)
		require.NoError(t, f.Close())

		require.ErrorIs(t, s.CreateActive(5), fs.ErrExist)
		_, _, ok := s.Active()
		assert.False(t, ok)
	})

	t.Run("not above registered files", func(t *testing.T) {
		t.Parallel()
		s, _, dir := newTestStore(t)
		f, err := vfs.OSFS{}.Create(segPath(dir, 3))
		require.NoError(t, err)
		require.NoError(t, f.Close())
		require.NoError(t, s.Register(FileName{Kind: KindSegment, ID: 3}))

		require.ErrorIs(t, s.CreateActive(3), ErrSegmentOrder)
		require.ErrorIs(t, s.CreateActive(2), ErrSegmentOrder)
		assert.Equal(t, []string{SegmentName(3)}, listDir(t, dir), "a refused call touches no file")
		require.NoError(t, s.CreateActive(4))
	})
}

func TestStore_RollSyncsThenCreatesThenSyncsDir(t *testing.T) {
	t.Parallel()
	s, ffs, dir := newTestStore(t)
	require.NoError(t, s.CreateActive(1))
	f1 := testFrame(t, "one", 10)
	appendFrame(t, s, f1, 1)

	mark := len(ffs.ops())
	require.NoError(t, s.Roll(2))
	assert.Equal(t, []string{
		"sync " + SegmentName(1),
		"create " + SegmentName(2),
		"syncdir " + filepath.Base(dir),
	}, opsSince(ffs, mark), "invariant 7: fsync before sealing, SyncDir after create")

	f2 := testFrame(t, "two", 20)
	assert.Equal(t, int64(0), appendFrame(t, s, f2, 2), "the new segment starts empty")

	got, err := s.ReadAt(1, 0, int64(len(f1)))
	require.NoError(t, err)
	assert.Equal(t, f1, got, "the sealed segment stays readable")
	got, err = s.ReadAt(2, 0, int64(len(f2)))
	require.NoError(t, err)
	assert.Equal(t, f2, got)

	id, size, ok := s.Active()
	assert.True(t, ok)
	assert.Equal(t, uint32(2), id)
	assert.Equal(t, int64(len(f2)), size)
	assert.Equal(t, int64(len(f1)+len(f2)), s.DiskBytes())
	assert.Equal(t, []string{SegmentName(1), SegmentName(2)}, listDir(t, dir))

	s.mu.RLock()
	sealed := s.segs[1]
	s.mu.RUnlock()
	assert.True(t, sealed.sealed.Load())
	_, err = sealed.append(f1)
	require.Error(t, err, "a sealed segment refuses appends")
	assert.Equal(t, int64(len(f1)), diskSize(t, segPath(dir, 1)))
}

func TestStore_RollRejectsBadIDs(t *testing.T) {
	t.Parallel()
	s, ffs, dir := newTestStore(t)
	require.ErrorIs(t, s.Roll(1), ErrNoActiveSegment)
	_, _, err := s.Append(testFrame(t, "k", 1))
	require.ErrorIs(t, err, ErrNoActiveSegment)

	require.NoError(t, s.CreateActive(5))
	mark := len(ffs.ops())
	require.ErrorIs(t, s.Roll(5), ErrSegmentOrder)
	require.ErrorIs(t, s.Roll(4), ErrSegmentOrder)
	assert.Empty(t, opsSince(ffs, mark), "a refused roll neither syncs nor creates")
	assert.Equal(t, []string{SegmentName(5)}, listDir(t, dir))

	require.NoError(t, s.Roll(9), "gaps between IDs are normal")
}

func TestStore_RegisterOpensSealedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// An earlier run left two segments and a compaction output.
	first := NewStore(vfs.OSFS{}, dir, StoreOptions{Limits: testLimits})
	require.NoError(t, first.CreateActive(1))
	f1, f2 := testFrame(t, "one", 5), testFrame(t, "two", 50)
	appendFrame(t, first, f1, 1)
	require.NoError(t, first.Roll(2))
	appendFrame(t, first, f2, 2)
	require.NoError(t, first.Close())
	f3 := testFrame(t, "three", 0)
	cf, err := vfs.OSFS{}.Create(filepath.Join(dir, CompactName(3)))
	require.NoError(t, err)
	_, err = cf.Write(f3)
	require.NoError(t, err)
	require.NoError(t, cf.Close())

	s := NewStore(newFaultFS(), dir, StoreOptions{Limits: testLimits})
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.Register(FileName{Kind: KindSegment, ID: 1}))
	require.NoError(t, s.Register(FileName{Kind: KindSegment, ID: 2}))
	require.NoError(t, s.Register(FileName{Kind: KindCompact, ID: 3}))
	require.NoError(t, s.CreateActive(4))

	for id, want := range map[uint32][]byte{1: f1, 2: f2, 3: f3} {
		got, err := s.ReadAt(id, 0, int64(len(want)))
		require.NoError(t, err, "file %d", id)
		assert.Equal(t, want, got, "file %d", id)
	}
	assert.Equal(t, int64(len(f1)+len(f2)+len(f3)), s.DiskBytes())

	s.mu.RLock()
	assert.True(t, s.segs[3].sealed.Load(), "registered files are sealed")
	s.mu.RUnlock()

	require.ErrorIs(t, s.Register(FileName{Kind: KindCompact, ID: 3}), ErrSegmentOrder, "duplicate ID")
	require.ErrorIs(t, s.Register(FileName{Kind: KindSegment, ID: 4}), ErrSegmentOrder, "the active ID")
	require.ErrorIs(t, s.Register(FileName{Kind: KindSegment, ID: 7}), ErrSegmentOrder, "above the active segment")
	require.Error(t, s.Register(FileName{Kind: KindCompactTmp, ID: 0}), "a .tmp file holds no readable records")
	require.ErrorIs(t, s.Register(FileName{Kind: KindSegment, ID: 0}), fs.ErrNotExist)
}

func TestStore_ReadAtRejectsBadRanges(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t)
	require.NoError(t, s.CreateActive(1))
	frame := testFrame(t, "key", 30)
	appendFrame(t, s, frame, 1)
	size := int64(len(frame))

	tests := []struct {
		name   string
		id     uint32
		off, n int64
		want   []error
	}{
		{"unknown file", 7, 0, size, []error{ErrSegmentNotFound}},
		{"zero length", 1, 0, 0, []error{ErrCorrupt}},
		{"negative length", 1, 0, -1, []error{ErrCorrupt}},
		{"longer than the largest record", 1, 0, testLimits.MaxRecordSize() + 1, []error{ErrCorrupt}},
		{"negative offset", 1, -1, size, []error{ErrCorrupt, io.ErrUnexpectedEOF}},
		{"runs past the end", 1, 1, size, []error{ErrCorrupt, io.ErrUnexpectedEOF}},
		{"starts past the end", 1, size + 100, 1, []error{ErrCorrupt, io.ErrUnexpectedEOF}},
		{"offset near overflow", 1, math.MaxInt64, size, []error{ErrCorrupt, io.ErrUnexpectedEOF}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := s.ReadAt(tc.id, tc.off, tc.n)
			for _, want := range tc.want {
				require.ErrorIs(t, err, want)
			}
			assert.Nil(t, got)
		})
	}
}

// buildSealed returns a Store with sealed files 1..n (frames[i] at offset 0 of
// file i+1) and active segment n+1.
func buildSealed(tb testing.TB, n int) (s *Store, ffs *faultFS, dir string, frames [][]byte) {
	tb.Helper()
	s, ffs, dir = newTestStore(tb)
	require.NoError(tb, s.CreateActive(1))
	for i := range n {
		id := uint32(i + 1)
		frame := testFrame(tb, fmt.Sprintf("k%d", id), 10*i)
		appendFrame(tb, s, frame, id)
		frames = append(frames, frame)
		require.NoError(tb, s.Roll(id+1))
	}
	return s, ffs, dir, frames
}

func TestStore_DropClosesDeletesAndSyncsDir(t *testing.T) {
	t.Parallel()
	s, ffs, dir, frames := buildSealed(t, 3)
	before := s.DiskBytes()

	mark := len(ffs.ops())
	require.NoError(t, s.Drop([]uint32{1, 2, 1}), "duplicate IDs are ignored")
	assert.Equal(t, []string{
		"close " + SegmentName(1), "remove " + SegmentName(1),
		"close " + SegmentName(2), "remove " + SegmentName(2),
		"syncdir " + filepath.Base(dir),
	}, opsSince(ffs, mark))

	for _, id := range []uint32{1, 2} {
		_, err := s.ReadAt(id, 0, int64(len(frames[id-1])))
		require.ErrorIs(t, err, ErrSegmentNotFound, "file %d", id)
	}
	got, err := s.ReadAt(3, 0, int64(len(frames[2])))
	require.NoError(t, err)
	assert.Equal(t, frames[2], got)
	assert.Equal(t, before-int64(len(frames[0])+len(frames[1])), s.DiskBytes())
	assert.Equal(t, []string{SegmentName(3), SegmentName(4)}, listDir(t, dir))
}

func TestStore_DropChecksEveryIDFirst(t *testing.T) {
	t.Parallel()
	s, ffs, dir, frames := buildSealed(t, 2)
	mark := len(ffs.ops())

	require.ErrorIs(t, s.Drop([]uint32{1, 99}), ErrSegmentNotFound)
	require.ErrorIs(t, s.Drop([]uint32{1, 3}), ErrSegmentOrder, "3 is the active segment")
	require.NoError(t, s.Drop(nil))
	assert.Empty(t, opsSince(ffs, mark), "nothing was closed, removed or synced")

	got, err := s.ReadAt(1, 0, int64(len(frames[0])))
	require.NoError(t, err)
	assert.Equal(t, frames[0], got)
	assert.Len(t, listDir(t, dir), 3)
}

// TestStore_DropClosesOnlyAfterRemovingFromSet (ARCHITECTURE §6): at Close the
// file has left the set and Store.mu is not held.
func TestStore_DropClosesOnlyAfterRemovingFromSet(t *testing.T) {
	t.Parallel()
	s, ffs, _, _ := buildSealed(t, 1)

	var closed string
	ffs.onNext(opClose, func(name string) {
		closed = filepath.Base(name)
		if !assert.True(t, s.mu.TryRLock(), "Store.mu must be released before closing") {
			return
		}
		_, present := s.segs[1]
		s.mu.RUnlock()
		assert.False(t, present, "file must leave the set before it is closed")
	})
	require.NoError(t, s.Drop([]uint32{1}))
	assert.Equal(t, SegmentName(1), closed)
}

func TestStore_SyncActive(t *testing.T) {
	t.Parallel()
	s, ffs, _ := newTestStore(t)
	require.ErrorIs(t, s.SyncActive(), ErrNoActiveSegment)

	require.NoError(t, s.CreateActive(1))
	appendFrame(t, s, testFrame(t, "k", 1), 1)
	mark := len(ffs.ops())
	require.NoError(t, s.SyncActive())
	assert.Equal(t, []string{"sync " + SegmentName(1)}, opsSince(ffs, mark))
}

func TestStore_CloseSyncsActiveAndClosesEverything(t *testing.T) {
	t.Parallel()
	s, ffs, dir, frames := buildSealed(t, 1)
	f2 := testFrame(t, "last", 40)
	appendFrame(t, s, f2, 2)

	mark := len(ffs.ops())
	require.NoError(t, s.Close())
	assert.Equal(t, []string{
		"sync " + SegmentName(2), "close " + SegmentName(1), "close " + SegmentName(2),
	}, opsSince(ffs, mark))
	require.NoError(t, s.Close(), "Close is idempotent")

	_, _, err := s.Append(f2)
	require.ErrorIs(t, err, ErrStoreClosed)
	_, err = s.ReadAt(1, 0, int64(len(frames[0])))
	require.ErrorIs(t, err, ErrStoreClosed)
	require.ErrorIs(t, s.Roll(3), ErrStoreClosed)
	require.ErrorIs(t, s.CreateActive(3), ErrStoreClosed)
	require.ErrorIs(t, s.Register(FileName{Kind: KindSegment, ID: 1}), ErrStoreClosed)
	require.ErrorIs(t, s.Drop([]uint32{1}), ErrStoreClosed)
	require.ErrorIs(t, s.SyncActive(), ErrStoreClosed)
	assert.Zero(t, s.DiskBytes())
	_, _, ok := s.Active()
	assert.False(t, ok)

	// Everything written is still on disk for the next run.
	again := NewStore(vfs.OSFS{}, dir, StoreOptions{Limits: testLimits})
	t.Cleanup(func() { require.NoError(t, again.Close()) })
	require.NoError(t, again.Register(FileName{Kind: KindSegment, ID: 2}))
	got, err := again.ReadAt(2, 0, int64(len(f2)))
	require.NoError(t, err)
	assert.Equal(t, f2, got)
}
