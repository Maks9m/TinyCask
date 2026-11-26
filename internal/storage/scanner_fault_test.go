package storage

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSegment appends recs to seg_1.data through a Store on ffs, syncs it,
// and returns the record offsets and the file size.
func writeSegment(tb testing.TB, ffs *faultFS, dir string, recs []Record) ([]int64, int64) {
	tb.Helper()
	s := NewStore(ffs, dir, StoreOptions{Limits: testLimits})
	require.NoError(tb, s.CreateActive(1))
	var offs []int64
	for _, r := range recs {
		frame, err := Encode(r, testLimits)
		require.NoError(tb, err)
		offs = append(offs, appendFrame(tb, s, frame, 1))
	}
	_, size, ok := s.Active()
	require.True(tb, ok)
	require.NoError(tb, s.Close())
	return offs, size
}

// TestFault_Scanner_ReadError checks that an I/O error is reported as such,
// at the offset of the record being read, and never as corruption: recovery
// must fail Open on it rather than truncate good data away.
func TestFault_Scanner_ReadError(t *testing.T) {
	t.Parallel()
	recs := sampleRecords()
	// partial is how many bytes the failing read delivers before its error.
	for _, partial := range []int{0, 5, HeaderSize} {
		t.Run(fmt.Sprintf("partial=%d", partial), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ffs := newFaultFS()
			offs, size := writeSegment(t, ffs, dir, recs)

			f, err := ffs.Open(segPath(dir, 1))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, f.Close()) })

			// The armed read is the first one: record 0's header. Even a read
			// that delivered the whole header before failing must not be
			// trusted, because its error was not io.EOF.
			sc := NewScanner(f, size, ScanOptions{Limits: testLimits, ChunkSize: HeaderSize})
			ffs.arm(opRead, fault{err: errInjected, n: partial})
			got, err := scanAll(t, sc)

			assert.Empty(t, got)
			var se *ScanError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, offs[0], se.Offset)
			require.ErrorIs(t, err, errInjected)
			assert.NotErrorIs(t, err, ErrCorrupt, "an I/O error is not corruption")
			assert.False(t, sc.Next(), "Next after a failure")
		})
	}
}

// TestFault_Scanner_ReadErrorMidFile fails a read after some records were
// already yielded: the stop offset is the end of the last yielded record.
func TestFault_Scanner_ReadErrorMidFile(t *testing.T) {
	t.Parallel()
	recs := sampleRecords()
	dir := t.TempDir()
	ffs := newFaultFS()
	offs, size := writeSegment(t, ffs, dir, recs)

	f, err := ffs.Open(segPath(dir, 1))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })

	// With one-byte chunks every header and every frame is its own read.
	sc := NewScanner(f, size, ScanOptions{Limits: testLimits, ChunkSize: 1})
	require.True(t, sc.Next())
	require.True(t, sc.Next())
	ffs.arm(opRead, fault{err: errInjected})
	require.False(t, sc.Next())

	var se *ScanError
	require.ErrorAs(t, sc.Err(), &se)
	assert.Equal(t, offs[2], se.Offset)
	assert.Equal(t, offs[2], sc.Offset())
	require.ErrorIs(t, sc.Err(), errInjected)
	assert.NotErrorIs(t, sc.Err(), ErrCorrupt)
}
