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

// benchKeySet returns distinct random keys from a fixed seed, so runs compare.
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

// heapAlloc returns live heap bytes after a full GC (HeapAlloc).
func heapAlloc() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// BenchmarkKeyDir_Insert1M inserts 1M keys and reports heap-B/key, to compare
// with the design doc's 100–150 MB per million keys (§4.3). No assertion.
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
