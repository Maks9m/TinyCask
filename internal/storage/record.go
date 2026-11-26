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
	// HeaderSize is the fixed header length in bytes.
	HeaderSize = 21

	// DefaultMaxKeySize is the default key limit (1 KiB).
	DefaultMaxKeySize uint32 = 1 << 10
	// DefaultMaxValueSize is the default value limit (1 MiB).
	DefaultMaxValueSize uint32 = 1 << 20

	// MinKeySize is the shortest valid key; empty keys are invalid.
	MinKeySize = 1
)

// Header field offsets.
const (
	offCRC       = 0
	offTimestamp = offCRC + 4
	offType      = offTimestamp + 8
	offKeySize   = offType + 1
	offValueSize = offKeySize + 4
	offPayload   = offValueSize + 4
)

// Compile-time check: the offsets must add up to HeaderSize.
var _ = [1]struct{}{}[offPayload-HeaderSize]

// RecordType is the one-byte record kind in the header.
type RecordType uint8

const (
	// TypePut stores a value for a key.
	TypePut RecordType = 1
	// TypeDelete is a tombstone; its value size must be zero.
	TypeDelete RecordType = 2
)

// Valid reports whether t is a known v1 type.
func (t RecordType) Valid() bool { return t == TypePut || t == TypeDelete }

// Limits bounds key and value sizes, checked on encode and before trusting any
// length on decode (invariant 10). Zero means the default.
type Limits struct {
	MaxKeySize   uint32
	MaxValueSize uint32
}

// DefaultLimits returns key 1–1024 B, value 0–1 MiB (ARCHITECTURE §3.1).
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

// MaxRecordSize is the largest frame allowed, header included.
func (l Limits) MaxRecordSize() int64 {
	l = l.withDefaults()
	return frameSize(int64(l.MaxKeySize), int64(l.MaxValueSize))
}

// check applies the rules shared by Encode and Decode: type, then limits.
// Empty violation means OK.
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
	Timestamp uint64 // Unix ns; informational, ordering is (FileID, offset)
	Type      RecordType
	Key       []byte
	Value     []byte // empty for tombstones
}

// Header is a decoded, validated record header.
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

// RecordSize returns HeaderSize + keyLen + valueSize.
func RecordSize(keyLen int, valueSize uint32) int64 {
	return frameSize(int64(keyLen), int64(valueSize))
}

// RecordOffset returns valueOffset - HeaderSize - keyLen: where the record
// starts, from the value offset the KeyDir stores.
func RecordOffset(valueOffset int64, keyLen int) int64 {
	return valueOffset - HeaderSize - int64(keyLen)
}

// ValueOffset is the inverse of RecordOffset.
func ValueOffset(recordOffset int64, keyLen int) int64 {
	return recordOffset + HeaderSize + int64(keyLen)
}

func frameSize(keyLen, valueSize int64) int64 {
	return HeaderSize + keyLen + valueSize
}

// Encode validates r and returns its whole frame in one buffer, so it can be
// written with one Write. Violations return ErrInvalidRecord.
func Encode(r Record, lim Limits) ([]byte, error) {
	lim = lim.withDefaults()
	keySize, valueSize := uint64(len(r.Key)), uint64(len(r.Value))
	if rule, detail := lim.check(r.Type, keySize, valueSize); rule != "" {
		return nil, invalid(rule, "%s", detail)
	}

	// The uint32 casts are safe: check bounded both sizes.
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

// DecodeHeader validates the first HeaderSize bytes (length, type, limits).
// It never reads past the header, so callers can size the payload read from
// it safely. Failures wrap ErrCorrupt.
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

// Decode validates buf as exactly one record (header → type → limits →
// payload length → CRC). Failures wrap ErrCorrupt.
//
// No allocation: Key and Value alias buf, capped so appending to one can't
// overwrite the other. Copy them before reusing buf.
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
