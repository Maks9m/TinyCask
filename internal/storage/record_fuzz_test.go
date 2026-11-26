package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// FuzzDecodeRecord feeds arbitrary bytes to Decode and checks three things:
// it never panics, it never allocates more than the limits allow (a lying
// length field must not turn into a huge allocation), and any input it accepts
// re-encodes to exactly the same bytes.
//
// heapBytes measures the whole process, so the bound is the largest legal
// frame rather than zero; a decoder that trusted a 4 GiB length would still
// blow far past it.
func FuzzDecodeRecord(f *testing.F) {
	lim := DefaultLimits()

	for _, file := range []string{"record_v1_put.hex", "record_v1_delete.hex"} {
		f.Add(readGolden(f, file))
	}
	for _, rec := range []Record{
		{Type: TypePut, Key: []byte("k")},
		{Timestamp: 1, Type: TypePut, Key: []byte("key"), Value: []byte("value")},
		{Timestamp: 2, Type: TypeDelete, Key: []byte{0}},
	} {
		buf, err := Encode(rec, lim)
		require.NoError(f, err)
		f.Add(buf)
		f.Add(buf[:len(buf)-1])
		f.Add(buf[:HeaderSize])
	}
	f.Add([]byte{})
	f.Add(rawFrame(1, 0xffffffff, 0xffffffff, nil, false))
	f.Add(rawFrame(2, 1, 1, []byte("kv"), true))

	f.Fuzz(func(t *testing.T, data []byte) {
		var (
			rec Record
			err error
		)
		allocated := heapBytes(func() { rec, err = Decode(data, lim) })
		require.LessOrEqual(t, allocated, uint64(lim.MaxRecordSize()),
			"Decode of %d bytes allocated %d bytes", len(data), allocated)

		if err != nil {
			require.ErrorIs(t, err, ErrCorrupt)
			return
		}
		require.LessOrEqual(t, len(rec.Key), int(lim.MaxKeySize))
		require.LessOrEqual(t, len(rec.Value), int(lim.MaxValueSize))

		again, err := Encode(rec, lim)
		require.NoError(t, err, "a decoded record must be encodable")
		require.Equal(t, data, again, "decode(encode(x)) must reproduce the input exactly")
	})
}
