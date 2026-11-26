package storage

import (
	"errors"
	"fmt"
)

// ErrCorrupt reports bytes that are not a valid v1 record: a short header, an
// unknown type, a length outside the limits, a short or overlong payload, or a
// CRC mismatch. Errors returned by Decode wrap it together with the reason.
var ErrCorrupt = errors.New("storage: corrupt record")

// ErrInvalidRecord reports that Encode was asked to write a record that breaks
// the format rules (empty or oversized key, oversized value, unknown type, or a
// tombstone with a value). It is a caller bug: the engine validates sizes first.
var ErrInvalidRecord = errors.New("storage: invalid record")

// ErrBadFileName reports a data-directory entry that has the prefix and suffix
// of a TinyCask data file but an ID that is not exactly 10 decimal digits or
// does not fit in a uint32. TinyCask never writes such a name, so the
// directory cannot be trusted (ARCHITECTURE §3.2).
var ErrBadFileName = errors.New("storage: malformed data file name")

// ErrSegmentNotFound reports a read from a file ID the Store does not hold:
// never registered, or dropped by a compaction. During Get it means the
// compaction won the race and the lookup must be redone (ARCHITECTURE §5.2).
var ErrSegmentNotFound = errors.New("storage: segment not found")

// ErrSegmentDamaged reports that the active segment is in an unknown state: an
// append failed and could not be truncated away, or an fsync failed. The Store
// refuses every later append and the engine must go read-only (invariant 8).
var ErrSegmentDamaged = errors.New("storage: active segment damaged")

// ErrNoActiveSegment reports an Append, Roll or SyncActive on a Store that has
// no active segment yet.
var ErrNoActiveSegment = errors.New("storage: no active segment")

// ErrSegmentOrder reports a file ID that would break the store's ordering
// rules (invariant 4): a duplicate ID, a new active segment whose ID is not
// above every file in the set, a sealed file registered at or above the active
// segment, or an attempt to drop the active segment.
var ErrSegmentOrder = errors.New("storage: file ID out of order")

// ErrStoreClosed reports a call on a Store after Close.
var ErrStoreClosed = errors.New("storage: store closed")

// violation names the rule a record breaks. The same rules guard Encode and
// Decode; only the sentinel differs (ErrInvalidRecord vs ErrCorrupt).
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

// recordError is the concrete error behind ErrCorrupt and ErrInvalidRecord. It
// keeps the violated rule machine-readable so tests can check which validation
// step fired without comparing error strings.
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
