package storage

import (
	"embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"math"
	"path"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenFiles is embedded because os is forbidden here (invariant 1).
//
//go:embed testdata/*.hex
var goldenFiles embed.FS

// Samples pinned by testdata/ golden files. Shared by parallel tests: read-only.
var (
	samplePut = Record{Timestamp: 1700000000123456789, Type: TypePut, Key: []byte("user"), Value: []byte("data")}
	sampleDel = Record{Timestamp: 1700000000987654321, Type: TypeDelete, Key: []byte("user")}
)

// requireRule asserts that err wraps sentinel and was raised by the given rule.
func requireRule(tb testing.TB, err error, sentinel error, rule violation) {
	tb.Helper()
	require.ErrorIs(tb, err, sentinel)
	var re *recordError
	require.ErrorAs(tb, err, &re)
	assert.Equal(tb, rule, re.rule, "error: %v", err)
	// The ticket requires the reason to be visible to whoever reads the log.
	assert.Contains(tb, err.Error(), string(rule))
}

// rawFrame builds a frame by hand, bypassing Encode, to make invalid frames.
// fixCRC sets a correct CRC, proving a non-CRC check caught the problem.
func rawFrame(typ byte, keySize, valueSize uint32, payload []byte, fixCRC bool) []byte {
	buf := make([]byte, HeaderSize, HeaderSize+len(payload))
	binary.LittleEndian.PutUint64(buf[4:], 42)
	buf[12] = typ
	binary.LittleEndian.PutUint32(buf[13:], keySize)
	binary.LittleEndian.PutUint32(buf[17:], valueSize)
	buf = append(buf, payload...)
	if fixCRC {
		binary.LittleEndian.PutUint32(buf[0:], crc32.ChecksumIEEE(buf[4:]))
	}
	return buf
}

// heapBytes reports heap bytes f allocates. Don't call from parallel tests.
func heapBytes(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestRecord_RoundTrip(t *testing.T) {
	t.Parallel()
	small := Limits{MaxKeySize: 8, MaxValueSize: 16}
	tests := []struct {
		name string
		rec  Record
		lim  Limits
	}{
		{"put", samplePut, DefaultLimits()},
		{"put empty value", Record{Timestamp: 7, Type: TypePut, Key: []byte("k")}, DefaultLimits()},
		{"delete", sampleDel, DefaultLimits()},
		{"zero timestamp", Record{Type: TypePut, Key: []byte("k"), Value: []byte("v")}, DefaultLimits()},
		{"max timestamp", Record{Timestamp: math.MaxUint64, Type: TypePut, Key: []byte("k"), Value: []byte("v")}, DefaultLimits()},
		{"binary key and value", Record{Type: TypePut, Key: []byte{0, 0xff, '\r', '\n'}, Value: []byte{0, 1, 2}}, DefaultLimits()},
		{"key and value at limit", Record{Type: TypePut, Key: []byte(strings.Repeat("k", 8)), Value: []byte(strings.Repeat("v", 16))}, small},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			buf, err := Encode(tc.rec, tc.lim)
			require.NoError(t, err)
			assert.Equal(t, RecordSize(len(tc.rec.Key), uint32(len(tc.rec.Value))), int64(len(buf)))

			got, err := Decode(buf, tc.lim)
			require.NoError(t, err)
			assert.Equal(t, tc.rec.Timestamp, got.Timestamp)
			assert.Equal(t, tc.rec.Type, got.Type)
			assert.Equal(t, tc.rec.Key, got.Key)
			assert.Len(t, got.Value, len(tc.rec.Value))
			if len(tc.rec.Value) > 0 {
				assert.Equal(t, tc.rec.Value, got.Value)
			}
		})
	}
}

func TestDecode_DetectsEverySingleBitFlip(t *testing.T) {
	t.Parallel()
	for _, rec := range []Record{samplePut, sampleDel} {
		orig, err := Encode(rec, DefaultLimits())
		require.NoError(t, err)
		for bit := range len(orig) * 8 {
			buf := append([]byte(nil), orig...)
			buf[bit/8] ^= 1 << (bit % 8)
			_, err := Decode(buf, DefaultLimits())
			require.ErrorIs(t, err, ErrCorrupt, "type %d: flipped bit %d went undetected", rec.Type, bit)
		}
	}
}

func TestEncode_RejectsInvalidRecords(t *testing.T) {
	t.Parallel()
	lim := Limits{MaxKeySize: 4, MaxValueSize: 8}
	tests := []struct {
		name string
		rec  Record
		rule violation
	}{
		{"nil key", Record{Type: TypePut}, violEmptyKey},
		{"empty key", Record{Type: TypePut, Key: []byte{}, Value: []byte("v")}, violEmptyKey},
		{"oversized key", Record{Type: TypePut, Key: []byte("12345")}, violKeyTooLarge},
		{"oversized value", Record{Type: TypePut, Key: []byte("k"), Value: []byte("123456789")}, violValueTooLarge},
		{"tombstone with value", Record{Type: TypeDelete, Key: []byte("k"), Value: []byte("v")}, violTombstoneValue},
		{"type zero", Record{Type: 0, Key: []byte("k")}, violUnknownType},
		{"type three", Record{Type: 3, Key: []byte("k")}, violUnknownType},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			buf, err := Encode(tc.rec, lim)
			requireRule(t, err, ErrInvalidRecord, tc.rule)
			assert.NotErrorIs(t, err, ErrCorrupt)
			assert.Nil(t, buf)
		})
	}
}

func TestDecode_RejectsCorruptRecords(t *testing.T) {
	t.Parallel()
	lim := Limits{MaxKeySize: 4, MaxValueSize: 8}
	good, err := Encode(Record{Type: TypePut, Key: []byte("k"), Value: []byte("v")}, lim)
	require.NoError(t, err)
	badCRC := append([]byte(nil), good...)
	badCRC[0] ^= 0xff

	tests := []struct {
		name string
		buf  []byte
		rule violation
	}{
		{"nil buffer", nil, violShortHeader},
		{"header one byte short", good[:HeaderSize-1], violShortHeader},
		{"type zero", rawFrame(0, 1, 0, []byte("k"), true), violUnknownType},
		{"type 0xff", rawFrame(0xff, 1, 0, []byte("k"), true), violUnknownType},
		{"empty key", rawFrame(1, 0, 1, []byte("v"), true), violEmptyKey},
		{"oversized key", rawFrame(1, 5, 0, []byte("12345"), true), violKeyTooLarge},
		{"oversized value", rawFrame(1, 1, 9, []byte("k123456789"), true), violValueTooLarge},
		{"key size 4 GiB", rawFrame(1, math.MaxUint32, 0, nil, false), violKeyTooLarge},
		{"value size 4 GiB", rawFrame(1, 1, math.MaxUint32, []byte("k"), false), violValueTooLarge},
		{"tombstone with value", rawFrame(2, 1, 1, []byte("kv"), true), violTombstoneValue},
		{"payload cut short", good[:len(good)-1], violShortPayload},
		{"header only", good[:HeaderSize], violShortPayload},
		{"trailing byte", append(append([]byte(nil), good...), 0), violTrailingBytes},
		{"crc mismatch", badCRC, violChecksum},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, err := Decode(tc.buf, lim)
			requireRule(t, err, ErrCorrupt, tc.rule)
			assert.Equal(t, Record{}, rec)
		})
	}
}

// Each case breaks several rules; the earliest (header → type → limits →
// payload → CRC) must be reported.
func TestDecode_ValidationOrder(t *testing.T) {
	t.Parallel()
	lim := Limits{MaxKeySize: 4, MaxValueSize: 8}
	tests := []struct {
		name string
		buf  []byte
		rule violation
	}{
		{"short header before everything", []byte{0xff, 0xff, 0xff}, violShortHeader},
		{"type before limits, length and crc", rawFrame(9, math.MaxUint32, math.MaxUint32, nil, false), violUnknownType},
		{"limits before length and crc", rawFrame(1, 1, math.MaxUint32, []byte("k"), false), violValueTooLarge},
		{"length before crc", rawFrame(1, 2, 2, []byte("k"), false), violShortPayload},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Decode(tc.buf, lim)
			requireRule(t, err, ErrCorrupt, tc.rule)
		})
	}
}

func TestDecodeHeader_ValidatesWithoutPayload(t *testing.T) {
	t.Parallel()
	buf, err := Encode(samplePut, DefaultLimits())
	require.NoError(t, err)

	h, err := DecodeHeader(buf[:HeaderSize], DefaultLimits())
	require.NoError(t, err)
	assert.Equal(t, Header{
		CRC:       binary.LittleEndian.Uint32(buf),
		Timestamp: samplePut.Timestamp,
		Type:      TypePut,
		KeySize:   4,
		ValueSize: 4,
	}, h)
	assert.Equal(t, int64(len(buf)), h.RecordSize())

	_, err = DecodeHeader(rawFrame(1, 1, math.MaxUint32, nil, false), DefaultLimits())
	requireRule(t, err, ErrCorrupt, violValueTooLarge)
}

func TestLimits_ZeroFieldsMeanDefaults(t *testing.T) {
	t.Parallel()
	assert.Equal(t, DefaultLimits(), Limits{}.withDefaults())
	assert.Equal(t, Limits{MaxKeySize: 3, MaxValueSize: DefaultMaxValueSize}, Limits{MaxKeySize: 3}.withDefaults())
	assert.Equal(t, int64(HeaderSize)+int64(DefaultMaxKeySize)+int64(DefaultMaxValueSize), Limits{}.MaxRecordSize())

	key := []byte(strings.Repeat("k", int(DefaultMaxKeySize)))
	buf, err := Encode(Record{Type: TypePut, Key: key}, Limits{})
	require.NoError(t, err)
	_, err = Decode(buf, Limits{})
	require.NoError(t, err)

	_, err = Decode(buf, Limits{MaxKeySize: DefaultMaxKeySize - 1})
	requireRule(t, err, ErrCorrupt, violKeyTooLarge)
}

func TestRecord_GeometryHelpers(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 21, HeaderSize)
	assert.Equal(t, int64(21+4+4), RecordSize(4, 4))
	assert.Equal(t, int64(21+1), RecordSize(1, 0))

	// A record written at offset 100 with a 4-byte key has its value at 125.
	assert.Equal(t, int64(125), ValueOffset(100, 4))
	assert.Equal(t, int64(100), RecordOffset(125, 4))

	buf, err := Encode(samplePut, DefaultLimits())
	require.NoError(t, err)
	const at = int64(4096)
	vo := ValueOffset(at, len(samplePut.Key))
	assert.Equal(t, samplePut.Value, buf[vo-at:], "value offset must point at the value bytes")
	assert.Equal(t, at, RecordOffset(vo, len(samplePut.Key)))
}

func TestDecode_AliasesInputWithCappedSlices(t *testing.T) {
	t.Parallel()
	buf, err := Encode(samplePut, DefaultLimits())
	require.NoError(t, err)
	rec, err := Decode(buf, DefaultLimits())
	require.NoError(t, err)

	assert.Same(t, &buf[HeaderSize], &rec.Key[0])
	assert.Same(t, &buf[HeaderSize+len(rec.Key)], &rec.Value[0])
	// Appending to the key must not overwrite the value that follows it.
	rec.Key = append(rec.Key, 'X')
	assert.Equal(t, "userX", string(rec.Key))
	assert.Equal(t, samplePut.Value, rec.Value)
	assert.Equal(t, samplePut.Value, buf[HeaderSize+len(samplePut.Key):])
}

// Not parallel: heapBytes measures process-wide allocation.
func TestDecode_DoesNotAllocateFromClaimedLengths(t *testing.T) {
	valid, err := Encode(samplePut, DefaultLimits())
	require.NoError(t, err)
	huge := rawFrame(1, math.MaxUint32, math.MaxUint32, nil, false)
	short := rawFrame(1, 4, uint32(DefaultMaxValueSize), []byte("user"), false)

	const runs = 100
	tests := []struct {
		name     string
		buf      []byte
		wantFail bool
	}{
		{"valid", valid, false},
		{"4 GiB lengths", huge, true},
		{"1 MiB claimed, absent", short, true},
	}
	for _, tc := range tests {
		failures := 0
		perOp := heapBytes(func() {
			for range runs {
				if _, err := Decode(tc.buf, DefaultLimits()); err != nil {
					failures++
				}
			}
		}) / runs
		assert.Less(t, perOp, uint64(512), "%s: Decode allocated %d bytes per call", tc.name, perOp)
		if tc.wantFail {
			assert.Equal(t, runs, failures, tc.name)
		} else {
			assert.Zero(t, failures, tc.name)
		}
	}
}

// readGolden parses annotated hex: '#' comments, whitespace ignored.
func readGolden(tb testing.TB, name string) []byte {
	tb.Helper()
	raw, err := goldenFiles.ReadFile(path.Join("testdata", name))
	require.NoError(tb, err)
	var digits strings.Builder
	for line := range strings.Lines(string(raw)) {
		line, _, _ = strings.Cut(line, "#")
		digits.WriteString(strings.Join(strings.Fields(line), ""))
	}
	b, err := hex.DecodeString(digits.String())
	require.NoError(tb, err)
	return b
}

func TestRecord_GoldenV1(t *testing.T) {
	t.Parallel()
	for file, rec := range map[string]Record{"record_v1_put.hex": samplePut, "record_v1_delete.hex": sampleDel} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			golden := readGolden(t, file)

			enc, err := Encode(rec, DefaultLimits())
			require.NoError(t, err)
			assert.Equal(t, golden, enc, "Encode no longer produces format v1 bytes")

			dec, err := Decode(golden, DefaultLimits())
			require.NoError(t, err)
			assert.Equal(t, rec.Timestamp, dec.Timestamp)
			assert.Equal(t, rec.Type, dec.Type)
			assert.Equal(t, rec.Key, dec.Key)
			assert.Equal(t, string(rec.Value), string(dec.Value))
		})
	}
}

func TestRecordType_Valid(t *testing.T) {
	t.Parallel()
	for v := range 256 {
		typ := RecordType(v)
		assert.Equal(t, typ == 1 || typ == 2, typ.Valid(), "type %d", v)
	}
	assert.False(t, errors.Is(ErrCorrupt, ErrInvalidRecord), "the two sentinels must stay distinct")
}
