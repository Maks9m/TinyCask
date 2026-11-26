package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
)

// TestEngine_EndToEndOnOSFS runs the T2.2 surface on the real disk to catch
// drift between SimFS and OSFS.
func TestEngine_EndToEndOnOSFS(t *testing.T) {
	fsys := vfs.OSFS{}
	dir := filepath.Join(t.TempDir(), "nested", "data")
	e, err := Open(fsys, dir, testOptions())
	require.NoError(t, err)

	_, err = Open(fsys, dir, testOptions())
	require.ErrorIs(t, err, ErrLocked)

	value := strings.Repeat("v", 300)
	const keys = 30 // ~9.6 KiB: rolls at least twice
	for i := range keys {
		require.NoError(t, e.Put(fmt.Appendf(nil, "key-%02d", i), []byte(value)))
	}
	require.NoError(t, e.Put([]byte("key-00"), []byte("overwritten")))
	require.NoError(t, e.Delete([]byte("key-01")))
	require.ErrorIs(t, e.Delete([]byte("key-01")), ErrKeyNotFound)

	assertValue(t, e, "key-00", "overwritten")
	_, err = e.Get([]byte("key-01"))
	require.ErrorIs(t, err, ErrKeyNotFound)
	for i := 2; i < keys; i++ {
		assertValue(t, e, fmt.Sprintf("key-%02d", i), value)
	}

	files := dataFiles(t, fsys, dir)
	require.GreaterOrEqual(t, len(files), 3)
	for _, name := range files {
		assert.LessOrEqual(t, fileSize(t, fsys, dir, name), int64(testSegmentSize), name)
	}
	st := e.Stats()
	assert.Equal(t, keys-1, st.Keys)
	assert.Equal(t, totalFileSize(t, fsys, dir), st.DiskBytes)
	wantLive := recordSize("key-00", "overwritten") + int64(keys-2)*recordSize("key-02", value)
	assert.Equal(t, wantLive, st.LiveBytes)

	require.NoError(t, e.Close())
	require.NoError(t, e.Close())
	require.ErrorIs(t, e.Put([]byte("k"), []byte("v")), ErrClosed)
	requireLockFree(t, fsys, dir)
	assert.Equal(t, storage.SegmentName(firstSegmentID), files[0])
}
