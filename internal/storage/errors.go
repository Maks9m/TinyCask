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
