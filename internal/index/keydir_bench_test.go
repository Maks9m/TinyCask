package index

import (
	"encoding/binary"
	"math/rand/v2"
	"runtime"
	"testing"
)

const (
	benchKeys      = 1_000_000
	benchMinKeyLen = 16
	benchMaxKeyLen = 32
	benchIDBytes   = 8 // a unique big-endian counter prefix keeps keys distinct
)

// benchKeySet returns benchKeys distinct keys of benchMinKeyLen..benchMaxKeyLen
// bytes, derived from a fixed seed so runs are comparable.
func benchKeySet() [][]byte {
	r := rand.New(rand.NewPCG(1, 2))
	keys := make([][]byte, benchKeys)
	for i := range keys {
		k := make([]byte, benchMinKeyLen+r.IntN(benchMaxKeyLen-benchMinKeyLen+1))
		binary.BigEndian.PutUint64(k, uint64(i))
		for j := benchIDBytes; j < len(k); j++ {
			k[j] = byte(r.Uint32())
		}
		keys[i] = k
	}
	return keys
}

// heapAlloc returns the bytes held by live heap objects after a full GC
// (MemStats.HeapAlloc, not the span-level HeapInuse).
func heapAlloc() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// BenchmarkKeyDir_Insert1M inserts one million keys into an empty KeyDir and
// reports the live heap the KeyDir holds per key (heap-B/key), which is the
// number to compare with the design doc's 100–150 MB per million keys
// (design doc §4.3). It does not assert on it.
func BenchmarkKeyDir_Insert1M(b *testing.B) {
	keys := benchKeySet()
	var keyBytes int
	for _, k := range keys {
		keyBytes += len(k)
	}
	b.ReportAllocs()

	var perKey float64
	for b.Loop() {
		b.StopTimer()
		before := heapAlloc()
		b.StartTimer()

		kd := New()
		for i, k := range keys {
			kd.Put(k, CommandIndex{FileID: 1, ValueSize: 100, ValueOffset: int64(i) * 128, Timestamp: uint64(i)})
		}

		b.StopTimer()
		after := heapAlloc()
		if kd.Len() != benchKeys {
			b.Fatalf("Len = %d, want %d", kd.Len(), benchKeys)
		}
		runtime.KeepAlive(kd)
		perKey = float64(after-before) / benchKeys
		b.StartTimer()
	}
	b.ReportMetric(perKey, "heap-B/key")
	b.ReportMetric(float64(keyBytes)/benchKeys, "key-B/key")
}
