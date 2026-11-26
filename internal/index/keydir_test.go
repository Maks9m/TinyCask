package index

import (
	"flag"
	"math/rand/v2"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedFlag drives every randomized test in this package. Re-run a failure with
// go test -run TestName ./internal/index/ -seed=<logged seed>.
var seedFlag = flag.Uint64("seed", 1, "seed for randomized KeyDir tests")

func newRand(tb testing.TB) *rand.Rand {
	tb.Helper()
	seed := *seedFlag
	tb.Logf("seed=%d", seed)
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

func entry(fileID uint32, off int64) CommandIndex {
	return CommandIndex{FileID: fileID, ValueSize: 7, ValueOffset: off, Timestamp: uint64(off) * 10}
}

func TestKeyDir_Empty(t *testing.T) {
	t.Parallel()
	kd := New()

	_, ok := kd.Get([]byte("missing"))
	assert.False(t, ok)
	assert.Equal(t, 0, kd.Len())

	prev, existed := kd.Delete([]byte("missing"))
	assert.False(t, existed, "deleting an absent key reports nothing removed")
	assert.Equal(t, CommandIndex{}, prev)
	assert.Equal(t, 0, kd.Len())
}

func TestKeyDir_PutGetDelete(t *testing.T) {
	t.Parallel()
	kd := New()
	k := []byte("user:1")
	first, second := entry(1, 100), entry(2, 50)

	prev, existed := kd.Put(k, first)
	assert.False(t, existed, "first Put has no previous entry")
	assert.Equal(t, CommandIndex{}, prev)
	got, ok := kd.Get(k)
	require.True(t, ok)
	assert.Equal(t, first, got)
	assert.Equal(t, 1, kd.Len())

	prev, existed = kd.Put(k, second)
	assert.True(t, existed, "overwrite reports the previous entry")
	assert.Equal(t, first, prev)
	got, ok = kd.Get(k)
	require.True(t, ok)
	assert.Equal(t, second, got)
	assert.Equal(t, 1, kd.Len(), "overwrite does not add a key")

	prev, existed = kd.Delete(k)
	assert.True(t, existed)
	assert.Equal(t, second, prev, "Delete returns the removed entry")
	_, ok = kd.Get(k)
	assert.False(t, ok)
	assert.Equal(t, 0, kd.Len())

	_, existed = kd.Delete(k)
	assert.False(t, existed, "second Delete finds nothing")
}

func TestKeyDir_EmptyKeyIsDistinct(t *testing.T) {
	t.Parallel()
	// The engine rejects empty keys, but the index must still treat nil and
	// []byte{} as the same key and not confuse it with any other.
	kd := New()
	kd.Put(nil, entry(1, 1))
	got, ok := kd.Get([]byte{})
	require.True(t, ok)
	assert.Equal(t, entry(1, 1), got)
	_, ok = kd.Get([]byte{0})
	assert.False(t, ok)
}

func TestKeyDir_PutCopiesKey(t *testing.T) {
	t.Parallel()
	kd := New()
	buf := []byte("alpha")
	kd.Put(buf, entry(1, 10))

	copy(buf, "omega") // the caller reuses its buffer

	got, ok := kd.Get([]byte("alpha"))
	require.True(t, ok, "the stored key must not alias the caller's slice")
	assert.Equal(t, entry(1, 10), got)
	_, ok = kd.Get([]byte("omega"))
	assert.False(t, ok)
	assert.Equal(t, 1, kd.Len())
}

func TestKeyDir_UpdateIfDoesNotRetainKey(t *testing.T) {
	t.Parallel()
	kd := New()
	kd.Put([]byte("alpha"), entry(1, 10))

	buf := []byte("alpha")
	require.True(t, kd.UpdateIf(buf, func(CommandIndex) bool { return true }, entry(5, 20)))
	copy(buf, "omega")

	got, ok := kd.Get([]byte("alpha"))
	require.True(t, ok)
	assert.Equal(t, entry(5, 20), got)
	_, ok = kd.Get([]byte("omega"))
	assert.False(t, ok)
}

func TestKeyDir_UpdateIf(t *testing.T) {
	t.Parallel()
	const mergeID = 10 // compaction output ID M: swap only entries in files < M
	older := func(cur CommandIndex) bool { return cur.FileID < mergeID }

	tests := []struct {
		name      string
		existing  *CommandIndex
		wantOK    bool
		wantEntry *CommandIndex // nil: key must be absent afterwards
		wantCalls int
	}{
		{
			name:      "condition true replaces entry",
			existing:  new(entry(3, 100)),
			wantOK:    true,
			wantEntry: new(entry(mergeID, 0)),
			wantCalls: 1,
		},
		{
			name:      "condition false leaves entry",
			existing:  new(entry(mergeID+1, 100)), // overwritten during the merge
			wantOK:    false,
			wantEntry: new(entry(mergeID+1, 100)),
			wantCalls: 1,
		},
		{
			name:      "key absent is not inserted",
			existing:  nil, // deleted during the merge
			wantOK:    false,
			wantEntry: nil,
			wantCalls: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kd := New()
			k := []byte("k")
			if tc.existing != nil {
				kd.Put(k, *tc.existing)
			}
			calls := 0
			cond := func(cur CommandIndex) bool {
				calls++
				require.NotNil(t, tc.existing)
				assert.Equal(t, *tc.existing, cur, "cond sees the current entry")
				return older(cur)
			}

			ok := kd.UpdateIf(k, cond, entry(mergeID, 0))

			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantCalls, calls)
			got, present := kd.Get(k)
			if tc.wantEntry == nil {
				assert.False(t, present)
				assert.Equal(t, 0, kd.Len())
				return
			}
			require.True(t, present)
			assert.Equal(t, *tc.wantEntry, got)
			assert.Equal(t, 1, kd.Len())
		})
	}
}

// refMap is the mutex-protected reference model for the concurrency test.
type refMap struct {
	// mu protects m.
	mu sync.Mutex
	m  map[string]CommandIndex
}

func (r *refMap) get(k string) (CommandIndex, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[k]
	return e, ok
}

func (r *refMap) set(k string, e CommandIndex) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[k] = e
}

func (r *refMap) del(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, k)
}

// TestKeyDir_ConcurrentMatchesModel runs writers (Put, Delete, UpdateIf) and
// readers at the same time. Each key is owned by one writer, so the writer can
// mirror every operation into the reference map and the final states must be
// equal; readers share the whole key space with every writer. Every entry for
// key i carries ValueSize == i, so a reader can tell an entry that belongs to
// a different key, or a torn one, from a valid one.
func TestKeyDir_ConcurrentMatchesModel(t *testing.T) {
	const (
		writers      = 4
		readers      = 4
		numKeys      = 512
		opsPerWriter = 20000
		mergeID      = 8 // UpdateIf swaps only entries with FileID < mergeID
	)
	rng := newRand(t)
	keys := make([][]byte, numKeys)
	for i := range keys {
		keys[i] = []byte("key:" + strconv.Itoa(i))
	}
	writerSeeds := make([]uint64, writers)
	for w := range writerSeeds {
		writerSeeds[w] = rng.Uint64()
	}

	kd := New()
	ref := &refMap{m: make(map[string]CommandIndex)}
	done := make(chan struct{})
	var readersWG, writersWG sync.WaitGroup

	for range readers {
		readersWG.Go(func() {
			for i := 0; ; i = (i + 1) % numKeys {
				select {
				case <-done:
					return
				default:
				}
				if e, ok := kd.Get(keys[i]); ok {
					assert.Equal(t, uint32(i), e.ValueSize, "entry for key %d belongs to another key", i)
				}
				if i == 0 {
					n := kd.Len()
					assert.True(t, n >= 0 && n <= numKeys, "Len out of range: %d", n)
				}
			}
		})
	}

	for w := range writers {
		writersWG.Go(func() {
			r := rand.New(rand.NewPCG(writerSeeds[w], uint64(w)))
			for range opsPerWriter {
				// Pick a key this writer owns: i ≡ w (mod writers).
				i := r.IntN(numKeys/writers)*writers + w
				k := keys[i]
				ks := string(k)
				want, wantOK := ref.get(ks)
				e := CommandIndex{
					FileID:      uint32(r.IntN(2 * mergeID)),
					ValueSize:   uint32(i),
					ValueOffset: r.Int64N(1 << 20),
					Timestamp:   r.Uint64(),
				}
				switch op := r.IntN(10); {
				case op < 5:
					prev, existed := kd.Put(k, e)
					assert.Equal(t, wantOK, existed)
					assert.Equal(t, want, prev)
					ref.set(ks, e)
				case op < 7:
					prev, existed := kd.Delete(k)
					assert.Equal(t, wantOK, existed)
					assert.Equal(t, want, prev)
					ref.del(ks)
				default:
					e.FileID = mergeID
					swapped := kd.UpdateIf(k, func(cur CommandIndex) bool { return cur.FileID < mergeID }, e)
					wantSwap := wantOK && want.FileID < mergeID
					assert.Equal(t, wantSwap, swapped)
					if wantSwap {
						ref.set(ks, e)
					}
				}
			}
		})
	}

	writersWG.Wait()
	close(done)
	readersWG.Wait()

	require.Equal(t, len(ref.m), kd.Len())
	for _, k := range keys {
		want, wantOK := ref.m[string(k)]
		got, gotOK := kd.Get(k)
		require.Equal(t, wantOK, gotOK, "presence of %q", k)
		assert.Equal(t, want, got, "entry for %q", k)
	}
}
