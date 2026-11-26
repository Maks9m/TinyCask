package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// scanned is one record a Scanner yielded, with Key and Value copied out of
// the scanner's buffer.
type scanned struct {
	rec    Record
	recOff int64
	valOff int64
}

// scanAll drains sc and returns what it yielded and how it ended.
func scanAll(tb testing.TB, sc *Scanner) ([]scanned, error) {
	tb.Helper()
	var out []scanned
	for sc.Next() {
		r := sc.Record()
		r.Key = bytes.Clone(r.Key)
		r.Value = bytes.Clone(r.Value)
		out = append(out, scanned{rec: r, recOff: sc.RecordOffset(), valOff: sc.ValueOffset()})
	}
	return out, sc.Err()
}

// encodeAll encodes recs back to back, as a segment holds them, and returns
// the file bytes and the offset of each record.
func encodeAll(tb testing.TB, lim Limits, recs ...Record) ([]byte, []int64) {
	tb.Helper()
	var file []byte
	offs := make([]int64, 0, len(recs))
	for _, r := range recs {
		frame, err := Encode(r, lim)
		require.NoError(tb, err)
		offs = append(offs, int64(len(file)))
		file = append(file, frame...)
	}
	return file, offs
}

// scanBytes scans an in-memory file.
func scanBytes(tb testing.TB, file []byte, opts ScanOptions) ([]scanned, *Scanner, error) {
	tb.Helper()
	sc := NewScanner(bytes.NewReader(file), int64(len(file)), opts)
	got, err := scanAll(tb, sc)
	return got, sc, err
}

// requireScanStop asserts that err is a *ScanError at offset wrapping the
// corruption rule.
func requireScanStop(tb testing.TB, err error, offset int64, rule violation) {
	tb.Helper()
	var se *ScanError
	require.ErrorAs(tb, err, &se)
	assert.Equal(tb, offset, se.Offset, "error: %v", err)
	requireRule(tb, err, ErrCorrupt, rule)
}

// sampleRecords is a short mixed log: PUT, empty-value PUT, overwrite, DELETE.
func sampleRecords() []Record {
	return []Record{
		{Timestamp: 1, Type: TypePut, Key: []byte("alpha"), Value: []byte("one")},
		{Timestamp: 2, Type: TypePut, Key: []byte("b"), Value: nil},
		{Timestamp: 3, Type: TypePut, Key: []byte("alpha"), Value: bytes.Repeat([]byte("x"), 64)},
		{Timestamp: 4, Type: TypeDelete, Key: []byte("b")},
	}
}

// requireRecords checks got against want and the offsets they were encoded at.
func requireRecords(tb testing.TB, want []Record, offs []int64, got []scanned) {
	tb.Helper()
	require.Len(tb, got, len(want))
	for i, w := range want {
		g := got[i]
		assert.Equal(tb, w.Timestamp, g.rec.Timestamp, "record %d", i)
		assert.Equal(tb, w.Type, g.rec.Type, "record %d", i)
		assert.Equal(tb, w.Key, g.rec.Key, "record %d", i)
		assert.Equal(tb, len(w.Value), len(g.rec.Value), "record %d", i)
		if len(w.Value) > 0 {
			assert.Equal(tb, w.Value, g.rec.Value, "record %d", i)
		}
		assert.Equal(tb, offs[i], g.recOff, "record %d", i)
		assert.Equal(tb, ValueOffset(offs[i], len(w.Key)), g.valOff, "record %d", i)
		assert.Equal(tb, offs[i], RecordOffset(g.valOff, len(g.rec.Key)), "record %d", i)
	}
}

func TestScanner_EmptyFile(t *testing.T) {
	t.Parallel()
	got, sc, err := scanBytes(t, nil, ScanOptions{})
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, int64(0), sc.Offset())
	assert.False(t, sc.Next(), "Next after EOF")
}

func TestScanner_OneRecord(t *testing.T) {
	t.Parallel()
	file, offs := encodeAll(t, testLimits, samplePut)
	got, sc, err := scanBytes(t, file, ScanOptions{Limits: testLimits})
	require.NoError(t, err)
	requireRecords(t, []Record{samplePut}, offs, got)
	assert.Equal(t, int64(HeaderSize+len(samplePut.Key)), got[0].valOff)
	assert.Equal(t, int64(len(file)), sc.Offset())
}

func TestScanner_ManyRecords(t *testing.T) {
	t.Parallel()
	recs := sampleRecords()
	file, offs := encodeAll(t, testLimits, recs...)
	// Chunk sizes below, at and above a header and a record force every way a
	// record can straddle the buffered window.
	for _, chunk := range []int{1, HeaderSize - 1, HeaderSize, 30, int(testLimits.MaxRecordSize()), 0} {
		t.Run(fmt.Sprintf("chunk=%d", chunk), func(t *testing.T) {
			t.Parallel()
			got, sc, err := scanBytes(t, file, ScanOptions{Limits: testLimits, ChunkSize: chunk})
			require.NoError(t, err)
			requireRecords(t, recs, offs, got)
			assert.Equal(t, int64(len(file)), sc.Offset())
		})
	}
}

// TestScanner_Corruption covers every way a scan can stop early. Each case
// starts from two valid records, so the error must carry the offset of the
// first bad byte (the end of the good prefix), not 0 or the end of the file.
func TestScanner_Corruption(t *testing.T) {
	t.Parallel()
	good, _ := encodeAll(t, testLimits, sampleRecords()[:2]...)
	bad := int64(len(good))
	third, err := Encode(samplePut, testLimits)
	require.NoError(t, err)

	cases := []struct {
		name string
		tail []byte
		rule violation
	}{
		{"header cut short", third[:HeaderSize-1], violShortHeader},
		{"header cut to one byte", third[:1], violShortHeader},
		{"payload cut short", third[:len(third)-1], violShortPayload},
		{"header only", third[:HeaderSize], violShortPayload},
		{"oversized key length", rawFrame(byte(TypePut), testLimits.MaxKeySize+1, 0, nil, true), violKeyTooLarge},
		{"oversized value length", rawFrame(byte(TypePut), 1, math.MaxUint32, []byte("k"), true), violValueTooLarge},
		{"empty key", rawFrame(byte(TypePut), 0, 0, nil, true), violEmptyKey},
		{"unknown type", rawFrame(9, 1, 0, []byte("k"), true), violUnknownType},
		{"zero type", rawFrame(0, 1, 0, []byte("k"), true), violUnknownType},
		{"tombstone with value", rawFrame(byte(TypeDelete), 1, 1, []byte("kv"), true), violTombstoneValue},
		{"length points past end of file", rawFrame(byte(TypePut), 1, 40, []byte("k"), true), violShortPayload},
		{"zeroed tail", make([]byte, HeaderSize), violUnknownType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file := append(bytes.Clone(good), tc.tail...)
			got, sc, err := scanBytes(t, file, ScanOptions{Limits: testLimits})
			assert.Len(t, got, 2, "records before the bad one are still yielded")
			requireScanStop(t, err, bad, tc.rule)
			assert.Equal(t, bad, sc.Offset())
			assert.False(t, sc.Next(), "Next after a failure")
			assert.Equal(t, Record{}, sc.Record(), "no record after a failure")
		})
	}
}

// TestScanner_CRCMismatchMidFile flips one byte inside a record that is
// followed by valid ones: the scan stops at the damaged record and yields
// nothing after it, because nothing after a bad record can be trusted.
func TestScanner_CRCMismatchMidFile(t *testing.T) {
	t.Parallel()
	recs := sampleRecords()
	file, offs := encodeAll(t, testLimits, recs...)
	// Last byte of record 2's value: inside the payload, so only the CRC can
	// catch it.
	file[offs[3]-1] ^= 0x01

	got, sc, err := scanBytes(t, file, ScanOptions{Limits: testLimits})
	requireRecords(t, recs[:2], offs[:2], got)
	requireScanStop(t, err, offs[2], violChecksum)
	assert.Equal(t, offs[2], sc.Offset())
}

// countingReader records the longest ReadAt it was asked for.
type countingReader struct {
	r io.ReaderAt

	// mu protects the fields below. Scanners are single-goroutine; the lock
	// only keeps the race detector honest if a test changes.
	mu      sync.Mutex
	maxRead int
	reads   int
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	c.maxRead = max(c.maxRead, len(p))
	c.reads++
	c.mu.Unlock()
	return c.r.ReadAt(p, off)
}

// TestScanner_OversizedLengthDoesNotAllocate checks invariant 10: a header
// that claims a 4 GiB value is rejected from the header alone, without a
// read or a buffer sized from the claim.
func TestScanner_OversizedLengthDoesNotAllocate(t *testing.T) {
	t.Parallel()
	file := rawFrame(byte(TypePut), 1, math.MaxUint32, []byte("k"), true)
	file = append(file, make([]byte, 100)...)
	cr := &countingReader{r: bytes.NewReader(file)}
	const chunk = 32
	sc := NewScanner(cr, int64(len(file)), ScanOptions{ChunkSize: chunk})

	require.False(t, sc.Next())
	requireScanStop(t, sc.Err(), 0, violValueTooLarge)
	assert.LessOrEqual(t, cr.maxRead, chunk)
	assert.LessOrEqual(t, cap(sc.buf), chunk)
}

// TestScanner_ReadsInBoundedChunks checks that a file much larger than the
// chunk is never read, or buffered, whole.
func TestScanner_ReadsInBoundedChunks(t *testing.T) {
	t.Parallel()
	var recs []Record
	for i := range 500 {
		recs = append(recs, Record{Timestamp: uint64(i), Type: TypePut,
			Key: fmt.Appendf(nil, "key-%03d", i), Value: bytes.Repeat([]byte{byte(i)}, i%65)})
	}
	file, offs := encodeAll(t, testLimits, recs...)
	const chunk = 256
	require.Greater(t, len(file), 20*chunk)

	cr := &countingReader{r: bytes.NewReader(file)}
	sc := NewScanner(cr, int64(len(file)), ScanOptions{Limits: testLimits, ChunkSize: chunk})
	got, err := scanAll(t, sc)
	require.NoError(t, err)
	requireRecords(t, recs, offs, got)

	assert.LessOrEqual(t, cr.maxRead, chunk)
	assert.LessOrEqual(t, cap(sc.buf), chunk)
	// Chunking must actually batch records, not fall back to a read each.
	assert.Less(t, cr.reads, len(recs))
}

// TestScanner_RecordLargerThanChunk checks that a record over the chunk size
// is read with one ReadAt of exactly its own length.
func TestScanner_RecordLargerThanChunk(t *testing.T) {
	t.Parallel()
	big := Record{Type: TypePut, Key: []byte("big"), Value: bytes.Repeat([]byte("v"), int(DefaultMaxValueSize))}
	recs := []Record{samplePut, big, sampleDel}
	file, offs := encodeAll(t, DefaultLimits(), recs...)

	cr := &countingReader{r: bytes.NewReader(file)}
	sc := NewScanner(cr, int64(len(file)), ScanOptions{ChunkSize: 1024})
	got, err := scanAll(t, sc)
	require.NoError(t, err)
	requireRecords(t, recs, offs, got)

	bigSize := int(RecordSize(len(big.Key), uint32(len(big.Value))))
	assert.Equal(t, bigSize, cr.maxRead)
	assert.Equal(t, bigSize, cap(sc.buf))
}

// TestScanner_RecordAliasesBuffer documents that Record is only valid until
// the next call of Next, and that its slices cannot overwrite each other.
func TestScanner_RecordAliasesBuffer(t *testing.T) {
	t.Parallel()
	file, _ := encodeAll(t, testLimits, samplePut)
	sc := NewScanner(bytes.NewReader(file), int64(len(file)), ScanOptions{Limits: testLimits})
	require.True(t, sc.Next())
	r := sc.Record()
	assert.Equal(t, len(r.Key), cap(r.Key), "key capped so appending cannot clobber the value")
	assert.Equal(t, len(r.Value), cap(r.Value))
}

// TestScanner_SizeBoundsTheScan checks that the scanner stops at the size it
// was given, even if the reader holds more bytes.
func TestScanner_SizeBoundsTheScan(t *testing.T) {
	t.Parallel()
	recs := sampleRecords()
	file, offs := encodeAll(t, testLimits, recs...)
	sc := NewScanner(bytes.NewReader(file), offs[2], ScanOptions{Limits: testLimits})
	got, err := scanAll(t, sc)
	require.NoError(t, err)
	requireRecords(t, recs[:2], offs[:2], got)
}

// TestScanner_FileShorterThanSize covers a reader that ends before the size
// the caller passed (the file shrank under it): the cut record is corrupt.
func TestScanner_FileShorterThanSize(t *testing.T) {
	t.Parallel()
	recs := sampleRecords()
	file, offs := encodeAll(t, testLimits, recs...)
	// Record 2 has a 64-byte value; cut just past its key.
	cut := file[:ValueOffset(offs[2], len(recs[2].Key))+1]
	sc := NewScanner(bytes.NewReader(cut), int64(len(file)), ScanOptions{Limits: testLimits})
	got, err := scanAll(t, sc)
	requireRecords(t, recs[:2], offs[:2], got)
	requireScanStop(t, err, offs[2], violShortPayload)

	// Cut inside the header of record 2 this time.
	sc = NewScanner(bytes.NewReader(file[:offs[2]+3]), int64(len(file)), ScanOptions{Limits: testLimits, ChunkSize: 1})
	got, err = scanAll(t, sc)
	requireRecords(t, recs[:2], offs[:2], got)
	requireScanStop(t, err, offs[2], violShortHeader)
}

func TestScanner_NegativeSize(t *testing.T) {
	t.Parallel()
	sc := NewScanner(bytes.NewReader(nil), -1, ScanOptions{})
	require.False(t, sc.Next())
	var se *ScanError
	require.ErrorAs(t, sc.Err(), &se)
	assert.Equal(t, int64(0), se.Offset)
	assert.NotErrorIs(t, sc.Err(), ErrCorrupt)
}

// eofReader returns io.EOF together with a full read whenever the read
// reaches the end of the data, which io.ReaderAt allows.
type eofReader struct{ data []byte }

func (e eofReader) ReadAt(p []byte, off int64) (int, error) {
	n := copy(p, e.data[off:])
	if off+int64(n) == int64(len(e.data)) {
		return n, io.EOF
	}
	return n, nil
}

// shortReader breaks the io.ReaderAt contract: it returns at most limit bytes
// and no error.
type shortReader struct {
	data  []byte
	limit int
}

func (s shortReader) ReadAt(p []byte, off int64) (int, error) {
	return copy(p[:min(len(p), s.limit)], s.data[off:]), nil
}

// TestScanner_ShortReadWithoutErrorIsNotCorruption keeps a misbehaving reader
// from making recovery truncate good data.
func TestScanner_ShortReadWithoutErrorIsNotCorruption(t *testing.T) {
	t.Parallel()
	file, _ := encodeAll(t, testLimits, sampleRecords()...)
	sc := NewScanner(shortReader{data: file, limit: 10}, int64(len(file)), ScanOptions{Limits: testLimits})
	require.False(t, sc.Next())
	var se *ScanError
	require.ErrorAs(t, sc.Err(), &se)
	assert.Equal(t, int64(0), se.Offset)
	require.ErrorIs(t, sc.Err(), io.ErrUnexpectedEOF)
	assert.NotErrorIs(t, sc.Err(), ErrCorrupt)
}

func TestScanner_AcceptsEOFWithFullRead(t *testing.T) {
	t.Parallel()
	recs := sampleRecords()
	file, offs := encodeAll(t, testLimits, recs...)
	sc := NewScanner(eofReader{data: file}, int64(len(file)), ScanOptions{Limits: testLimits, ChunkSize: 16})
	got, err := scanAll(t, sc)
	require.NoError(t, err)
	requireRecords(t, recs, offs, got)
}

// TestScanner_RandomLog scans seeded random logs with random chunk sizes and,
// half the time, a random cut, and checks the result against the encoder.
func TestScanner_RandomLog(t *testing.T) {
	t.Parallel()
	rng := testRand(t)
	for iter := range 200 {
		n := rng.IntN(40)
		recs := make([]Record, n)
		for i := range recs {
			key := make([]byte, 1+rng.IntN(int(testLimits.MaxKeySize)))
			for j := range key {
				key[j] = byte(rng.Uint32())
			}
			recs[i] = Record{Timestamp: rng.Uint64(), Type: TypeDelete, Key: key}
			if rng.IntN(4) != 0 {
				value := make([]byte, rng.IntN(int(testLimits.MaxValueSize)+1))
				for j := range value {
					value[j] = byte(rng.Uint32())
				}
				recs[i].Type, recs[i].Value = TypePut, value
			}
		}
		file, offs := encodeAll(t, testLimits, recs...)
		size := int64(len(file))
		if rng.IntN(2) == 0 {
			size = rng.Int64N(size + 1)
		}
		chunk := 1 + rng.IntN(200)

		sc := NewScanner(bytes.NewReader(file), size, ScanOptions{Limits: testLimits, ChunkSize: chunk})
		got, err := scanAll(t, sc)

		// The expected result: every record that fits wholly below size.
		whole := 0
		for whole < n && offs[whole]+RecordSize(len(recs[whole].Key), uint32(len(recs[whole].Value))) <= size {
			whole++
		}
		msg := fmt.Sprintf("iteration %d (n=%d size=%d chunk=%d)", iter, n, size, chunk)
		requireRecords(t, recs[:whole], offs[:whole], got)
		if whole == n || offs[whole] == size {
			require.NoError(t, err, msg)
			assert.Equal(t, size, sc.Offset(), msg)
			continue
		}
		var se *ScanError
		require.ErrorAs(t, err, &se, msg)
		assert.Equal(t, offs[whole], se.Offset, msg)
		require.ErrorIs(t, err, ErrCorrupt, msg)
	}
}

// TestScanner_StoreSegmentOnDisk is the week-1 end-to-end check: records
// appended through a Store to real files are scanned back from disk at the
// offsets Append returned, across a roll.
func TestScanner_StoreSegmentOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(vfs.OSFS{}, dir, StoreOptions{Limits: testLimits})
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.CreateActive(1))

	recs := sampleRecords()
	offsByID := map[uint32][]int64{}
	for i, r := range append(recs, recs...) {
		if i == len(recs) {
			require.NoError(t, s.Roll(2))
		}
		frame, err := Encode(r, testLimits)
		require.NoError(t, err)
		id, off, err := s.Append(frame)
		require.NoError(t, err)
		offsByID[id] = append(offsByID[id], off)
	}
	require.NoError(t, s.SyncActive())

	for _, id := range []uint32{1, 2} {
		f, err := vfs.OSFS{}.Open(segPath(dir, id))
		require.NoError(t, err)
		size, err := f.Size()
		require.NoError(t, err)

		got, err := scanAll(t, NewScanner(f, size, ScanOptions{Limits: testLimits, ChunkSize: 40}))
		require.NoError(t, err)
		require.NoError(t, f.Close())
		requireRecords(t, recs, offsByID[id], got)

		// The value offset the scanner reports is where Store.ReadAt finds
		// the value: what KeyDir will store after recovery. Store.ReadAt
		// refuses empty reads, so records without a value are skipped.
		for _, g := range got {
			if len(g.rec.Value) == 0 {
				continue
			}
			v, err := s.ReadAt(id, g.valOff, int64(len(g.rec.Value)))
			require.NoError(t, err)
			assert.Equal(t, g.rec.Value, v)
		}
	}
}

// TestScanner_ErrorMessageNamesOffset makes the stop offset visible in logs.
func TestScanner_ErrorMessageNamesOffset(t *testing.T) {
	t.Parallel()
	file, offs := encodeAll(t, testLimits, sampleRecords()...)
	binary.LittleEndian.PutUint32(file[offs[1]:], 0) // break record 1's CRC
	_, _, err := scanBytes(t, file, ScanOptions{Limits: testLimits})
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("offset %d", offs[1]))
}
