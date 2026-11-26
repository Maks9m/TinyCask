package simfs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
	"github.com/Maks9m/TinyCask/internal/vfs/simfs"
	"github.com/Maks9m/TinyCask/internal/vfs/vfstest"
)

func TestSimFS_Conformance(t *testing.T) {
	t.Parallel()
	vfstest.RunFSTests(t, func(testing.TB) vfs.FS { return simfs.New() })
}

func TestSimFS_InstancesAreIndependent(t *testing.T) {
	t.Parallel()
	a, b := simfs.New(), simfs.New()
	require.NoError(t, a.MkdirAll("/db"))
	create(t, a, "/db/x", "a")

	_, err := b.List("/db")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestSimFS_SyncedLength(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "hello")
	assertSynced(t, s, "a", 0) // written but not synced

	require.NoError(t, f.Sync())
	assertSynced(t, s, "a", 5)

	write(t, f, " world")
	assertSynced(t, s, "a", 5) // the tail is unsynced again
	assertContent(t, f, "hello world")

	require.NoError(t, f.Truncate(3)) // below the synced length
	assertSynced(t, s, "a", 3)

	require.NoError(t, f.Truncate(6)) // zero-extends; the new bytes are unsynced
	assertSynced(t, s, "a", 3)
	assertContent(t, f, "hel\x00\x00\x00")

	require.NoError(t, f.Sync())
	assertSynced(t, s, "a", 6)
}

func TestSimFS_SyncedLengthIsPerInode(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "abc")
	g := open(t, s, "a")
	require.NoError(t, g.Sync()) // any handle syncs the shared file
	assertSynced(t, s, "a", 3)

	require.NoError(t, s.Rename("a", "b"))
	assertSynced(t, s, "b", 3) // the synced length moves with the file
	_, err := s.SyncedSize("a")
	require.ErrorIs(t, err, fs.ErrNotExist)

	require.NoError(t, s.Remove("b"))
	write(t, f, "def") // the orphaned inode still accepts writes and syncs
	require.NoError(t, f.Sync())
	assertContent(t, g, "abcdef")
	_, err = s.SyncedSize("b")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestSimFS_RenameReplacedHandleKeepsOldContents(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	src := create(t, s, "src", "new")
	dst := create(t, s, "dst", "old")
	require.NoError(t, s.Rename("src", "dst"))

	assertContent(t, dst, "old") // the replaced inode lives on for its handle
	write(t, src, "!")
	assertContent(t, open(t, s, "dst"), "new!")
}

func TestSimFS_RenameToSelfIsNoop(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	create(t, s, "a", "abc")
	require.NoError(t, s.Rename("a", "./a"))
	assertContent(t, open(t, s, "a"), "abc")
	assertList(t, s, ".", "a")
}

func TestSimFS_WriteCopiesCallerBuffer(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "")
	buf := []byte("abc")
	write(t, f, string(buf))
	_, err := f.Write(buf)
	require.NoError(t, err)
	buf[0] = 'X'
	assertContent(t, f, "abcabc")
}

func TestSimFS_ReadAtReturnsBareEOF(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "abc")
	n, err := f.ReadAt(make([]byte, 4), 1)
	assert.Equal(t, 2, n)
	assert.Equal(t, io.EOF, err) // unwrapped, so callers using == still work
}

func TestSimFS_RelativePaths(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	require.NoError(t, s.MkdirAll("data/sub"))
	create(t, s, "data/a", "x")
	assertList(t, s, ".", "data")
	assertList(t, s, "data", "a", "sub")
	assertList(t, s, "./data/", "a", "sub") // names are cleaned
}

func TestSimFS_DirectoryErrors(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	require.NoError(t, s.MkdirAll("/db/sub"))
	create(t, s, "/db/f", "x")

	cases := []struct {
		name string
		call func() error
	}{
		{"Create over dir", func() error { _, err := s.Create("/db/sub"); return err }},
		{"Create under file", func() error { _, err := s.Create("/db/f/a"); return err }},
		{"Open dir", func() error { _, err := s.Open("/db/sub"); return err }},
		{"Lock dir", func() error { _, err := s.Lock("/db/sub"); return err }},
		{"Lock under file", func() error { _, err := s.Lock("/db/f/LOCK"); return err }},
		{"Rename dir", func() error { return s.Rename("/db/sub", "/db/x") }},
		{"Rename onto dir", func() error { return s.Rename("/db/f", "/db/sub") }},
		{"Rename into missing dir", func() error { return s.Rename("/db/f", "/db/nope/f") }},
		{"Remove non-empty dir", func() error { return s.Remove("/db") }},
		{"Remove root", func() error { return s.Remove("/") }},
		{"Remove cwd", func() error { return s.Remove(".") }},
		{"MkdirAll over file", func() error { return s.MkdirAll("/db/f/x") }},
		{"List file", func() error { _, err := s.List("/db/f"); return err }},
		{"SyncDir file", func() error { return s.SyncDir("/db/f") }},
	}
	for _, tc := range cases {
		err := tc.call()
		require.Error(t, err, tc.name)
		assert.NotErrorIs(t, err, vfs.ErrLocked, tc.name)
		var pe *fs.PathError
		assert.ErrorAs(t, err, &pe, tc.name)
	}

	// Nothing above changed the tree.
	assertList(t, s, "/db", "f", "sub")
	assertContent(t, open(t, s, "/db/f"), "x")

	require.NoError(t, s.Remove("/db/sub")) // an empty directory can go
	assertList(t, s, "/db", "f")
}

func TestSimFS_Lock(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	l, err := s.Lock("LOCK")
	require.NoError(t, err)
	assertList(t, s, ".", "LOCK") // created on demand

	// Like flock, the lock belongs to the inode: a new file under the same
	// name is a different inode and can be locked.
	require.NoError(t, s.Remove("LOCK"))
	l2, err := s.Lock("LOCK")
	require.NoError(t, err)

	require.NoError(t, l.Close())
	require.ErrorIs(t, l.Close(), fs.ErrClosed)

	_, err = s.Lock("./LOCK") // the second inode is still held
	require.ErrorIs(t, err, vfs.ErrLocked)

	require.NoError(t, l2.Close())
	assertList(t, s, ".", "LOCK") // the file stays after release

	// A locked file can still be opened and written; flock is advisory.
	l3, err := s.Lock("LOCK")
	require.NoError(t, err)
	f := open(t, s, "LOCK")
	write(t, f, "pid")
	require.NoError(t, l3.Close())
}

func TestSimFS_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	const (
		writers    = 8
		perWriter  = 200
		recordSize = 8 // writer ID (4 bytes) + sequence number (4 bytes)
	)
	s := simfs.New()
	require.NoError(t, s.MkdirAll("/db"))
	shared := create(t, s, "/db/shared", "")

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			f, err := s.Open("/db/shared")
			if !assert.NoError(t, err) {
				return
			}
			defer func() { assert.NoError(t, f.Close()) }()
			rec := make([]byte, recordSize)
			for i := range perWriter {
				binary.LittleEndian.PutUint32(rec[:4], uint32(w))
				binary.LittleEndian.PutUint32(rec[4:], uint32(i))
				_, err := f.Write(rec)
				assert.NoError(t, err)
				if i%50 == 0 {
					assert.NoError(t, f.Sync())
				}
			}
		})
		wg.Go(func() { readWhileWriting(t, shared, writers*perWriter*recordSize) })
		wg.Go(func() { churnNamespace(t, s, w) })
	}
	wg.Wait()

	// Every Write is atomic, so the file is whole records, each writer's in order.
	assertSize(t, shared, writers*perWriter*recordSize)
	buf := make([]byte, writers*perWriter*recordSize)
	_, err := shared.ReadAt(buf, 0)
	require.NoError(t, err)
	next := make([]uint32, writers)
	for rec := range slices.Chunk(buf, recordSize) {
		w := binary.LittleEndian.Uint32(rec[:4])
		require.Less(t, w, uint32(writers))
		require.Equal(t, next[w], binary.LittleEndian.Uint32(rec[4:]))
		next[w]++
	}
}

// readWhileWriting reads the file until it reaches total bytes, checking that
// the size never shrinks and no read returns more bytes than the file held.
func readWhileWriting(t *testing.T, f vfs.File, total int64) {
	var last int64
	buf := make([]byte, 64)
	for last < total {
		size, err := f.Size()
		if !assert.NoError(t, err) {
			return
		}
		assert.GreaterOrEqual(t, size, last)
		last = size
		off := size / 2
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			assert.NoError(t, err)
			return
		}
		// Writers may grow the file between Size and ReadAt, so bound the read
		// by a size taken afterwards: it can never exceed what exists now.
		after, err := f.Size()
		if !assert.NoError(t, err) {
			return
		}
		assert.LessOrEqual(t, off+int64(n), after)
	}
}

// churnNamespace creates, renames, lists and removes files of its own while
// other goroutines write, so the namespace maps are exercised under -race.
func churnNamespace(t *testing.T, s *simfs.SimFS, id int) {
	name := "/db/tmp" + string(rune('a'+id))
	for range 50 {
		f, err := s.Create(name)
		if !assert.NoError(t, err) {
			return
		}
		_, err = f.Write([]byte("x"))
		assert.NoError(t, err)
		assert.NoError(t, f.Close())
		assert.NoError(t, s.Rename(name, name+".done"))
		_, err = s.List("/db")
		assert.NoError(t, err)
		assert.NoError(t, s.Remove(name+".done"))
	}
}

func assertSynced(t testing.TB, s *simfs.SimFS, name string, want int64) {
	t.Helper()
	got, err := s.SyncedSize(name)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func assertSize(t testing.TB, f vfs.File, want int64) {
	t.Helper()
	got, err := f.Size()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func assertContent(t testing.TB, f vfs.File, want string) {
	t.Helper()
	assertSize(t, f, int64(len(want)))
	buf := make([]byte, len(want))
	_, err := f.ReadAt(buf, 0)
	require.NoError(t, err)
	assert.True(t, bytes.Equal([]byte(want), buf), "content %q, want %q", buf, want)
}

func assertList(t testing.TB, s *simfs.SimFS, dir string, want ...string) {
	t.Helper()
	got, err := s.List(dir)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func create(t testing.TB, s *simfs.SimFS, name, content string) vfs.File {
	t.Helper()
	f, err := s.Create(name)
	require.NoError(t, err)
	t.Cleanup(func() { closeFile(t, f) })
	write(t, f, content)
	return f
}

func open(t testing.TB, s *simfs.SimFS, name string) vfs.File {
	t.Helper()
	f, err := s.Open(name)
	require.NoError(t, err)
	t.Cleanup(func() { closeFile(t, f) })
	return f
}

// closeFile closes f at cleanup. A handle the test already closed, or one a
// crash or Restart killed, is fine.
func closeFile(t testing.TB, f vfs.File) {
	t.Helper()
	if err := f.Close(); err != nil && !errors.Is(err, fs.ErrClosed) && !errors.Is(err, simfs.ErrCrashed) {
		t.Errorf("close: %v", err)
	}
}

func write(t testing.TB, f vfs.File, s string) {
	t.Helper()
	n, err := f.Write([]byte(s))
	require.NoError(t, err)
	require.Equal(t, len(s), n)
}
