package storage

import (
	"encoding/binary"
	"hash/crc32"
	"strconv"
)

// Record format v1 (ARCHITECTURE §3.1), frozen:
//
//	| CRC32 4B | Timestamp 8B | Type 1B | KeySize 4B | ValSize 4B | Key | Value |
//
// Integers are little-endian. The CRC (IEEE) covers every byte after itself.
const (
	// HeaderSize is the fixed length of a record header in bytes.
	HeaderSize = 21

	// DefaultMaxKeySize is the default upper bound on key length (1 KiB).
	DefaultMaxKeySize uint32 = 1 << 10
	// DefaultMaxValueSize is the default upper bound on value length (1 MiB).
	DefaultMaxValueSize uint32 = 1 << 20

	// MinKeySize is the shortest key the format accepts. Empty keys are invalid.
	MinKeySize = 1
)

// Byte offsets of the header fields.
const (
	offCRC       = 0
	offTimestamp = offCRC + 4
	offType      = offTimestamp + 8
	offKeySize   = offType + 1
	offValueSize = offKeySize + 4
	offPayload   = offValueSize + 4
)

// The field offsets above must add up to HeaderSize; this fails to compile if
// either side drifts.
var _ = [1]struct{}{}[offPayload-HeaderSize]

// RecordType is the one-byte record kind stored in the header.
type RecordType uint8

const (
	// TypePut stores a value for a key.
	TypePut RecordType = 1
	// TypeDelete is a tombstone; its value size must be zero.
	TypeDelete RecordType = 2
)

// Valid reports whether t is a record type known to format v1.
func (t RecordType) Valid() bool { return t == TypePut || t == TypeDelete }

// Limits bounds key and value sizes. They are checked when encoding and, on
// decode, before any length from disk is trusted. A zero field means the
// default. The fields are uint32 because the on-disk length fields are.
type Limits struct {
	MaxKeySize   uint32
	MaxValueSize uint32
}

// DefaultLimits returns the limits from ARCHITECTURE §3.1: key 1–1024 bytes,
// value 0–1 MiB.
func DefaultLimits() Limits {
	return Limits{MaxKeySize: DefaultMaxKeySize, MaxValueSize: DefaultMaxValueSize}
}

func (l Limits) withDefaults() Limits {
	if l.MaxKeySize == 0 {
		l.MaxKeySize = DefaultMaxKeySize
	}
	if l.MaxValueSize == 0 {
		l.MaxValueSize = DefaultMaxValueSize
	}
	return l
}

// MaxRecordSize is the largest frame these limits allow, header included.
func (l Limits) MaxRecordSize() int64 {
	l = l.withDefaults()
	return frameSize(int64(l.MaxKeySize), int64(l.MaxValueSize))
}

// check applies the type and size rules shared by Encode and Decode, in the
// order Decode must apply them: type first, then the limits. An empty
// violation means the fields are acceptable.
func (l Limits) check(t RecordType, keySize, valueSize uint64) (violation, string) {
	u := func(n uint64) string { return strconv.FormatUint(n, 10) }
	switch {
	case !t.Valid():
		return violUnknownType, "type " + u(uint64(t))
	case keySize < MinKeySize:
		return violEmptyKey, "key size 0"
	case keySize > uint64(l.MaxKeySize):
		return violKeyTooLarge, "key size " + u(keySize) + " > " + u(uint64(l.MaxKeySize))
	case valueSize > uint64(l.MaxValueSize):
		return violValueTooLarge, "value size " + u(valueSize) + " > " + u(uint64(l.MaxValueSize))
	case t == TypeDelete && valueSize != 0:
		return violTombstoneValue, "value size " + u(valueSize)
	}
	return "", ""
}

// Record is one decoded log entry.
type Record struct {
	// Timestamp is Unix nanoseconds. It is informational only; ordering is
	// always (FileID, offset).
	Timestamp uint64
	Type      RecordType
	Key       []byte
	// Value is empty for tombstones and for PUTs of an empty value.
	Value []byte
}

// Header is the fixed-size prefix of a record, decoded and validated.
type Header struct {
	CRC       uint32
	Timestamp uint64
	Type      RecordType
	KeySize   uint32
	ValueSize uint32
}

// RecordSize is the full frame length this header describes.
func (h Header) RecordSize() int64 {
	return frameSize(int64(h.KeySize), int64(h.ValueSize))
}

// RecordSize returns the frame length of a record with a key of keyLen bytes
// and a value of valueSize bytes: HeaderSize + keyLen + valueSize.
func RecordSize(keyLen int, valueSize uint32) int64 {
	return frameSize(int64(keyLen), int64(valueSize))
}

// RecordOffset returns where a record starts, given the offset of its value
// and its key length: valueOffset - HeaderSize - keyLen.
func RecordOffset(valueOffset int64, keyLen int) int64 {
	return valueOffset - HeaderSize - int64(keyLen)
}

// ValueOffset returns where a record's value starts, given the offset of the
// record and its key length. It is the inverse of RecordOffset.
func ValueOffset(recordOffset int64, keyLen int) int64 {
	return recordOffset + HeaderSize + int64(keyLen)
}

func frameSize(keyLen, valueSize int64) int64 {
	return HeaderSize + keyLen + valueSize
}

// Encode validates r against lim and returns its complete frame in a single
// newly allocated buffer, ready for one Write call. A rule violation returns
// an error wrapping ErrInvalidRecord and nothing is allocated.
func Encode(r Record, lim Limits) ([]byte, error) {
	lim = lim.withDefaults()
	keySize, valueSize := uint64(len(r.Key)), uint64(len(r.Value))
	if rule, detail := lim.check(r.Type, keySize, valueSize); rule != "" {
		return nil, invalid(rule, "%s", detail)
	}

	// Both sizes fit in uint32: check bounded them by uint32 limits.
	buf := make([]byte, RecordSize(len(r.Key), uint32(valueSize)))
	binary.LittleEndian.PutUint64(buf[offTimestamp:], r.Timestamp)
	buf[offType] = byte(r.Type)
	binary.LittleEndian.PutUint32(buf[offKeySize:], uint32(keySize))
	binary.LittleEndian.PutUint32(buf[offValueSize:], uint32(valueSize))
	n := copy(buf[offPayload:], r.Key)
	copy(buf[offPayload+n:], r.Value)
	binary.LittleEndian.PutUint32(buf[offCRC:], crc32.ChecksumIEEE(buf[offTimestamp:]))
	return buf, nil
}

// DecodeHeader parses and validates the first HeaderSize bytes of buf: length,
// then type, then the size limits. It never looks past the header, so a reader
// can call it before deciding how many payload bytes to read. Every failure
// wraps ErrCorrupt.
func DecodeHeader(buf []byte, lim Limits) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, corrupt(violShortHeader, "have %d of %d bytes", len(buf), HeaderSize)
	}
	h := Header{
		CRC:       binary.LittleEndian.Uint32(buf[offCRC:]),
		Timestamp: binary.LittleEndian.Uint64(buf[offTimestamp:]),
		Type:      RecordType(buf[offType]),
		KeySize:   binary.LittleEndian.Uint32(buf[offKeySize:]),
		ValueSize: binary.LittleEndian.Uint32(buf[offValueSize:]),
	}
	if rule, detail := lim.withDefaults().check(h.Type, uint64(h.KeySize), uint64(h.ValueSize)); rule != "" {
		return Header{}, corrupt(rule, "%s", detail)
	}
	return h, nil
}

// Decode validates buf as exactly one record, in the order header → type →
// limits → payload length → CRC, and returns it. buf must hold the whole frame
// and nothing else; a short or overlong buffer is corruption.
//
// Decode does not allocate: Key and Value alias buf (each capped so that
// appending to one cannot overwrite the other). Copy them before reusing buf.
// Every failure wraps ErrCorrupt.
func Decode(buf []byte, lim Limits) (Record, error) {
	h, err := DecodeHeader(buf, lim)
	if err != nil {
		return Record{}, err
	}
	size := h.RecordSize()
	switch have := int64(len(buf)); {
	case have < size:
		return Record{}, corrupt(violShortPayload, "have %d of %d bytes", have, size)
	case have > size:
		return Record{}, corrupt(violTrailingBytes, "have %d bytes, record is %d", have, size)
	}
	if sum := crc32.ChecksumIEEE(buf[offTimestamp:]); sum != h.CRC {
		return Record{}, corrupt(violChecksum, "stored 0x%08x, computed 0x%08x", h.CRC, sum)
	}
	keyEnd := offPayload + int(h.KeySize)
	return Record{
		Timestamp: h.Timestamp,
		Type:      h.Type,
		Key:       buf[offPayload:keyEnd:keyEnd],
		Value:     buf[keyEnd:len(buf):len(buf)],
	}, nil
}
