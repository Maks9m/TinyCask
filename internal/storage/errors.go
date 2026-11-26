package storage

import (
	"errors"
	"fmt"
)

// ErrCorrupt reports bytes that are not a valid v1 record (bad header, type,
// length or CRC).
var ErrCorrupt = errors.New("storage: corrupt record")

// ErrInvalidRecord reports that Encode got a record breaking the format rules.
// It is a caller bug: the engine validates sizes first.
var ErrInvalidRecord = errors.New("storage: invalid record")

// ErrBadFileName reports a data-file name with a malformed ID. TinyCask never
// writes one, so the directory cannot be trusted (ARCHITECTURE §3.2).
var ErrBadFileName = errors.New("storage: malformed data file name")

// ErrSegmentNotFound reports a read from a file ID the Store does not hold.
// During Get it means a compaction won the race: redo the lookup
// (ARCHITECTURE §5.2).
var ErrSegmentNotFound = errors.New("storage: segment not found")

// ErrSegmentDamaged reports that a failed truncate or fsync left the active
// segment in an unknown state. No more appends; engine goes read-only
// (invariant 8).
var ErrSegmentDamaged = errors.New("storage: active segment damaged")

// ErrNoActiveSegment reports a write on a Store with no active segment yet.
var ErrNoActiveSegment = errors.New("storage: no active segment")

// ErrSegmentOrder reports a file ID that breaks the Store's ordering rules
// (invariant 4): duplicate, active not highest, sealed above active, or
// dropping the active segment.
var ErrSegmentOrder = errors.New("storage: file ID out of order")

// ErrStoreClosed reports a call on a Store after Close.
var ErrStoreClosed = errors.New("storage: store closed")

// violation names the rule a record breaks, shared by Encode and Decode.
type violation string

const (
	violShortHeader    violation = "short header"
	violUnknownType    violation = "unknown record type"
	violEmptyKey       violation = "empty key"
	violKeyTooLarge    violation = "key size over limit"
	violValueTooLarge  violation = "value size over limit"
	violTombstoneValue violation = "tombstone with non-zero value size"
	violShortPayload   violation = "short payload"
	violTrailingBytes  violation = "trailing bytes after record"
	violChecksum       violation = "checksum mismatch"
)

// recordError is behind ErrCorrupt and ErrInvalidRecord. It keeps the rule
// so tests can check which validation fired without comparing strings.
type recordError struct {
	sentinel error
	rule     violation
	detail   string
}

func (e *recordError) Error() string {
	return fmt.Sprintf("%v: %s (%s)", e.sentinel, e.rule, e.detail)
}

func (e *recordError) Unwrap() error { return e.sentinel }

func corrupt(rule violation, format string, args ...any) error {
	return &recordError{sentinel: ErrCorrupt, rule: rule, detail: fmt.Sprintf(format, args...)}
}

func invalid(rule violation, format string, args ...any) error {
	return &recordError{sentinel: ErrInvalidRecord, rule: rule, detail: fmt.Sprintf(format, args...)}
}
