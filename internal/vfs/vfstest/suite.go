// Package vfstest is the conformance suite every vfs.FS implementation must
// pass (ARCHITECTURE §11, §13). It is imported only from _test.go files.
package vfstest

import (
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// filePrefix is the content most cases write before checking a behavior.
const filePrefix = "hello world"

// RunFSTests runs the conformance suite against the FS that newFS returns.
// newFS is called once per subtest and must return a fresh, independent FS.
// Every subtest works in its own directory under t.TempDir(), so an in-memory
// FS sees ordinary absolute paths and OSFS stays inside the test's temp dir.
func RunFSTests(t *testing.T, newFS func(t testing.TB) vfs.FS) {
	t.Helper()

	cases := []struct {
		name string
		run  func(t *testing.T, fsys vfs.FS, dir string)
	}{
		{"Create_NewFileIsEmptyAndReadable", testCreateNew},
		{"Create_ExistingFails", testCreateExisting},
		{"Create_MissingDirFails", testCreateMissingDir},
		{"Open_ReadsAndAppends", testOpenExisting},
		{"Open_MissingFails", testOpenMissing},
		{"Write_Appends", testWriteAppends},
		{"Write_AppendsAfterTruncate", testWriteAfterTruncate},
		{"Write_VisibleThroughOtherHandle", testWriteVisibleElsewhere},
		{"ReadAt_Offsets", testReadAtOffsets},
		{"ReadAt_PastEOF", testReadAtPastEOF},
		{"ReadAt_NegativeOffsetFails", testReadAtNegative},
		{"Truncate_Shrinks", testTruncateShrinks},
		{"Truncate_NegativeFails", testTruncateNegative},
		{"Truncate_KeepsFile", testTruncateKeepsFile},
		{"Sync_Succeeds", testSync},
		{"Close_LaterCallsFail", testCloseThenUse},
		{"Remove_DeletesFile", testRemove},
		{"Remove_MissingFails", testRemoveMissing},
		{"Remove_OpenHandleKeepsWorking", testRemoveOpenHandle},
		{"Rename_MovesFile", testRenameMoves},
		{"Rename_ReplacesTarget", testRenameReplaces},
		{"Rename_MissingFails", testRenameMissing},
		{"Rename_OpenHandleKeepsWorking", testRenameOpenHandle},
		{"List_SortedBaseNames", testListSorted},
		{"List_EmptyDir", testListEmpty},
		{"List_MissingDirFails", testListMissing},
		{"MkdirAll_NestedAndIdempotent", testMkdirAll},
		{"SyncDir_Succeeds", testSyncDir},
		{"SyncDir_MissingFails", testSyncDirMissing},
		{"Lock_IsExclusive", testLockExclusive},
		{"Lock_MissingDirFails", testLockMissingDir},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := newFS(t)
			dir := filepath.Join(t.TempDir(), "db")
			require.NoError(t, fsys.MkdirAll(dir))
			tc.run(t, fsys, dir)
		})
	}
}

func testCreateNew(t *testing.T, fsys vfs.FS, dir string) {
	f := create(t, fsys, filepath.Join(dir, "a"), "")
	assertSize(t, f, 0)
	write(t, f, filePrefix)
	assertContent(t, f, filePrefix)
}

func testCreateExisting(t *testing.T, fsys vfs.FS, dir string) {
	name := filepath.Join(dir, "a")
	create(t, fsys, name, filePrefix)

	f, err := fsys.Create(name)
	require.ErrorIs(t, err, fs.ErrExist)
	assert.Nil(t, f)

	g := open(t, fsys, name)
	assertContent(t, g, filePrefix) // the failed Create must not truncate
}

func testCreateMissingDir(t *testing.T, fsys vfs.FS, dir string) {
	_, err := fsys.Create(filepath.Join(dir, "missing", "a"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func testOpenExisting(t *testing.T, fsys vfs.FS, dir string) {
	name := filepath.Join(dir, "a")
	create(t, fsys, name, filePrefix)

	f := open(t, fsys, name)
	assertContent(t, f, filePrefix)
	write(t, f, "!")
	assertContent(t, f, filePrefix+"!")
}

func testOpenMissing(t *testing.T, fsys vfs.FS, dir string) {
	f, err := fsys.Open(filepath.Join(dir, "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Nil(t, f)
}

func testWriteAppends(t *testing.T, fsys vfs.FS, dir string) {
	f := create(t, fsys, filepath.Join(dir, "a"), "")
	write(t, f, "abc")
	write(t, f, "")
	write(t, f, "def")
	assertSize(t, f, 6)
	assertContent(t, f, "abcdef")
}

func testWriteAfterTruncate(t *testing.T, fsys vfs.FS, dir string) {
	f := create(t, fsys, filepath.Join(dir, "a"), filePrefix)
	require.NoError(t, f.Truncate(5))
	write(t, f, "!")
	assertContent(t, f, "hello!")

	require.NoError(t, f.Truncate(0))
	write(t, f, "x")
	assertContent(t, f, "x")
}

func testWriteVisibleElsewhere(t *testing.T, fsys vfs.FS, dir string) {
	name := filepath.Join(dir, "a")
	w := create(t, fsys, name, "")
	r := open(t, fsys, name)

	write(t, w, "abc") // unsynced bytes are visible, as through the page cache
	assertContent(t, r, "abc")
	write(t, r, "def") // both handles append at the shared end
	assertContent(t, w, "abcdef")
}

func testReadAtOffsets(t *testing.T, fsys vfs.FS, dir string) {
	f := create(t, fsys, filepath.Join(dir, "a"), filePrefix)

	buf := make([]byte, 3)
	n, err := f.ReadAt(buf, 6)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, "wor", string(buf))

	n, err = f.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, "hel", string(buf))

	n, err = f.ReadAt(buf[:0], 3)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func testReadAtPastEOF(t *testing.T, fsys vfs.FS, dir string) {
	f := create(t, fsys, filepath.Join(dir, "a"), filePrefix)
	size := int64(len(filePrefix))

	buf := make([]byte, 8)
	n, err := f.ReadAt(buf, size-3) // straddles the end
	require.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 3, n)
	assert.Equal(t, "rld", string(buf[:n]))

	n, err = f.ReadAt(buf, size) // starts exactly at the end
	require.ErrorIs(t, err, io.EOF)
	assert.Zero(t, n)

	n, err = f.ReadAt(buf, size+100) // starts beyond the end
	require.ErrorIs(t, err, io.EOF)
	assert.Zero(t, n)
}

func testReadAtNegative(t *testing.T, fsys vfs.FS, dir string) {
	f := create(t, fsys, filepath.Join(dir, "a"), filePrefix)
	n, err := f.ReadAt(make([]byte, 1), -1)
	require.Error(t, err)
	assert.NotErrorIs(t, err, io.EOF)
	assert.Zero(t, n)
}

func testTruncateShrinks(t *testing.T, fsys vfs.FS, dir string) {
	name := filepath.Join(dir, "a")
	f := create(t, fsys, name, filePrefix)
	require.NoError(t, f.Truncate(5))
	assertSize(t, f, 5)
	assertContent(t, f, "hello")

	require.NoError(t, f.Truncate(5)) // same size is a no-op
	assertContent(t, f, "hello")

	assertContent(t, open(t, fsys, name), "hello") // visible to other handles
}

func testTruncateNegative(t *testing.T, fsys vfs.FS, dir string) {
	f := create(t, fsys, filepath.Join(dir, "a"), filePrefix)
	require.Error(t, f.Truncate(-1))
	assertContent(t, f, filePrefix)
}

func testTruncateKeepsFile(t *testing.T, fsys vfs.FS, dir string) {
	name := filepath.Join(dir, "a")
	f := create(t, fsys, name, filePrefix)

	require.NoError(t, f.Truncate(0))
	assertSize(t, f, 0)
	assertList(t, fsys, dir, "a") // an empty file is still a file
}

func testSync(t *testing.T, fsys vfs.FS, dir string) {
	f := create(t, fsys, filepath.Join(dir, "a"), "")
	require.NoError(t, f.Sync()) // empty file
	write(t, f, filePrefix)
	require.NoError(t, f.Sync())
	assertContent(t, f, filePrefix)
}

func testCloseThenUse(t *testing.T, fsys vfs.FS, dir string) {
	f, err := fsys.Create(filepath.Join(dir, "a"))
	require.NoError(t, err)
	_, err = f.Write([]byte(filePrefix))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = f.Write([]byte("x"))
	require.ErrorIs(t, err, fs.ErrClosed, "Write")
	_, err = f.ReadAt(make([]byte, 1), 0)
	require.ErrorIs(t, err, fs.ErrClosed, "ReadAt")
	require.ErrorIs(t, f.Sync(), fs.ErrClosed, "Sync")
	require.ErrorIs(t, f.Truncate(0), fs.ErrClosed, "Truncate")
	_, err = f.Size()
	require.ErrorIs(t, err, fs.ErrClosed, "Size")
	require.ErrorIs(t, f.Close(), fs.ErrClosed, "second Close")
}

func testRemove(t *testing.T, fsys vfs.FS, dir string) {
	name := filepath.Join(dir, "a")
	create(t, fsys, name, filePrefix)
	require.NoError(t, fsys.Remove(name))

	_, err := fsys.Open(name)
	require.ErrorIs(t, err, fs.ErrNotExist)
	assertList(t, fsys, dir)

	create(t, fsys, name, "") // the name is free again
}

func testRemoveMissing(t *testing.T, fsys vfs.FS, dir string) {
	require.ErrorIs(t, fsys.Remove(filepath.Join(dir, "missing")), fs.ErrNotExist)
}

func testRemoveOpenHandle(t *testing.T, fsys vfs.FS, dir string) {
	name := filepath.Join(dir, "a")
	f := create(t, fsys, name, filePrefix)
	require.NoError(t, fsys.Remove(name))

	assertContent(t, f, filePrefix)
	write(t, f, "!")
	require.NoError(t, f.Sync())
	assertContent(t, f, filePrefix+"!")
}

func testRenameMoves(t *testing.T, fsys vfs.FS, dir string) {
	oldName, newName := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	create(t, fsys, oldName, filePrefix)
	require.NoError(t, fsys.Rename(oldName, newName))

	_, err := fsys.Open(oldName)
	require.ErrorIs(t, err, fs.ErrNotExist)
	assertContent(t, open(t, fsys, newName), filePrefix)
	assertList(t, fsys, dir, "b")
}

func testRenameReplaces(t *testing.T, fsys vfs.FS, dir string) {
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	create(t, fsys, src, "new contents")
	create(t, fsys, dst, "old contents that are longer")
	require.NoError(t, fsys.Rename(src, dst))

	assertContent(t, open(t, fsys, dst), "new contents")
	assertList(t, fsys, dir, "dst")
}

func testRenameMissing(t *testing.T, fsys vfs.FS, dir string) {
	dst := filepath.Join(dir, "dst")
	create(t, fsys, dst, filePrefix)

	err := fsys.Rename(filepath.Join(dir, "missing"), dst)
	require.ErrorIs(t, err, fs.ErrNotExist)
	assertContent(t, open(t, fsys, dst), filePrefix) // target untouched
}

func testRenameOpenHandle(t *testing.T, fsys vfs.FS, dir string) {
	oldName, newName := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	f := create(t, fsys, oldName, filePrefix)
	require.NoError(t, fsys.Rename(oldName, newName))

	write(t, f, "!") // the handle follows the file, not the name
	assertContent(t, f, filePrefix+"!")
	assertContent(t, open(t, fsys, newName), filePrefix+"!")
}

func testListSorted(t *testing.T, fsys vfs.FS, dir string) {
	for _, n := range []string{"c", "a", "b10", "b2"} {
		create(t, fsys, filepath.Join(dir, n), "")
	}
	assertList(t, fsys, dir, "a", "b10", "b2", "c")
}

func testListEmpty(t *testing.T, fsys vfs.FS, dir string) {
	assertList(t, fsys, dir)
}

func testListMissing(t *testing.T, fsys vfs.FS, dir string) {
	_, err := fsys.List(filepath.Join(dir, "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func testMkdirAll(t *testing.T, fsys vfs.FS, dir string) {
	nested := filepath.Join(dir, "x", "y", "z")
	require.NoError(t, fsys.MkdirAll(nested))
	require.NoError(t, fsys.MkdirAll(nested), "second MkdirAll")

	create(t, fsys, filepath.Join(nested, "a"), "")
	assertList(t, fsys, nested, "a")
	assertList(t, fsys, dir, "x")
}

func testSyncDir(t *testing.T, fsys vfs.FS, dir string) {
	require.NoError(t, fsys.SyncDir(dir)) // empty directory
	create(t, fsys, filepath.Join(dir, "a"), filePrefix)
	require.NoError(t, fsys.Rename(filepath.Join(dir, "a"), filepath.Join(dir, "b")))
	require.NoError(t, fsys.SyncDir(dir))
	assertList(t, fsys, dir, "b")
}

func testSyncDirMissing(t *testing.T, fsys vfs.FS, dir string) {
	require.ErrorIs(t, fsys.SyncDir(filepath.Join(dir, "missing")), fs.ErrNotExist)
}

func testLockExclusive(t *testing.T, fsys vfs.FS, dir string) {
	name := filepath.Join(dir, "LOCK")
	first, err := fsys.Lock(name)
	require.NoError(t, err)

	second, err := fsys.Lock(name)
	require.ErrorIs(t, err, vfs.ErrLocked)
	assert.Nil(t, second)

	require.NoError(t, first.Close())

	third, err := fsys.Lock(name) // released by Close
	require.NoError(t, err)
	require.NoError(t, third.Close())

	other, err := fsys.Lock(filepath.Join(dir, "OTHER")) // locks are per name
	require.NoError(t, err)
	require.NoError(t, other.Close())
}

func testLockMissingDir(t *testing.T, fsys vfs.FS, dir string) {
	l, err := fsys.Lock(filepath.Join(dir, "missing", "LOCK"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.NotErrorIs(t, err, vfs.ErrLocked)
	assert.Nil(t, l)
}

// create makes name with the given content and closes it when the test ends.
func create(t testing.TB, fsys vfs.FS, name, content string) vfs.File {
	t.Helper()
	f, err := fsys.Create(name)
	require.NoError(t, err)
	t.Cleanup(func() { closeFile(t, f) })
	write(t, f, content)
	return f
}

// open opens name and closes it when the test ends.
func open(t testing.TB, fsys vfs.FS, name string) vfs.File {
	t.Helper()
	f, err := fsys.Open(name)
	require.NoError(t, err)
	t.Cleanup(func() { closeFile(t, f) })
	return f
}

// closeFile closes f, tolerating a handle the test already closed itself.
func closeFile(t testing.TB, f vfs.File) {
	t.Helper()
	if err := f.Close(); err != nil && !errors.Is(err, fs.ErrClosed) {
		t.Errorf("close: %v", err)
	}
}

func write(t testing.TB, f vfs.File, s string) {
	t.Helper()
	n, err := f.Write([]byte(s))
	require.NoError(t, err)
	require.Equal(t, len(s), n)
}

func assertSize(t testing.TB, f vfs.File, want int64) {
	t.Helper()
	got, err := f.Size()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// assertContent checks the whole file through Size and ReadAt.
func assertContent(t testing.TB, f vfs.File, want string) {
	t.Helper()
	assertSize(t, f, int64(len(want)))
	buf := make([]byte, len(want))
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		require.NoError(t, err)
	}
	assert.Equal(t, want, string(buf[:n]))
}

func assertList(t testing.TB, fsys vfs.FS, dir string, want ...string) {
	t.Helper()
	got, err := fsys.List(dir)
	require.NoError(t, err)
	if len(want) == 0 {
		assert.Empty(t, got)
		return
	}
	assert.Equal(t, want, got)
}
