package storage

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// storedFrame is a frame and where the Store put it.
type storedFrame struct {
	id    uint32
	off   int64
	frame []byte
}

// TestStore_ConcurrentReadAppendDrop runs readers against sealed files that a
// dropper removes and against an active segment a writer appends to. Every
// read must return exactly the bytes written or, for a dropped file,
// ErrSegmentNotFound; never a read on a closed file (ARCHITECTURE §6). On
// OSFS, so a real closed descriptor would surface as an error.
func TestStore_ConcurrentReadAppendDrop(t *testing.T) {
	t.Parallel()
	const (
		sealedFiles  = 8
		perFile      = 16
		activeFrames = 200
		readers      = 4
		minReads     = 300
	)
	rng := testRand(t)
	dir := t.TempDir()
	s := NewStore(vfs.OSFS{}, dir, StoreOptions{Limits: testLimits})
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	require.NoError(t, s.CreateActive(1))
	var sealed []storedFrame
	for id := uint32(1); id <= sealedFiles; id++ {
		for j := range perFile {
			frame := testFrame(t, fmt.Sprintf("s%d-%d", id, j), rng.IntN(int(testLimits.MaxValueSize)+1))
			sealed = append(sealed, storedFrame{id: id, off: appendFrame(t, s, frame, id), frame: frame})
		}
		require.NoError(t, s.Roll(id+1))
	}
	const activeID = sealedFiles + 1

	// The writer fills in active[i].off and then publishes i+1; readers only
	// touch entries below the published count.
	active := make([]storedFrame, activeFrames)
	for i := range active {
		active[i] = storedFrame{id: activeID, frame: testFrame(t, fmt.Sprintf("a%d", i), rng.IntN(int(testLimits.MaxValueSize)+1))}
	}
	var published atomic.Int64

	dropOrder := make([]uint32, 0, sealedFiles)
	for _, i := range rng.Perm(sealedFiles) {
		dropOrder = append(dropOrder, uint32(i+1))
	}
	readerSeeds := make([]uint64, readers)
	for i := range readerSeeds {
		readerSeeds[i] = rng.Uint64()
	}

	start := make(chan struct{})
	var dropsDone atomic.Bool
	var hits, misses atomic.Int64
	var wg sync.WaitGroup

	wg.Go(func() { // writer
		<-start
		for i := range active {
			id, off, err := s.Append(active[i].frame)
			if !assert.NoError(t, err) || !assert.Equal(t, uint32(activeID), id) {
				return
			}
			active[i].off = off
			published.Store(int64(i + 1))
		}
	})
	wg.Go(func() { // dropper: one or two files per call
		defer dropsDone.Store(true)
		<-start
		for i := 0; i < len(dropOrder); {
			ids := dropOrder[i:min(i+1+i%2, len(dropOrder))]
			if !assert.NoError(t, s.Drop(ids)) {
				return
			}
			i += len(ids)
		}
	})
	for _, seed := range readerSeeds {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(seed, seed>>1))
			<-start
			for n := 0; n < minReads || !dropsDone.Load(); n++ {
				if k := published.Load(); k > 0 && r.IntN(2) == 0 {
					a := active[r.Int64N(k)]
					got, err := s.ReadAt(a.id, a.off, int64(len(a.frame)))
					if !assert.NoError(t, err) || !assert.Equal(t, a.frame, got) {
						return
					}
					continue
				}
				x := sealed[r.IntN(len(sealed))]
				got, err := s.ReadAt(x.id, x.off, int64(len(x.frame)))
				if err != nil {
					if !assert.ErrorIs(t, err, ErrSegmentNotFound) {
						return
					}
					misses.Add(1)
					continue
				}
				if !assert.Equal(t, x.frame, got) {
					return
				}
				hits.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	t.Logf("sealed reads: %d served, %d after drop", hits.Load(), misses.Load())

	// Final state: every sealed file is gone, every append is readable, and
	// the accounting matches the disk.
	for _, x := range sealed {
		_, err := s.ReadAt(x.id, x.off, int64(len(x.frame)))
		require.ErrorIs(t, err, ErrSegmentNotFound)
	}
	var end int64
	for _, a := range active {
		assert.Equal(t, end, a.off)
		end += int64(len(a.frame))
		got, err := s.ReadAt(a.id, a.off, int64(len(a.frame)))
		require.NoError(t, err)
		assert.Equal(t, a.frame, got)
	}
	assert.Equal(t, end, s.DiskBytes())
	assert.Equal(t, end, diskSize(t, segPath(dir, activeID)))
	assert.Equal(t, []string{SegmentName(activeID)}, listDir(t, dir))
}
