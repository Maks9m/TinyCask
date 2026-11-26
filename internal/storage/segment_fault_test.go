package storage

import (
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// errInjectedClose is a second fault, to check both errors reach the chain.
var errInjectedClose = errors.New("injected close fault")

// writeSealedFile writes seg_<id>.data directly, as an earlier run would.
func writeSealedFile(tb testing.TB, dir string, id uint32, frame []byte) {
	tb.Helper()
	f, err := vfs.OSFS{}.Create(segPath(dir, id))
	require.NoError(tb, err)
	_, err = f.Write(frame)
	require.NoError(tb, err)
	require.NoError(tb, f.Close())
}

// TestFault_StoreAppend_TornTailNeverVisible: a torn prefix on disk is never
// exposed by ReadAt, Active or DiskBytes, before or after truncate, even if
// the truncate fails. ErrNoSpace stays in the chain next to ErrSegmentDamaged.
func TestFault_StoreAppend_TornTailNeverVisible(t *testing.T) {
	t.Parallel()
	const torn = 7
	tests := []struct {
		name     string
		truncate *fault
	}{
		{"truncate succeeds", nil},
		{"truncate fails", &fault{err: errInjected}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ffs, dir := newTestStore(t)
			require.NoError(t, s.CreateActive(1))
			good := testFrame(t, "good", 20)
			appendFrame(t, s, good, 1)
			end := int64(len(good))

			requireHidden := func(tb testing.TB) {
				tb.Helper()
				_, size, _ := s.Active()
				assert.Equal(tb, end, size)
				assert.Equal(tb, end, s.DiskBytes())
				got, err := s.ReadAt(1, end, torn)
				require.ErrorIs(tb, err, ErrCorrupt)
				require.ErrorIs(tb, err, io.ErrUnexpectedEOF)
				assert.Nil(tb, got)
				requireReadable(tb, s, 1, 0, good)
			}

			ffs.arm(opWrite, fault{n: torn, err: vfs.ErrNoSpace})
			if tc.truncate != nil {
				ffs.arm(opTruncate, *tc.truncate)
			}
			truncated := false
			ffs.onNext(opTruncate, func(string) {
				truncated = true
				require.Equal(t, end+torn, diskSize(t, segPath(dir, 1)), "the torn prefix reached the disk")
				requireHidden(t)
			})

			_, _, err := s.Append(testFrame(t, "lost", 30))
			require.True(t, truncated, "a failed append is truncated away")
			require.ErrorIs(t, err, vfs.ErrNoSpace, "out-of-space stays in the chain")
			if tc.truncate == nil {
				assert.NotErrorIs(t, err, ErrSegmentDamaged)
				assert.Equal(t, end, diskSize(t, segPath(dir, 1)))
				requireHidden(t)
				return
			}
			require.ErrorIs(t, err, errInjected)
			require.ErrorIs(t, err, ErrSegmentDamaged)
			assert.Equal(t, end+torn, diskSize(t, segPath(dir, 1)), "the torn prefix stays until recovery")
			requireHidden(t)
			requireDamaged(t, s, ffs)
		})
	}
}

// TestFault_StoreReadAt_FullReadWithEOF: a full read with io.EOF is served,
// not reported as corruption.
func TestFault_StoreReadAt_FullReadWithEOF(t *testing.T) {
	t.Parallel()
	s, ffs, _ := newTestStore(t)
	require.NoError(t, s.CreateActive(1))
	frame := testFrame(t, "key", 20)
	appendFrame(t, s, frame, 1)

	ffs.arm(opRead, fault{n: len(frame), err: io.EOF})
	got, err := s.ReadAt(1, 0, int64(len(frame)))
	require.NoError(t, err)
	assert.Equal(t, frame, got)
}

// TestFault_StoreCreateActive_DiscardCloseFails: if install refuses the file
// and its Close fails, it is still deleted (so a retry works) and both errors
// are returned.
func TestFault_StoreCreateActive_DiscardCloseFails(t *testing.T) {
	t.Parallel()
	s, ffs, dir := newTestStore(t)
	ffs.onNext(opCreate, func(string) {
		f, err := vfs.OSFS{}.Create(segPath(dir, 9))
		if assert.NoError(t, err) && assert.NoError(t, f.Close()) {
			assert.NoError(t, s.Register(FileName{Kind: KindSegment, ID: 9}))
		}
	})
	ffs.arm(opClose, fault{err: errInjectedClose})

	mark := len(ffs.ops())
	err := s.CreateActive(1)
	require.ErrorIs(t, err, ErrSegmentOrder)
	require.ErrorIs(t, err, errInjectedClose)
	assert.Contains(t, opsSince(ffs, mark), "remove "+SegmentName(1), "the refused file is removed anyway")
	assert.Equal(t, []string{SegmentName(9)}, listDir(t, dir))
	_, _, ok := s.Active()
	assert.False(t, ok)
	assert.NotErrorIs(t, s.SyncActive(), ErrSegmentDamaged, "a refused create does not damage the store")
}

// TestFault_StoreRegister_CloseFailsOnErrorPath: both errors are returned and
// the file stays out of the set.
func TestFault_StoreRegister_CloseFailsOnErrorPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		arm     func(t *testing.T, s *Store, ffs *faultFS)
		wantErr error
		// retry says whether a second Register of the same file must work.
		retry bool
	}{
		{
			name:    "size fails",
			arm:     func(_ *testing.T, _ *Store, ffs *faultFS) { ffs.arm(opSize, fault{err: errInjected}) },
			wantErr: errInjected,
			retry:   true,
		},
		{
			name: "a lower active segment appears during open",
			arm: func(t *testing.T, s *Store, ffs *faultFS) {
				ffs.onNext(opOpen, func(string) { assert.NoError(t, s.CreateActive(1)) })
			},
			wantErr: ErrSegmentOrder,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, ffs, dir := newTestStore(t)
			frame := testFrame(t, "old", 8)
			writeSealedFile(t, dir, 3, frame)
			tc.arm(t, s, ffs)
			ffs.arm(opClose, fault{err: errInjectedClose})

			err := s.Register(FileName{Kind: KindSegment, ID: 3})
			require.ErrorIs(t, err, tc.wantErr)
			require.ErrorIs(t, err, errInjectedClose, "the failed close is reported, not dropped")
			_, err = s.ReadAt(3, 0, int64(len(frame)))
			require.ErrorIs(t, err, ErrSegmentNotFound)
			assert.Equal(t, int64(0), s.DiskBytes()-activeSize(s))
			if tc.retry {
				require.NoError(t, s.Register(FileName{Kind: KindSegment, ID: 3}))
				requireReadable(t, s, 3, 0, frame)
			}
		})
	}
}

// activeSize returns the active segment's size, or 0 if there is none.
func activeSize(s *Store) int64 {
	_, size, _ := s.Active()
	return size
}
