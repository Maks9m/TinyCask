package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
	"github.com/Maks9m/TinyCask/internal/vfs/simfs"
)

func TestReopen_RestoresLatestValuesAcrossSegments(t *testing.T) {
	fsys := simfs.New()
	e := openTest(t, fsys, testOptions())
	h := newHistory()
	const keys = 40 // ~9 KiB of 200-byte values: at least three segments
	for i := range keys {
		h.put(t, e, numKey(i), strings.Repeat("v", 200)+numKey(i))
	}
	for i := 0; i < keys; i += 3 {
		h.put(t, e, numKey(i), "overwritten")
	}
	for i := 1; i < keys; i += 4 {
		h.del(t, e, numKey(i))
	}
	h.put(t, e, numKey(5), "deleted then put again")
	h.put(t, e, "empty", "")
	before := e.Stats()
	require.NoError(t, e.Close())
	require.GreaterOrEqual(t, len(dataFiles(t, fsys, testDir)), 3)

	e = reopen(t, fsys, testDir, testOptions())
	h.check(t, e)
	after := e.Stats()
	assert.Equal(t, before.Keys, after.Keys)
	assert.Equal(t, before.LiveBytes, after.LiveBytes, "liveBytes must survive the restart")
	assert.Equal(t, before.DiskBytes, after.DiskBytes, "the new active segment is empty")
}

func TestReopen_RepeatedRestartsNeverAppendToOldFiles(t *testing.T) {
	fsys := simfs.New()
	h := newHistory()
	for round := range 4 {
		before := snapshot(t, fsys, testDir)
		wantActive := maxDataID(t, fsys, testDir) + 1

		e := reopen(t, fsys, testDir, testOptions())
		h.check(t, e)
		id, size, ok := e.store.Active()
		require.True(t, ok)
		assert.Equal(t, wantActive, id, "round %d: active segment must be max+1", round)
		assert.Zero(t, size)

		for i := range 12 {
			h.put(t, e, fmt.Sprintf("r%d-%02d", round, i), strings.Repeat("x", 300))
		}
		if round > 0 {
			h.del(t, e, fmt.Sprintf("r%d-00", round-1))
			h.put(t, e, fmt.Sprintf("r%d-01", round-1), "updated")
		}
		require.NoError(t, e.Close())

		after := snapshot(t, fsys, testDir)
		for name, content := range before {
			assert.Equal(t, content, after[name], "round %d: %s from an earlier run changed", round, name)
		}
	}
}

func TestReopen_NewActiveSegmentIsMaxPlusOne(t *testing.T) {
	tests := []struct {
		name  string
		files []storage.FileName
		want  uint32
	}{
		{"empty directory", nil, firstSegmentID},
		{"single segment", []storage.FileName{seg(5)}, 6},
		{"gap in IDs", []storage.FileName{seg(2), seg(9)}, 10},
		{"leftover tmp keeps IDs growing", []storage.FileName{seg(3), tmp(7)}, 8},
		{"compaction output only", []storage.FileName{compact(4)}, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := simfs.New()
			require.NoError(t, fsys.MkdirAll(testDir))
			for _, f := range tc.files {
				writeDataFile(t, fsys, testDir, f.String(), putRec(f.String(), "v"))
			}
			e := reopen(t, fsys, testDir, testOptions())
			id, _, ok := e.store.Active()
			require.True(t, ok)
			assert.Equal(t, tc.want, id)
			assert.Zero(t, fileSize(t, fsys, testDir, storage.SegmentName(tc.want)))
		})
	}
}

func TestRecover_FileIDsExhausted(t *testing.T) {
	fsys := simfs.New()
	name := storage.SegmentName(math.MaxUint32)
	writeDataFile(t, fsys, testDir, name)

	_, err := Open(fsys, testDir, testOptions())
	require.Error(t, err)
	requireLockFree(t, fsys, testDir)
	assert.Equal(t, []string{lockFileName, name}, mustList(t, fsys, testDir))
}

func TestRecover_DeletesTmpFiles(t *testing.T) {
	fsys := simfs.New()
	writeDataFile(t, fsys, testDir, seg(1).String(), putRec("a", "1"))
	writeDataFile(t, fsys, testDir, tmp(2).String(), putRec("a", "from unfinished compaction"))
	appendBytes(t, fsys, testDir, tmp(2).String(), []byte("half a record"))
	writeDataFile(t, fsys, testDir, tmp(5).String())

	e := reopen(t, fsys, testDir, testOptions())
	assertValue(t, e, "a", "1")
	assert.Equal(t, []string{lockFileName, seg(1).String(), seg(6).String()}, mustList(t, fsys, testDir))
}

func TestRecover_SupersedeRule(t *testing.T) {
	fsys := simfs.New()
	// Hand-built: two compaction outputs, older and newer segments around them.
	writeDataFile(t, fsys, testDir, seg(1).String(), putRec("a", "a1"), putRec("old", "o1"), putRec("resurrect", "r1"))
	writeDataFile(t, fsys, testDir, compact(2).String(), putRec("a", "a2"), putRec("old", "o2"))
	writeDataFile(t, fsys, testDir, seg(3).String(), putRec("a", "a3"))
	writeDataFile(t, fsys, testDir, seg(4).String(), delRec("resurrect"), putRec("b", "b4"))
	writeDataFile(t, fsys, testDir, compact(5).String(), putRec("a", "a5"), putRec("b", "b4"))
	writeDataFile(t, fsys, testDir, seg(6).String(), putRec("keep", "k6"), delRec("b"))
	writeDataFile(t, fsys, testDir, seg(7).String(), putRec("a", "a7"))

	e := reopen(t, fsys, testDir, testOptions())
	assert.Equal(t, []string{
		lockFileName, compact(5).String(), seg(6).String(), seg(7).String(), seg(8).String(),
	}, mustList(t, fsys, testDir), "every file below compact_5 must be deleted")
	assertValue(t, e, "a", "a7")
	assertValue(t, e, "keep", "k6")
	for _, gone := range []string{"b", "old", "resurrect"} {
		_, err := e.Get([]byte(gone))
		assert.ErrorIs(t, err, ErrKeyNotFound, gone)
	}
	assert.Equal(t, 2, e.Stats().Keys)
}

func TestRecover_TornTailInHighestFileIsTruncated(t *testing.T) {
	for _, d := range damages(t) {
		t.Run(d.name, func(t *testing.T) {
			fsys := simfs.New()
			h, last, goodSize := buildMultiSegment(t, fsys, testDir)
			liveBefore := h.liveBytes()
			appendBytes(t, fsys, testDir, last, d.bytes)

			var logs bytes.Buffer
			opts := testOptions()
			opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			e := reopen(t, fsys, testDir, opts)

			h.check(t, e)
			assert.Equal(t, goodSize, fileSize(t, fsys, testDir, last), "tail must be cut to the last good record")
			synced, err := fsys.SyncedSize(filepath.Join(testDir, last))
			require.NoError(t, err)
			assert.Equal(t, goodSize, synced)
			assert.Equal(t, liveBefore, e.Stats().LiveBytes)
			_, err = e.Get([]byte("torn"))
			require.ErrorIs(t, err, ErrKeyNotFound)
			assert.Contains(t, logs.String(), "torn tail")
			assert.Contains(t, logs.String(), last)
		})
	}
}

func TestRecover_CorruptRecordInsideHighestFileDropsTheRest(t *testing.T) {
	fsys := simfs.New()
	writeDataFile(t, fsys, testDir, seg(1).String(), putRec("a", "old"), putRec("c", "old"))
	writeDataFile(t, fsys, testDir, seg(2).String(),
		putRec("a", "new"), putRec("b", "b"), putRec("c", "new"), putRec("d", "d"))
	badOff := recordSize("a", "new") + recordSize("b", "b")
	flipByte(t, fsys, testDir, seg(2).String(), badOff+recordSize("c", "new")-1)

	e := reopen(t, fsys, testDir, testOptions())
	assert.Equal(t, badOff, fileSize(t, fsys, testDir, seg(2).String()))
	assertValue(t, e, "a", "new")
	assertValue(t, e, "b", "b")
	assertValue(t, e, "c", "old")
	_, err := e.Get([]byte("d"))
	require.ErrorIs(t, err, ErrKeyNotFound, "records after the corrupt one are dropped with it")
}

func TestRecover_CorruptLowerFileFailsOpen(t *testing.T) {
	type corruptCase struct {
		name string
		// build fills testDir and returns the damaged file and bad offset.
		build func(tb testing.TB, fsys vfs.FS) (file string, offset int64)
	}
	threeFiles := func(tb testing.TB, fsys vfs.FS) int64 {
		size := writeDataFile(tb, fsys, testDir, seg(1).String(), putRec("a", "aaaa"), putRec("b", "bbbb"))
		writeDataFile(tb, fsys, testDir, seg(2).String(), putRec("c", "cccc"))
		writeDataFile(tb, fsys, testDir, tmp(3).String(), putRec("x", "unfinished"))
		return size
	}
	var tests []corruptCase
	for _, d := range damages(t) {
		tests = append(tests, corruptCase{"tail of seg_1: " + d.name, func(tb testing.TB, fsys vfs.FS) (string, int64) {
			size := threeFiles(tb, fsys)
			appendBytes(tb, fsys, testDir, seg(1).String(), d.bytes)
			return seg(1).String(), size
		}})
	}
	tests = append(tests,
		corruptCase{"first record of seg_1", func(tb testing.TB, fsys vfs.FS) (string, int64) {
			threeFiles(tb, fsys)
			flipByte(tb, fsys, testDir, seg(1).String(), recordSize("a", "aaaa")-1)
			return seg(1).String(), 0
		}},
		corruptCase{"torn tail behind an empty newer segment", func(tb testing.TB, fsys vfs.FS) (string, int64) {
			size := writeDataFile(tb, fsys, testDir, seg(1).String(), putRec("a", "aaaa"))
			appendBytes(tb, fsys, testDir, seg(1).String(), []byte("torn"))
			writeDataFile(tb, fsys, testDir, seg(2).String())
			return seg(1).String(), size
		}},
	)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := simfs.New()
			file, wantOff := tc.build(t, fsys)
			before := snapshot(t, fsys, testDir)

			_, err := Open(fsys, testDir, testOptions())
			require.ErrorIs(t, err, ErrCorrupt)
			require.ErrorIs(t, err, storage.ErrCorrupt)
			var se *storage.ScanError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, wantOff, se.Offset)
			assert.Contains(t, err.Error(), file, "the error must name the file")
			assert.Contains(t, err.Error(), fmt.Sprintf("offset %d", wantOff), "the error must name the offset")

			requireLockFree(t, fsys, testDir)
			assert.Equal(t, before, snapshot(t, fsys, testDir), "a failed Open must leave the directory untouched")
		})
	}
}

func TestRecover_DuplicateFileIDFailsOpen(t *testing.T) {
	fsys := simfs.New()
	writeDataFile(t, fsys, testDir, seg(3).String(), putRec("a", "1"))
	writeDataFile(t, fsys, testDir, compact(3).String(), putRec("a", "2"))
	before := snapshot(t, fsys, testDir)

	_, err := Open(fsys, testDir, testOptions())
	require.ErrorIs(t, err, ErrCorrupt)
	requireLockFree(t, fsys, testDir)
	assert.Equal(t, before, snapshot(t, fsys, testDir))
}

func TestRecover_FaultsFailOpenHarmlessly(t *testing.T) {
	tests := []struct {
		name string
		op   fsOp
		// untouched: the fault hits while scanning, before any change.
		untouched bool
	}{
		{"open during scan", opOpen, true},
		{"read during scan", opRead, true},
		{"remove tmp", opRemove, false},
		{"sync directory after remove", opSyncDir, false},
		{"truncate torn tail", opTruncate, false},
		{"sync repaired tail", opSync, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys := newFaultFS()
			h, last, goodSize := buildMultiSegment(t, fsys, testDir)
			appendBytes(t, fsys, testDir, last, []byte("torn tail bytes"))
			writeDataFile(t, fsys, testDir, tmp(99).String(), putRec("x", "unfinished"))
			before := snapshot(t, fsys, testDir)

			fsys.arm(tc.op, fault{err: errInjected})
			_, err := Open(fsys, testDir, testOptions())
			require.ErrorIs(t, err, errInjected)
			require.NotErrorIs(t, err, ErrCorrupt, "an I/O error is not corruption")
			requireLockFree(t, fsys, testDir)
			if tc.untouched {
				assert.Equal(t, before, snapshot(t, fsys, testDir))
			}

			// The fault fired once; a clean restart recovers everything.
			e := reopen(t, fsys, testDir, testOptions())
			h.check(t, e)
			assert.Equal(t, goodSize, fileSize(t, fsys, testDir, last))
			assert.NotContains(t, mustList(t, fsys, testDir), tmp(99).String())
		})
	}
}

func TestReopen_OnOSFS(t *testing.T) {
	fsys := vfs.OSFS{}
	dir := filepath.Join(t.TempDir(), "data")
	h, last, goodSize := buildMultiSegment(t, fsys, dir)
	appendBytes(t, fsys, dir, last, damages(t)[2].bytes)
	writeDataFile(t, fsys, dir, tmp(99).String())

	e := reopen(t, fsys, dir, testOptions())
	h.check(t, e)
	assert.Equal(t, goodSize, fileSize(t, fsys, dir, last))
	require.NoError(t, e.Put([]byte("after"), []byte("restart")))
	require.NoError(t, e.Close())

	e = reopen(t, fsys, dir, testOptions())
	h.put(t, e, "after", "restart")
	h.check(t, e)
}

// --- helpers ---

// Header field offsets from ARCHITECTURE §3.1, for building damaged records.
const (
	hdrTypeOff    = 4 + 8 // after CRC32 and Timestamp
	hdrKeySizeOff = hdrTypeOff + 1
	hdrValSizeOff = hdrKeySizeOff + 4
	badRecordType = 9
)

// damage is a cut or corrupt record to append after valid data.
type damage struct {
	name  string
	bytes []byte
}

// damages covers every decode failure ARCHITECTURE §5.4 step 4 lists.
func damages(tb testing.TB) []damage {
	tb.Helper()
	frame := encodeRecs(tb, putRec("torn", "value"))
	edit := func(f func(b []byte)) []byte {
		b := bytes.Clone(frame)
		f(b)
		return b
	}
	return []damage{
		{"short header", frame[:storage.HeaderSize-1]},
		{"short payload", frame[:len(frame)-1]},
		{"checksum mismatch", edit(func(b []byte) { b[len(b)-1] ^= 0xff })},
		{"unknown type", edit(func(b []byte) { b[hdrTypeOff] = badRecordType })},
		{"key size over limit", edit(func(b []byte) {
			binary.LittleEndian.PutUint32(b[hdrKeySizeOff:], storage.DefaultMaxKeySize+1)
		})},
		{"value size over limit", edit(func(b []byte) {
			binary.LittleEndian.PutUint32(b[hdrValSizeOff:], storage.DefaultMaxValueSize+1)
		})},
	}
}

// history drives an engine and remembers what every key must hold.
type history struct {
	live map[string]string
	gone map[string]bool
}

func newHistory() *history {
	return &history{live: make(map[string]string), gone: make(map[string]bool)}
}

func (h *history) put(tb testing.TB, e *Engine, key, value string) {
	tb.Helper()
	require.NoError(tb, e.Put([]byte(key), []byte(value)))
	h.live[key] = value
	delete(h.gone, key)
}

func (h *history) del(tb testing.TB, e *Engine, key string) {
	tb.Helper()
	require.NoError(tb, e.Delete([]byte(key)))
	delete(h.live, key)
	h.gone[key] = true
}

// check asserts e returns every live value and none of the deleted keys.
func (h *history) check(tb testing.TB, e *Engine) {
	tb.Helper()
	for k, v := range h.live {
		assertValue(tb, e, k, v)
	}
	for k := range h.gone {
		_, err := e.Get([]byte(k))
		assert.ErrorIs(tb, err, ErrKeyNotFound, k)
	}
	assert.Equal(tb, len(h.live), e.Stats().Keys)
}

// reopen opens an engine on dir, closes it at test end and checks invariants.
func reopen(tb testing.TB, fsys vfs.FS, dir string, opts Options) *Engine {
	tb.Helper()
	e, err := Open(fsys, dir, opts)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, e.Close()) })
	checkInvariants(tb, fsys, dir, e)
	return e
}

// buildMultiSegment writes keys across several segments through the engine,
// closes it, and returns the history, the highest file and its size.
func buildMultiSegment(tb testing.TB, fsys vfs.FS, dir string) (h *history, last string, size int64) {
	tb.Helper()
	e, err := Open(fsys, dir, testOptions())
	require.NoError(tb, err)
	h = newHistory()
	for i := range 30 {
		h.put(tb, e, numKey(i), strings.Repeat("x", 200))
	}
	h.put(tb, e, numKey(0), "overwritten")
	h.del(tb, e, numKey(1))
	require.NoError(tb, e.Close())

	files := dataFiles(tb, fsys, dir)
	require.GreaterOrEqual(tb, len(files), 2)
	last = files[len(files)-1]
	return h, last, fileSize(tb, fsys, dir, last)
}

// liveBytes is the record size of every live key: what liveBytes must be.
func (h *history) liveBytes() int64 {
	var total int64
	for k, v := range h.live {
		total += recordSize(k, v)
	}
	return total
}

func numKey(i int) string { return fmt.Sprintf("key-%02d", i) }

func seg(id uint32) storage.FileName { return storage.FileName{Kind: storage.KindSegment, ID: id} }

func compact(id uint32) storage.FileName { return storage.FileName{Kind: storage.KindCompact, ID: id} }

func tmp(id uint32) storage.FileName { return storage.FileName{Kind: storage.KindCompactTmp, ID: id} }

func putRec(key, value string) storage.Record {
	return storage.Record{
		Timestamp: uint64(testClock().UnixNano()), Type: storage.TypePut, Key: []byte(key), Value: []byte(value),
	}
}

func delRec(key string) storage.Record {
	return storage.Record{Timestamp: uint64(testClock().UnixNano()), Type: storage.TypeDelete, Key: []byte(key)}
}

func encodeRecs(tb testing.TB, recs ...storage.Record) []byte {
	tb.Helper()
	var out []byte
	for _, r := range recs {
		frame, err := storage.Encode(r, storage.DefaultLimits())
		require.NoError(tb, err)
		out = append(out, frame...)
	}
	return out
}

// writeDataFile creates dir/base holding recs, synced, and returns its size.
func writeDataFile(tb testing.TB, fsys vfs.FS, dir, base string, recs ...storage.Record) int64 {
	tb.Helper()
	require.NoError(tb, fsys.MkdirAll(dir))
	f, err := fsys.Create(filepath.Join(dir, base))
	require.NoError(tb, err)
	data := encodeRecs(tb, recs...)
	if len(data) > 0 {
		_, err = f.Write(data)
		require.NoError(tb, err)
	}
	require.NoError(tb, f.Sync())
	require.NoError(tb, f.Close())
	return int64(len(data))
}

// appendBytes appends raw bytes to an existing file.
func appendBytes(tb testing.TB, fsys vfs.FS, dir, base string, data []byte) {
	tb.Helper()
	f, err := fsys.Open(filepath.Join(dir, base))
	require.NoError(tb, err)
	_, err = f.Write(data)
	require.NoError(tb, err)
	require.NoError(tb, f.Close())
}

// flipByte inverts one byte in place. Write always appends, so the file is
// read, cut to zero and rewritten.
func flipByte(tb testing.TB, fsys vfs.FS, dir, base string, off int64) {
	tb.Helper()
	data := []byte(snapshot(tb, fsys, dir)[base])
	require.Less(tb, off, int64(len(data)))
	data[off] ^= 0xff
	f, err := fsys.Open(filepath.Join(dir, base))
	require.NoError(tb, err)
	require.NoError(tb, f.Truncate(0))
	_, err = f.Write(data)
	require.NoError(tb, err)
	require.NoError(tb, f.Close())
}

// snapshot returns every file in dir but LOCK with its contents ({} if dir
// is missing).
func snapshot(tb testing.TB, fsys vfs.FS, dir string) map[string]string {
	tb.Helper()
	out := make(map[string]string)
	names, err := fsys.List(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out
	}
	require.NoError(tb, err)
	for _, base := range names {
		if base == lockFileName {
			continue // created by Lock itself, before recovery reads anything
		}
		f, err := fsys.Open(filepath.Join(dir, base))
		require.NoError(tb, err)
		size, err := f.Size()
		require.NoError(tb, err)
		buf := make([]byte, size)
		if size > 0 {
			_, err = f.ReadAt(buf, 0)
			require.NoError(tb, err)
		}
		require.NoError(tb, f.Close())
		out[base] = string(buf)
	}
	return out
}

// maxDataID returns the highest data-file ID in dir, or 0 if there is none.
func maxDataID(tb testing.TB, fsys vfs.FS, dir string) uint32 {
	tb.Helper()
	var maxID uint32
	for base := range snapshot(tb, fsys, dir) {
		name, ok, err := storage.ParseFileName(base)
		require.NoError(tb, err)
		if ok {
			maxID = max(maxID, name.ID)
		}
	}
	return maxID
}
