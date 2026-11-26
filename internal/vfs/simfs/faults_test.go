package simfs_test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
	"github.com/Maks9m/TinyCask/internal/vfs/simfs"
	"github.com/Maks9m/TinyCask/internal/vfs/vfstest"
)

var errBoom = errors.New("boom")

func TestFault_FailNth(t *testing.T) {
	t.Parallel()
	// Each case arms its operation with n = 2 and calls it three times: only
	// the second call fails, it changes nothing, and the fault does not refire.
	cases := []struct {
		op simfs.Op
		// prep builds the fixture and returns the i-th call (i = 1, 2, 3).
		prep func(t *testing.T, s *simfs.SimFS) func(i int) error
		// afterFail checks the state right after the failed call; want, at the end.
		afterFail, want func(t *testing.T, s *simfs.SimFS)
	}{
		{
			op: simfs.OpWrite,
			prep: func(t *testing.T, s *simfs.SimFS) func(int) error {
				f := create(t, s, "a", "")
				return func(i int) error {
					_, err := f.Write([]byte{byte('0' + i)})
					return err
				}
			},
			afterFail: func(t *testing.T, s *simfs.SimFS) { assertContent(t, open(t, s, "a"), "1") },
			want:      func(t *testing.T, s *simfs.SimFS) { assertContent(t, open(t, s, "a"), "13") },
		},
		{
			op: simfs.OpSync,
			prep: func(t *testing.T, s *simfs.SimFS) func(int) error {
				f := create(t, s, "a", "")
				return func(int) error {
					write(t, f, "x")
					return f.Sync()
				}
			},
			afterFail: func(t *testing.T, s *simfs.SimFS) { assertSynced(t, s, "a", 1) },
			want:      func(t *testing.T, s *simfs.SimFS) { assertSynced(t, s, "a", 3) },
		},
		{
			op: simfs.OpCreate,
			prep: func(t *testing.T, s *simfs.SimFS) func(int) error {
				return func(i int) error {
					f, err := s.Create(fmt.Sprintf("f%d", i))
					if err != nil {
						return err
					}
					return f.Close()
				}
			},
			afterFail: func(t *testing.T, s *simfs.SimFS) { assertList(t, s, ".", "f1") },
			want:      func(t *testing.T, s *simfs.SimFS) { assertList(t, s, ".", "f1", "f3") },
		},
		{
			op: simfs.OpRename,
			prep: func(t *testing.T, s *simfs.SimFS) func(int) error {
				for i := 1; i <= 3; i++ {
					create(t, s, fmt.Sprintf("a%d", i), "x")
				}
				return func(i int) error { return s.Rename(fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i)) }
			},
			afterFail: func(t *testing.T, s *simfs.SimFS) { assertList(t, s, ".", "a2", "a3", "b1") },
			want:      func(t *testing.T, s *simfs.SimFS) { assertList(t, s, ".", "a2", "b1", "b3") },
		},
		{
			op: simfs.OpRemove,
			prep: func(t *testing.T, s *simfs.SimFS) func(int) error {
				for i := 1; i <= 3; i++ {
					create(t, s, fmt.Sprintf("r%d", i), "x")
				}
				return func(i int) error { return s.Remove(fmt.Sprintf("r%d", i)) }
			},
			afterFail: func(t *testing.T, s *simfs.SimFS) { assertList(t, s, ".", "r2", "r3") },
			want:      func(t *testing.T, s *simfs.SimFS) { assertList(t, s, ".", "r2") },
		},
		{
			op: simfs.OpTruncate,
			prep: func(t *testing.T, s *simfs.SimFS) func(int) error {
				f := create(t, s, "a", "abcdef")
				return func(i int) error { return f.Truncate(int64(6 - i)) }
			},
			afterFail: func(t *testing.T, s *simfs.SimFS) { assertContent(t, open(t, s, "a"), "abcde") },
			want:      func(t *testing.T, s *simfs.SimFS) { assertContent(t, open(t, s, "a"), "abc") },
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.op), func(t *testing.T) {
			t.Parallel()
			s := simfs.New()
			call := tc.prep(t, s)
			s.FailNth(tc.op, 2, errBoom)

			require.NoError(t, call(1))
			err := call(2)
			require.ErrorIs(t, err, errBoom)
			var pe *fs.PathError
			assert.ErrorAs(t, err, &pe, "the injected error is wrapped like a real one")
			tc.afterFail(t, s)

			require.NoError(t, call(3), "the fault fires once")
			tc.want(t, s)
			assert.False(t, s.Crashed(), "a failed call is not a crash")
		})
	}
}

func TestFault_FailNthArming(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "ab") // calls before arming are not counted

	// Faults on different operations count independently.
	s.FailNth(simfs.OpWrite, 2, errBoom)
	s.FailNth(simfs.OpSync, 1, nil)
	require.ErrorIs(t, f.Sync(), simfs.ErrInjected, "a nil error defaults to ErrInjected")
	write(t, f, "c")
	_, err := f.Write([]byte("d"))
	require.ErrorIs(t, err, errBoom)

	// Arming again replaces the pending fault; n < 1 means the next call.
	s.FailNth(simfs.OpWrite, 5, errBoom)
	s.FailNth(simfs.OpWrite, 0, io.ErrUnexpectedEOF)
	_, err = f.Write([]byte("e"))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	write(t, f, "f")
	assertContent(t, f, "abcf")
}

func TestFault_TearWrite(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	a := create(t, s, "a", "abc")
	b := create(t, s, "b", "")
	require.NoError(t, a.Sync())

	// The tear point counts bytes across all files: 3 in a, then 2 in b.
	s.TearAt(7)
	write(t, b, "de")
	n, err := a.Write([]byte("fghij"))
	assert.Equal(t, 2, n, "only the bytes up to the tear point land")
	require.ErrorIs(t, err, simfs.ErrCrashed)
	assert.True(t, s.Crashed())
	assert.Equal(t, int64(7), s.BytesWritten())

	_, err = b.Write([]byte("x"))
	require.ErrorIs(t, err, simfs.ErrCrashed, "the whole FS is down, not just the torn file")

	s.Restart()
	assertContent(t, open(t, s, "a"), "abcfg") // the applied prefix survives a process crash
	assertContent(t, open(t, s, "b"), "de")

	// The tear fired once: writes past the old point now succeed.
	write(t, open(t, s, "a"), "more")
	assert.False(t, s.Crashed())
}

func TestFault_TearAtOrBeforeCounter(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "abc")
	s.TearAt(1) // already passed

	write(t, f, "") // an empty write cannot tear
	n, err := f.Write([]byte("d"))
	assert.Equal(t, 0, n)
	require.ErrorIs(t, err, simfs.ErrCrashed)

	s.Restart()
	assertContent(t, open(t, s, "a"), "abc")
}

func TestFault_Quota(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	s.SetQuota(10)
	a := create(t, s, "a", "12345678")
	b := create(t, s, "b", "")

	n, err := b.Write([]byte("abcde"))
	assert.Equal(t, 2, n, "the bytes that fit are applied")
	require.ErrorIs(t, err, vfs.ErrNoSpace)
	assertContent(t, b, "ab")
	n, err = b.Write([]byte("x"))
	assert.Equal(t, 0, n)
	require.ErrorIs(t, err, vfs.ErrNoSpace)
	assert.False(t, s.Crashed(), "a full disk is not a crash")
	assert.Equal(t, int64(10), s.BytesWritten())

	require.NoError(t, s.Remove("a")) // frees 8 bytes, though a is still open
	write(t, b, "cdefghij")
	_, err = a.Write([]byte("x"))
	require.ErrorIs(t, err, vfs.ErrNoSpace, "a write to the removed file is still limited by the quota")

	require.NoError(t, b.Truncate(4)) // truncating frees bytes too
	write(t, b, "123456")

	c := create(t, s, "c", "") // replacing b by rename frees b's 10 bytes
	require.NoError(t, s.Rename("c", "b"))
	write(t, c, "0123456789")

	s.SetQuota(simfs.NoQuota)
	write(t, c, "unlimited")
}

func TestFault_QuotaAndTearTogether(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "")
	s.SetQuota(4)
	s.TearAt(6)

	// The quota cuts first: a short write, no crash, the tear stays armed.
	n, err := f.Write([]byte("abcdefgh"))
	assert.Equal(t, 4, n)
	require.ErrorIs(t, err, vfs.ErrNoSpace)
	assert.False(t, s.Crashed())

	// With room again the tear fires at byte 6.
	s.SetQuota(100)
	n, err = f.Write([]byte("efgh"))
	assert.Equal(t, 2, n)
	require.ErrorIs(t, err, simfs.ErrCrashed)
}

func TestFault_CrashRejectsEveryCall(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	require.NoError(t, s.MkdirAll("/db"))
	f := create(t, s, "/db/a", "abc")
	l, err := s.Lock("/db/LOCK")
	require.NoError(t, err)

	s.Crash()
	assert.True(t, s.Crashed())
	for _, c := range everyCall(s, f) {
		assert.ErrorIs(t, c.call(), simfs.ErrCrashed, c.name)
	}
	require.ErrorIs(t, l.Close(), simfs.ErrCrashed)
	assertSynced(t, s, "/db/a", 0) // inspection helpers still work

	s.Restart()
	assert.False(t, s.Crashed())
	for _, c := range everyFileCall(f) {
		assert.ErrorIs(t, c.call(), simfs.ErrCrashed, "old handle, "+c.name)
	}
	require.ErrorIs(t, l.Close(), simfs.ErrCrashed, "old lock handle")

	// The revived FS works, the lock was released, and every FS call succeeds.
	g := open(t, s, "/db/a")
	assertContent(t, g, "abc")
	l2, err := s.Lock("/db/LOCK")
	require.NoError(t, err)
	require.NoError(t, l2.Close())
	for _, c := range everyCall(s, g) {
		assert.NoError(t, c.call(), c.name)
	}
}

func TestFault_RestartWithoutCrashKillsHandles(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "abc")
	l, err := s.Lock("LOCK")
	require.NoError(t, err)

	s.Restart() // a reboot with no crash first
	_, err = f.Write([]byte("d"))
	require.ErrorIs(t, err, simfs.ErrCrashed)
	require.ErrorIs(t, l.Close(), simfs.ErrCrashed)
	l2, err := s.Lock("LOCK")
	require.NoError(t, err, "Restart released the lock")
	require.NoError(t, l2.Close())
	assertContent(t, open(t, s, "a"), "abc")
}

func TestFault_RestartKeepsUnsynced(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "hello")
	require.NoError(t, f.Sync())
	write(t, f, " world")

	s.Crash()
	s.Restart()
	assertContent(t, open(t, s, "a"), "hello world")
	assertSynced(t, s, "a", 5)
}

func TestFault_RestartLoseUnsynced(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	require.NoError(t, s.MkdirAll("/db"))
	partial := create(t, s, "/db/partial", "hello")
	require.NoError(t, partial.Sync())
	write(t, partial, " world")
	create(t, s, "/db/never", "xyz")
	full := create(t, s, "/db/full", "all")
	require.NoError(t, full.Sync())

	s.Crash()
	s.Restart(simfs.LoseUnsynced)

	assertContent(t, open(t, s, "/db/partial"), "hello")
	assertContent(t, open(t, s, "/db/never"), "") // the name is durable, the bytes are not
	assertContent(t, open(t, s, "/db/full"), "all")
	assertList(t, s, "/db", "full", "never", "partial")
	assertSynced(t, s, "/db/partial", 5)
}

func TestFault_RestartDisarmsFaults(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	f := create(t, s, "a", "ab")
	s.FailNth(simfs.OpWrite, 1, errBoom)
	s.FailNth(simfs.OpSync, 1, errBoom)
	s.TearAt(s.BytesWritten() + 1)
	s.SetQuota(5)

	s.Restart()
	assert.Equal(t, int64(2), s.BytesWritten(), "the counter persists")
	g := open(t, s, "a")
	write(t, g, "cd") // neither the FailNth nor the tear fires
	require.NoError(t, g.Sync())
	n, err := g.Write([]byte("ef"))
	assert.Equal(t, 1, n, "the quota persists: it is the disk size")
	require.ErrorIs(t, err, vfs.ErrNoSpace)
	_, err = f.Write([]byte("x"))
	require.ErrorIs(t, err, simfs.ErrCrashed)
}

func TestFault_BytesWritten(t *testing.T) {
	t.Parallel()
	s := simfs.New()
	a := create(t, s, "a", "abc")
	b := create(t, s, "b", "de")
	assert.Equal(t, int64(5), s.BytesWritten(), "counts every file")

	s.FailNth(simfs.OpWrite, 1, errBoom)
	_, err := a.Write([]byte("xyz"))
	require.ErrorIs(t, err, errBoom)
	require.NoError(t, b.Close())
	_, err = b.Write([]byte("xyz"))
	require.ErrorIs(t, err, fs.ErrClosed)
	assert.Equal(t, int64(5), s.BytesWritten(), "failed writes apply nothing")

	require.NoError(t, a.Truncate(0))
	assert.Equal(t, int64(5), s.BytesWritten(), "truncation does not rewind the counter")

	s.SetQuota(4) // b holds 2 bytes, so 2 of the 3 fit
	n, err := a.Write([]byte("xyz"))
	assert.Equal(t, 2, n)
	require.ErrorIs(t, err, vfs.ErrNoSpace)
	assert.Equal(t, int64(5+n), s.BytesWritten(), "a short write counts its prefix")
}

func TestFault_ConcurrentWritersTear(t *testing.T) {
	t.Parallel()
	const (
		writers   = 8
		chunk     = 10
		maxChunks = 1000 // a bound so a bug cannot make the test loop forever
		tearAt    = 1234 // not a multiple of chunk, so one write is torn
	)
	s := simfs.New()
	files := make([]vfs.File, writers)
	for w := range writers {
		files[w] = create(t, s, fmt.Sprintf("w%d", w), "")
	}
	s.TearAt(tearAt)

	errs := make([]error, writers)
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			buf := make([]byte, chunk)
			for range maxChunks {
				if _, err := files[w].Write(buf); err != nil {
					errs[w] = err
					return
				}
			}
		})
	}
	wg.Wait()

	// However the writers interleave, exactly tearAt bytes land and everyone stops.
	assert.True(t, s.Crashed())
	assert.Equal(t, int64(tearAt), s.BytesWritten())
	for w, err := range errs {
		assert.ErrorIs(t, err, simfs.ErrCrashed, "writer %d", w)
	}
	s.Restart()
	var total int64
	for w := range writers {
		size, err := open(t, s, fmt.Sprintf("w%d", w)).Size()
		require.NoError(t, err)
		total += size
	}
	assert.Equal(t, int64(tearAt), total)
}

func TestFault_ConformanceAfterRestart(t *testing.T) {
	t.Parallel()
	vfstest.RunFSTests(t, func(testing.TB) vfs.FS {
		s := simfs.New()
		s.Crash()
		s.Restart(simfs.LoseUnsynced)
		return s
	})
}

// namedCall is one call in an ordered list; later calls may rely on earlier ones.
type namedCall struct {
	name string
	call func() error
}

// everyCall returns one call of each FS method, then each method of f, Close
// last. In this order every call succeeds on a healthy FS where /db and
// /db/a exist.
func everyCall(s *simfs.SimFS, f vfs.File) []namedCall {
	calls := []namedCall{
		{"Create", func() error { return closeIfOpened(s.Create("/db/new")) }},
		{"Open", func() error { return closeIfOpened(s.Open("/db/a")) }},
		{"Rename", func() error { return s.Rename("/db/new", "/db/renamed") }},
		{"Remove", func() error { return s.Remove("/db/renamed") }},
		{"List", func() error {
			_, err := s.List("/db")
			return err
		}},
		{"MkdirAll", func() error { return s.MkdirAll("/db/sub") }},
		{"SyncDir", func() error { return s.SyncDir("/db") }},
		{"Lock", func() error {
			l, err := s.Lock("/db/LOCK2")
			if err != nil {
				return err
			}
			return l.Close()
		}},
	}
	return append(calls, everyFileCall(f)...)
}

// everyFileCall returns one call of each method of f, Close last.
func everyFileCall(f vfs.File) []namedCall {
	return []namedCall{
		{"File.Write", func() error {
			_, err := f.Write([]byte("x"))
			return err
		}},
		{"File.ReadAt", func() error {
			_, err := f.ReadAt(make([]byte, 1), 0)
			return err
		}},
		{"File.Sync", f.Sync},
		{"File.Truncate", func() error { return f.Truncate(3) }},
		{"File.Size", func() error {
			_, err := f.Size()
			return err
		}},
		{"File.Close", f.Close},
	}
}

func closeIfOpened(f vfs.File, err error) error {
	if err != nil {
		return err
	}
	return f.Close()
}
