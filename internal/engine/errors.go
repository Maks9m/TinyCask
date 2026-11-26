package engine

import (
	"errors"
	"fmt"

	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
)

// ErrKeyNotFound reports a Get or Delete of a key that has no live value.
var ErrKeyNotFound = errors.New("engine: key not found")

// ErrInvalidKey reports an empty key.
var ErrInvalidKey = errors.New("engine: invalid key")

// ErrKeyTooLarge reports a key longer than Options.MaxKeySize.
var ErrKeyTooLarge = errors.New("engine: key too large")

// ErrValueTooLarge reports a value longer than Options.MaxValueSize.
var ErrValueTooLarge = errors.New("engine: value too large")

// ErrNoSpace reports a full device or quota. The write was rolled back; the
// engine stays writable.
var ErrNoSpace = errors.New("engine: no space left on device")

// ErrReadOnly reports a write after a failed truncate or fsync (invariant 8).
// Reads still work; only a restart makes the engine writable again.
var ErrReadOnly = errors.New("engine: read-only")

// ErrClosed reports a call after Close.
var ErrClosed = errors.New("engine: closed")

// ErrCorrupt reports a record that failed validation (CRC, header or key).
var ErrCorrupt = errors.New("engine: corrupt data")

// ErrLocked reports that another engine holds the data directory's LOCK.
var ErrLocked = errors.New("engine: data directory is locked")

// ErrCompactionRunning reports a compaction request while one is in progress.
var ErrCompactionRunning = errors.New("engine: compaction already running")

// translate maps lower-layer sentinels to engine ones (ARCHITECTURE §8),
// keeping the original in the chain.
func translate(err error) error {
	if err == nil {
		return nil
	}
	for _, m := range errorMap() {
		if errors.Is(err, m.from) {
			return fmt.Errorf("%w: %w", m.to, err)
		}
	}
	return err
}

type errorMapping struct{ from, to error }

// errorMap is a function, not a var, to avoid mutable package state.
// Order matters: a damaged segment wins over the I/O error that caused it.
func errorMap() []errorMapping {
	return []errorMapping{
		{storage.ErrSegmentDamaged, ErrReadOnly},
		{storage.ErrStoreClosed, ErrClosed},
		{vfs.ErrNoSpace, ErrNoSpace},
		{vfs.ErrLocked, ErrLocked},
		{storage.ErrCorrupt, ErrCorrupt},
	}
}
