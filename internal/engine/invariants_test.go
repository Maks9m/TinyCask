package engine

import (
	"bytes"
	"cmp"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
)

// logRecord is where the checker found a key's last record.
type logRecord struct {
	typ         storage.RecordType
	fileID      uint32
	valueOffset int64
	valueSize   uint32
}

// checkInvariants re-reads dir independently and checks e against it:
//
//   - no *.tmp or superseded file remains;
//   - every data file scans cleanly (torn tail already cut);
//   - KeyDir holds exactly the keys whose last record (by FileID, offset) is
//     a PUT, each pointing at that record;
//   - every entry reads back with a valid CRC, type PUT and the same key;
//   - liveBytes and diskBytes match the files.
//
// Call with the engine open and idle, after every reopen.
func checkInvariants(tb testing.TB, fsys vfs.FS, dir string, e *Engine) {
	tb.Helper()
	files := checkerFiles(tb, fsys, dir)

	last := make(map[string]logRecord)
	var diskBytes int64
	for _, name := range files {
		diskBytes += scanInto(tb, fsys, dir, name, last)
	}

	var liveKeys int
	var liveBytes int64
	for key, rec := range last {
		entry, ok := e.keydir.Get([]byte(key))
		if rec.typ == storage.TypeDelete {
			assert.False(tb, ok, "key %q: last record is a tombstone but KeyDir has it", key)
			continue
		}
		liveKeys++
		liveBytes += storage.RecordSize(len(key), rec.valueSize)
		if !assert.True(tb, ok, "key %q: last record is a PUT but KeyDir lacks it", key) {
			continue
		}
		assert.Equal(tb, rec.fileID, entry.FileID, "key %q: FileID", key)
		assert.Equal(tb, rec.valueOffset, entry.ValueOffset, "key %q: ValueOffset", key)
		assert.Equal(tb, rec.valueSize, entry.ValueSize, "key %q: ValueSize", key)
		readBack(tb, fsys, dir, key, entry.FileID, entry.ValueOffset, entry.ValueSize)
	}
	// With every expected key present, equal sizes mean KeyDir has no extras.
	assert.Equal(tb, liveKeys, e.keydir.Len(), "KeyDir size vs keys whose last record is a PUT")
	assert.Equal(tb, liveBytes, e.liveBytes.Load(), "liveBytes vs records KeyDir should point at")
	assert.Equal(tb, diskBytes, e.diskBytes.Load(), "diskBytes vs data file sizes")
}

// checkerFiles lists dir's data files in replay order and asserts that
// recovery left no *.tmp and nothing below the highest compaction output.
func checkerFiles(tb testing.TB, fsys vfs.FS, dir string) []storage.FileName {
	tb.Helper()
	var files []storage.FileName
	var compactID uint32
	for _, base := range mustList(tb, fsys, dir) {
		name, ok, err := storage.ParseFileName(base)
		require.NoError(tb, err)
		if !ok {
			continue
		}
		require.NotEqual(tb, storage.KindCompactTmp, name.Kind, "%s survived recovery", base)
		if name.Kind == storage.KindCompact {
			compactID = max(compactID, name.ID)
		}
		files = append(files, name)
	}
	slices.SortFunc(files, func(a, b storage.FileName) int { return cmp.Compare(a.ID, b.ID) })
	for i, name := range files {
		require.GreaterOrEqual(tb, name.ID, compactID, "%s is superseded by %s", name, storage.CompactName(compactID))
		if i > 0 {
			require.NotEqual(tb, files[i-1].ID, name.ID, "two files share ID %d", name.ID)
		}
	}
	return files
}

// scanInto scans one file from offset 0, records each key's latest record in
// last, and returns the file size. Any scan error fails the test.
func scanInto(tb testing.TB, fsys vfs.FS, dir string, name storage.FileName, last map[string]logRecord) int64 {
	tb.Helper()
	f, err := fsys.Open(filepath.Join(dir, name.String()))
	require.NoError(tb, err)
	defer func() { require.NoError(tb, f.Close()) }()
	size, err := f.Size()
	require.NoError(tb, err)

	sc := storage.NewScanner(f, size, storage.ScanOptions{})
	for sc.Next() {
		rec := sc.Record()
		last[string(rec.Key)] = logRecord{
			typ:         rec.Type,
			fileID:      name.ID,
			valueOffset: sc.ValueOffset(),
			valueSize:   uint32(len(rec.Value)),
		}
	}
	require.NoError(tb, sc.Err(), "%s must scan cleanly after recovery", name)
	return size
}

// readBack reads the record an entry points at straight from the file and
// decodes it, which verifies the CRC.
func readBack(tb testing.TB, fsys vfs.FS, dir, key string, fileID uint32, valueOffset int64, valueSize uint32) {
	tb.Helper()
	path := filepath.Join(dir, fileNameByID(tb, fsys, dir, fileID))
	f, err := fsys.Open(path)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, f.Close()) }()

	buf := make([]byte, storage.RecordSize(len(key), valueSize))
	_, err = f.ReadAt(buf, storage.RecordOffset(valueOffset, len(key)))
	require.NoError(tb, err, "key %q", key)
	rec, err := storage.Decode(buf, storage.DefaultLimits())
	if assert.NoError(tb, err, "key %q: record must decode with a valid CRC", key) {
		assert.Equal(tb, storage.TypePut, rec.Type, "key %q", key)
		assert.True(tb, bytes.Equal([]byte(key), rec.Key), "key %q: stored key %q", key, rec.Key)
	}
}

// fileNameByID finds the segment or compaction output with this ID.
func fileNameByID(tb testing.TB, fsys vfs.FS, dir string, id uint32) string {
	tb.Helper()
	for _, base := range mustList(tb, fsys, dir) {
		name, ok, err := storage.ParseFileName(base)
		require.NoError(tb, err)
		if ok && name.ID == id && name.Readable() {
			return base
		}
	}
	require.Failf(tb, "missing file", "no data file with ID %d in %s", id, dir)
	return ""
}

func TestInvariants_DetectsMismatch(t *testing.T) {
	// The checker must fail on a wrong engine, or a green reopen test proves
	// nothing. Each case breaks one thing on an otherwise healthy engine.
	tests := []struct {
		name   string
		mutate func(e *Engine)
	}{
		{"missing key", func(e *Engine) { e.keydir.Delete([]byte("a")) }},
		{"resurrected key", func(e *Engine) {
			entry, _ := e.keydir.Get([]byte("a"))
			e.keydir.Put([]byte("gone"), entry)
		}},
		{"stale entry", func(e *Engine) {
			entry, _ := e.keydir.Get([]byte("b"))
			e.keydir.Put([]byte("a"), entry)
		}},
		{"liveBytes drift", func(e *Engine) { e.liveBytes.Add(1) }},
		{"diskBytes drift", func(e *Engine) { e.diskBytes.Add(-1) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, fsys := openSim(t)
			require.NoError(t, e.Put([]byte("a"), []byte("1")))
			require.NoError(t, e.Put([]byte("b"), []byte("2")))
			require.NoError(t, e.Put([]byte("gone"), []byte("3")))
			require.NoError(t, e.Delete([]byte("gone")))
			checkInvariants(t, fsys, testDir, e)

			tc.mutate(e)
			rec := &recordingTB{TB: t}
			checkInvariants(rec, fsys, testDir, e)
			assert.True(t, rec.failed, "checker missed the broken invariant")
		})
	}
}

// recordingTB records assertion failures instead of failing the real test.
// Only the methods testify's assert package calls are overridden.
type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Errorf(string, ...any) { r.failed = true }
func (r *recordingTB) Helper()               {}
