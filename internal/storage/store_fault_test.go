package storage

import (
	"io"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// requireDamaged checks that s refuses every kind of write with
// ErrSegmentDamaged and touches no file while doing so (invariant 8).
func requireDamaged(tb testing.TB, s *Store, ffs *faultFS) {
	tb.Helper()
	mark := len(ffs.ops())
	_, _, err := s.Append(testFrame(tb, "late", 1))
	require.ErrorIs(tb, err, ErrSegmentDamaged)
	require.ErrorIs(tb, s.SyncActive(), ErrSegmentDamaged)
	require.ErrorIs(tb, s.Roll(1000), ErrSegmentDamaged)
	assert.Empty(tb, opsSince(ffs, mark), "a damaged store neither writes nor retries an fsync")
}

// requireReadable checks that frame is still served at (id, off).
func requireReadable(tb testing.TB, s *Store, id uint32, off int64, frame []byte) {
	tb.Helper()
	got, err := s.ReadAt(id, off, int64(len(frame)))
	require.NoError(tb, err)
	assert.Equal(tb, frame, got)
}

func TestFault_StoreAppend_FailedWriteIsTruncatedAway(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		flt     fault
		wantErr error
	}{
		{"write fails before any byte", fault{err: errInjected}, errInjected},
		{"torn write on a full disk", fault{n: 7, err: vfs.ErrNoSpace}, vfs.ErrNoSpace},
		{"short write without an error", fault{n: 7}, io.ErrShortWrite},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ffs, dir := newTestStore(t)
			require.NoError(t, s.CreateActive(1))
			good := testFrame(t, "good", 20)
			appendFrame(t, s, good, 1)
			end := int64(len(good))

			ffs.arm(opWrite, tc.flt)
			mark := len(ffs.ops())
			_, _, err := s.Append(testFrame(t, "lost", 30))
			require.ErrorIs(t, err, tc.wantErr)
			assert.NotErrorIs(t, err, ErrSegmentDamaged)
			assert.Equal(t, []string{"write " + SegmentName(1), "truncate " + SegmentName(1)}, opsSince(ffs, mark))
			assert.Equal(t, end, diskSize(t, segPath(dir, 1)), "torn bytes are gone from disk")
			_, size, _ := s.Active()
			assert.Equal(t, end, size)

			// The store stays writable and the next record lands where the
			// failed one would have.
			next := testFrame(t, "next", 10)
			assert.Equal(t, end, appendFrame(t, s, next, 1))
			requireReadable(t, s, 1, 0, good)
			requireReadable(t, s, 1, end, next)
		})
	}
}

func TestFault_StoreAppend_FailedTruncateDamagesStore(t *testing.T) {
	t.Parallel()
	s, ffs, dir := newTestStore(t)
	require.NoError(t, s.CreateActive(1))
	good := testFrame(t, "good", 20)
	appendFrame(t, s, good, 1)

	ffs.arm(opWrite, fault{n: 5, err: errInjected})
	ffs.arm(opTruncate, fault{err: errInjected})
	_, _, err := s.Append(testFrame(t, "torn", 30))
	require.ErrorIs(t, err, errInjected)
	require.ErrorIs(t, err, ErrSegmentDamaged)

	requireDamaged(t, s, ffs)
	assert.Equal(t, []string{SegmentName(1)}, listDir(t, dir))
	_, size, _ := s.Active()
	assert.Equal(t, int64(len(good)), size, "the torn tail is never exposed to readers")
	requireReadable(t, s, 1, 0, good)

	mark := len(ffs.ops())
	require.NoError(t, s.Close())
	assert.Equal(t, []string{"close " + SegmentName(1)}, opsSince(ffs, mark), "Close does not sync a damaged segment")
}

func TestFault_StoreSyncActive_FailedSyncDamagesStore(t *testing.T) {
	t.Parallel()
	s, ffs, _ := newTestStore(t)
	require.NoError(t, s.CreateActive(1))
	good := testFrame(t, "good", 20)
	appendFrame(t, s, good, 1)

	ffs.arm(opSync, fault{err: errInjected})
	err := s.SyncActive()
	require.ErrorIs(t, err, errInjected)
	require.ErrorIs(t, err, ErrSegmentDamaged)

	requireDamaged(t, s, ffs)
	requireReadable(t, s, 1, 0, good)
	mark := len(ffs.ops())
	require.NoError(t, s.Close())
	assert.NotContains(t, opsSince(ffs, mark), "sync "+SegmentName(1), "an fsync error is never retried")
}

// TestFault_StoreSyncActive_SegmentSealedAndDroppedMidSync replays the race
// between the interval fsync and a roll plus compaction: SyncActive picks
// segment 1, then before its fsync runs, Roll seals segment 1 (syncing it) and
// Drop closes it. The late fsync fails on the closed handle; that must not
// damage the Store, because segment 1 was already synced by Roll.
func TestFault_StoreSyncActive_SegmentSealedAndDroppedMidSync(t *testing.T) {
	t.Parallel()
	s, ffs, _ := newTestStore(t)
	require.NoError(t, s.CreateActive(1))
	appendFrame(t, s, testFrame(t, "old", 5), 1)

	ffs.onNext(opSync, func(name string) {
		assert.Equal(t, SegmentName(1), filepath.Base(name))
		assert.NoError(t, s.Roll(2))
		assert.NoError(t, s.Drop([]uint32{1}))
	})
	require.NoError(t, s.SyncActive())

	frame := testFrame(t, "new", 5)
	appendFrame(t, s, frame, 2)
	requireReadable(t, s, 2, 0, frame)
	require.NoError(t, s.SyncActive(), "the store is not damaged")
}

func TestFault_StoreRoll(t *testing.T) {
	t.Parallel()

	t.Run("sync of the sealing segment fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, dir := newTestStore(t)
		require.NoError(t, s.CreateActive(1))
		ffs.arm(opSync, fault{err: errInjected})

		err := s.Roll(2)
		require.ErrorIs(t, err, errInjected)
		require.ErrorIs(t, err, ErrSegmentDamaged)
		id, _, _ := s.Active()
		assert.Equal(t, uint32(1), id)
		assert.Equal(t, []string{SegmentName(1)}, listDir(t, dir), "no next segment after a failed seal")
		requireDamaged(t, s, ffs)
	})

	t.Run("create of the next segment fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, _ := newTestStore(t)
		require.NoError(t, s.CreateActive(1))
		ffs.arm(opCreate, fault{err: errInjected})

		require.ErrorIs(t, s.Roll(2), errInjected)
		id, _, _ := s.Active()
		assert.Equal(t, uint32(1), id, "the synced segment stays active")
		frame := testFrame(t, "still", 3)
		appendFrame(t, s, frame, 1)
		require.NoError(t, s.Roll(2), "a later roll succeeds")
		requireReadable(t, s, 1, 0, frame)
	})

	t.Run("directory sync fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, _ := newTestStore(t)
		require.NoError(t, s.CreateActive(1))
		ffs.arm(opSyncDir, fault{err: errInjected})

		err := s.Roll(2)
		require.ErrorIs(t, err, errInjected)
		require.ErrorIs(t, err, ErrSegmentDamaged)
		id, _, _ := s.Active()
		assert.Equal(t, uint32(2), id, "the new segment is installed so Close releases it")
		requireDamaged(t, s, ffs)
	})
}

func TestFault_StoreCreateActive(t *testing.T) {
	t.Parallel()

	t.Run("create fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, _ := newTestStore(t)
		ffs.arm(opCreate, fault{err: errInjected})
		require.ErrorIs(t, s.CreateActive(1), errInjected)
		_, _, ok := s.Active()
		assert.False(t, ok)
		require.NoError(t, s.CreateActive(1), "nothing was left behind, so a retry works")
	})

	t.Run("directory sync fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, _ := newTestStore(t)
		ffs.arm(opSyncDir, fault{err: errInjected})
		err := s.CreateActive(1)
		require.ErrorIs(t, err, errInjected)
		require.ErrorIs(t, err, ErrSegmentDamaged)
		requireDamaged(t, s, ffs)
	})

	t.Run("a higher file joins the set during create", func(t *testing.T) {
		t.Parallel()
		s, ffs, dir := newTestStore(t)
		ffs.onNext(opCreate, func(string) {
			f, err := vfs.OSFS{}.Create(segPath(dir, 9))
			if assert.NoError(t, err) && assert.NoError(t, f.Close()) {
				assert.NoError(t, s.Register(FileName{Kind: KindSegment, ID: 9}))
			}
		})
		require.ErrorIs(t, s.CreateActive(1), ErrSegmentOrder)
		assert.Equal(t, []string{SegmentName(9)}, listDir(t, dir), "the refused file is deleted")
		_, _, ok := s.Active()
		assert.False(t, ok)
	})
}

func TestFault_StoreRegister(t *testing.T) {
	t.Parallel()
	// newWithFile returns a store whose directory holds seg_<id>.data with
	// one frame in it.
	newWithFile := func(t *testing.T, id uint32) (*Store, *faultFS, []byte) {
		t.Helper()
		s, ffs, dir := newTestStore(t)
		frame := testFrame(t, "old", 8)
		f, err := vfs.OSFS{}.Create(segPath(dir, id))
		require.NoError(t, err)
		_, err = f.Write(frame)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		return s, ffs, frame
	}

	t.Run("open fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, frame := newWithFile(t, 3)
		ffs.arm(opOpen, fault{err: errInjected})
		require.ErrorIs(t, s.Register(FileName{Kind: KindSegment, ID: 3}), errInjected)
		_, err := s.ReadAt(3, 0, int64(len(frame)))
		require.ErrorIs(t, err, ErrSegmentNotFound)
	})

	t.Run("size fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, frame := newWithFile(t, 3)
		ffs.arm(opSize, fault{err: errInjected})
		mark := len(ffs.ops())
		require.ErrorIs(t, s.Register(FileName{Kind: KindSegment, ID: 3}), errInjected)
		assert.Contains(t, opsSince(ffs, mark), "close "+SegmentName(3), "the handle is released")
		_, err := s.ReadAt(3, 0, int64(len(frame)))
		require.ErrorIs(t, err, ErrSegmentNotFound)
		require.NoError(t, s.Register(FileName{Kind: KindSegment, ID: 3}))
		requireReadable(t, s, 3, 0, frame)
	})

	t.Run("a lower active segment appears during open", func(t *testing.T) {
		t.Parallel()
		s, ffs, frame := newWithFile(t, 3)
		ffs.onNext(opOpen, func(string) { assert.NoError(t, s.CreateActive(1)) })
		mark := len(ffs.ops())
		require.ErrorIs(t, s.Register(FileName{Kind: KindSegment, ID: 3}), ErrSegmentOrder)
		assert.Contains(t, opsSince(ffs, mark), "close "+SegmentName(3))
		_, err := s.ReadAt(3, 0, int64(len(frame)))
		require.ErrorIs(t, err, ErrSegmentNotFound)
	})
}

func TestFault_StoreDrop(t *testing.T) {
	t.Parallel()

	t.Run("remove fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, dir, frames := buildSealed(t, 2)
		ffs.arm(opRemove, fault{err: errInjected})
		mark := len(ffs.ops())
		require.ErrorIs(t, s.Drop([]uint32{1, 2}), errInjected)
		assert.Equal(t, "syncdir "+filepath.Base(dir), opsSince(ffs, mark)[4], "the directory is synced anyway")
		for _, id := range []uint32{1, 2} {
			_, err := s.ReadAt(id, 0, int64(len(frames[id-1])))
			require.ErrorIs(t, err, ErrSegmentNotFound, "file %d left the set", id)
		}
		assert.Equal(t, []string{SegmentName(1), SegmentName(3)}, listDir(t, dir), "the other file is removed")
		appendFrame(t, s, testFrame(t, "ok", 1), 3)
	})

	t.Run("close fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, dir, frames := buildSealed(t, 1)
		ffs.arm(opClose, fault{err: errInjected})
		require.ErrorIs(t, s.Drop([]uint32{1}), errInjected)
		_, err := s.ReadAt(1, 0, int64(len(frames[0])))
		require.ErrorIs(t, err, ErrSegmentNotFound)
		assert.Equal(t, []string{SegmentName(2)}, listDir(t, dir), "the file is still deleted")
	})

	t.Run("directory sync fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, _, _ := buildSealed(t, 1)
		ffs.arm(opSyncDir, fault{err: errInjected})
		err := s.Drop([]uint32{1})
		require.ErrorIs(t, err, errInjected)
		require.ErrorIs(t, err, ErrSegmentDamaged)
		requireDamaged(t, s, ffs)
	})
}

func TestFault_StoreClose(t *testing.T) {
	t.Parallel()

	t.Run("sync of the active segment fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, _, _ := buildSealed(t, 1)
		ffs.arm(opSync, fault{err: errInjected})
		mark := len(ffs.ops())
		err := s.Close()
		require.ErrorIs(t, err, errInjected)
		require.ErrorIs(t, err, ErrSegmentDamaged)
		ops := opsSince(ffs, mark)
		assert.True(t, slices.Contains(ops, "close "+SegmentName(1)) && slices.Contains(ops, "close "+SegmentName(2)),
			"every file is closed anyway: %v", ops)
		require.NoError(t, s.Close())
	})

	t.Run("closing one file fails", func(t *testing.T) {
		t.Parallel()
		s, ffs, _, _ := buildSealed(t, 1)
		ffs.arm(opClose, fault{err: errInjected})
		mark := len(ffs.ops())
		require.ErrorIs(t, s.Close(), errInjected)
		assert.Contains(t, opsSince(ffs, mark), "close "+SegmentName(2), "the remaining files are closed")
		require.NoError(t, s.Close())
	})
}

func TestFault_StoreReadAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		flt     fault
		wantErr []error
		corrupt bool
	}{
		{"read error", fault{err: errInjected}, []error{errInjected}, false},
		{"short read without an error", fault{n: 3}, []error{ErrCorrupt, io.ErrUnexpectedEOF}, true},
		{"short read with EOF", fault{n: 3, err: io.EOF}, []error{ErrCorrupt, io.ErrUnexpectedEOF}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ffs, _ := newTestStore(t)
			require.NoError(t, s.CreateActive(1))
			frame := testFrame(t, "key", 20)
			appendFrame(t, s, frame, 1)

			ffs.arm(opRead, tc.flt)
			got, err := s.ReadAt(1, 0, int64(len(frame)))
			for _, want := range tc.wantErr {
				require.ErrorIs(t, err, want)
			}
			assert.Nil(t, got)
			if !tc.corrupt {
				assert.NotErrorIs(t, err, ErrCorrupt, "an I/O error is not corruption")
			}
			requireReadable(t, s, 1, 0, frame)
			appendFrame(t, s, testFrame(t, "more", 1), 1)
		})
	}
}
