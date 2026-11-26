package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Maks9m/TinyCask/internal/index"
	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
)

const (
	lockFileName          = "LOCK" // held for the engine's lifetime
	firstSegmentID uint32 = 1      // active segment ID in an empty dir
	// maxGetAttempts bounds Get retries when a compaction drops the file
	// between lookup and read (ARCHITECTURE §5.2).
	maxGetAttempts = 3
)

// Engine is the embedded key-value store (ARCHITECTURE §10). Safe for
// concurrent use; never retains the caller's slices.
type Engine struct {
	dir    string
	opts   Options
	limits storage.Limits
	lock   io.Closer
	store  *storage.Store
	keydir *index.KeyDir

	closed   atomic.Bool // one-way; set under mu
	readOnly atomic.Bool // one-way (invariant 8)

	// liveBytes: size of records KeyDir points at (changes with KeyDir).
	// diskBytes: size of all data files (changes with appends). Written under mu.
	liveBytes atomic.Int64
	diskBytes atomic.Int64

	// bgCancel stops the sync ticker goroutine; bg waits for it (invariant 11).
	// Set by Open, used only by Close.
	bgCancel context.CancelFunc
	bg       sync.WaitGroup

	// mu is the writer lock (invariant 2): append + KeyDir update + counters,
	// rolls, read-only switch, Close. Order: mu → KeyDir → Store (invariant 3).
	mu sync.Mutex
}

// Open opens the engine on dir (created if needed): takes LOCK (ErrLocked if
// held), replays data files into KeyDir (ARCHITECTURE §5.4) and creates a new
// active segment.
//
// Corruption outside the highest-ID file fails with ErrCorrupt and changes
// nothing; a torn tail in the highest-ID file is cut and synced.
func Open(fsys vfs.FS, dir string, opts Options) (*Engine, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	if err = fsys.MkdirAll(dir); err != nil {
		return nil, fmt.Errorf("engine: create directory %s: %w", dir, err)
	}
	lock, err := fsys.Lock(filepath.Join(dir, lockFileName))
	if err != nil {
		return nil, translate(fmt.Errorf("engine: lock %s: %w", dir, err))
	}
	e := &Engine{
		dir:    dir,
		opts:   opts,
		limits: opts.limits(),
		lock:   lock,
		store:  storage.NewStore(fsys, dir, storage.StoreOptions{Limits: opts.limits()}),
		keydir: index.New(),
	}
	if err = e.recover(fsys); err != nil {
		return nil, errors.Join(err, e.store.Close(), e.releaseLock())
	}
	e.startBackground()
	return e, nil
}

// Put stores value under key (ARCHITECTURE §5.1).
func (e *Engine) Put(key, value []byte) error {
	// Fast path; writableLocked re-checks under the lock for a racing Close.
	if e.closed.Load() {
		return ErrClosed
	}
	if err := e.checkKey(key); err != nil {
		return err
	}
	if uint64(len(value)) > uint64(e.limits.MaxValueSize) {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrValueTooLarge, len(value), e.limits.MaxValueSize)
	}
	ts := e.timestamp()
	frame, err := e.encode(storage.Record{Timestamp: ts, Type: storage.TypePut, Key: key, Value: value})
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	fileID, off, err := e.appendLocked(frame)
	if err != nil {
		return err
	}
	valueSize := uint32(len(value)) // safe: bounded by MaxValueSize
	prev, existed := e.keydir.Put(key, index.CommandIndex{
		FileID:      fileID,
		ValueSize:   valueSize,
		ValueOffset: storage.ValueOffset(off, len(key)),
		Timestamp:   ts,
	})
	e.liveBytes.Add(storage.RecordSize(len(key), valueSize) - e.entrySize(key, prev, existed))
	return nil
}

// Get returns a copy of key's value or ErrKeyNotFound (ARCHITECTURE §5.2),
// without the writer lock.
func (e *Engine) Get(key []byte) ([]byte, error) {
	if e.closed.Load() {
		return nil, ErrClosed
	}
	if err := e.checkKey(key); err != nil {
		return nil, err
	}
	var lastErr error
	for range maxGetAttempts {
		entry, ok := e.keydir.Get(key)
		if !ok {
			return nil, ErrKeyNotFound
		}
		value, err := e.readValue(key, entry)
		if errors.Is(err, storage.ErrSegmentNotFound) {
			// A compaction dropped the file after the lookup: look up again.
			lastErr = err
			continue
		}
		return value, err
	}
	return nil, fmt.Errorf("engine: get: file kept disappearing after %d attempts: %w", maxGetAttempts, lastErr)
}

// readValue reads and verifies entry's record with one ReadAt, no KeyDir lock.
func (e *Engine) readValue(key []byte, entry index.CommandIndex) ([]byte, error) {
	off := storage.RecordOffset(entry.ValueOffset, len(key))
	buf, err := e.store.ReadAt(entry.FileID, off, storage.RecordSize(len(key), entry.ValueSize))
	if err != nil {
		if errors.Is(err, storage.ErrSegmentNotFound) {
			return nil, err
		}
		return nil, translate(fmt.Errorf("engine: get: %w", err))
	}
	rec, err := storage.Decode(buf, e.limits)
	if err != nil {
		return nil, translate(fmt.Errorf("engine: get: file %d offset %d: %w", entry.FileID, off, err))
	}
	if rec.Type != storage.TypePut || !bytes.Equal(rec.Key, key) {
		return nil, fmt.Errorf("%w: file %d offset %d holds a different record than KeyDir expects",
			ErrCorrupt, entry.FileID, off)
	}
	// Clone so the caller owns just the value, not the whole frame.
	return bytes.Clone(rec.Value), nil
}

// Delete appends a tombstone for key (ARCHITECTURE §5.3). A missing key gives
// ErrKeyNotFound and writes nothing.
func (e *Engine) Delete(key []byte) error {
	if e.closed.Load() {
		return ErrClosed
	}
	if err := e.checkKey(key); err != nil {
		return err
	}
	frame, err := e.encode(storage.Record{Timestamp: e.timestamp(), Type: storage.TypeDelete, Key: key})
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if err = e.writableLocked(); err != nil {
		return err
	}
	// Under the writer lock, so nothing slips in before the tombstone.
	if _, ok := e.keydir.Get(key); !ok {
		return ErrKeyNotFound
	}
	if _, _, err = e.appendLocked(frame); err != nil {
		return err
	}
	prev, existed := e.keydir.Delete(key)
	e.liveBytes.Add(-e.entrySize(key, prev, existed))
	return nil
}

// Close stops background work, syncs, closes files and releases LOCK
// (ARCHITECTURE §5.6). Idempotent; the engine is closed even on error, and
// other methods then return ErrClosed.
func (e *Engine) Close() error {
	// Before taking mu: a background sync may be waiting for it.
	e.stopBackground()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed.Swap(true) {
		return nil
	}
	var serr error
	if err := e.store.Close(); err != nil {
		serr = fmt.Errorf("engine: close: %w", err)
	}
	return errors.Join(serr, e.releaseLock())
}

// appendLocked rolls if needed, appends, updates diskBytes and applies
// SyncAlways. Failed truncate/fsync → read-only (invariant 8). On error the
// caller must not touch KeyDir. e.mu must be held.
func (e *Engine) appendLocked(frame []byte) (fileID uint32, off int64, err error) {
	if err = e.writableLocked(); err != nil {
		return 0, 0, err
	}
	if err = e.maybeRollLocked(int64(len(frame))); err != nil {
		return 0, 0, e.fail(fmt.Errorf("engine: roll: %w", err))
	}
	fileID, off, err = e.store.Append(frame)
	if err != nil {
		return 0, 0, e.fail(fmt.Errorf("engine: append: %w", err))
	}
	// The frame is on file even if the fsync fails, so a failed Put may
	// still reappear after recovery.
	e.diskBytes.Add(int64(len(frame)))
	if err = e.syncAfterAppendLocked(); err != nil {
		return 0, 0, err
	}
	return fileID, off, nil
}

// maybeRollLocked rolls if n more bytes would pass MaxSegmentSize. Empty
// segments never roll, so an oversized record lands alone. e.mu must be held.
func (e *Engine) maybeRollLocked(n int64) error {
	id, size, ok := e.store.Active()
	if !ok {
		return storage.ErrNoActiveSegment
	}
	if size == 0 || size+n <= e.opts.MaxSegmentSize {
		return nil
	}
	if id == math.MaxUint32 {
		return fmt.Errorf("file IDs exhausted at %d", id)
	}
	return e.store.Roll(id + 1)
}

// writableLocked returns why writes are refused, or nil. e.mu must be held.
func (e *Engine) writableLocked() error {
	if e.closed.Load() {
		return ErrClosed
	}
	if e.readOnly.Load() {
		return ErrReadOnly
	}
	return nil
}

// fail goes read-only on a damaged segment (invariant 8) and translates err.
// e.mu must be held.
func (e *Engine) fail(err error) error {
	if errors.Is(err, storage.ErrSegmentDamaged) {
		e.readOnly.Store(true)
	}
	return translate(err)
}

func (e *Engine) checkKey(key []byte) error {
	switch {
	case len(key) < storage.MinKeySize:
		return ErrInvalidKey
	case uint64(len(key)) > uint64(e.limits.MaxKeySize):
		return fmt.Errorf("%w: %d bytes, limit %d", ErrKeyTooLarge, len(key), e.limits.MaxKeySize)
	}
	return nil
}

// encode builds the frame outside the writer lock. Sizes are pre-checked, so
// an error here is a bug.
func (e *Engine) encode(r storage.Record) ([]byte, error) {
	frame, err := storage.Encode(r, e.limits)
	if err != nil {
		return nil, fmt.Errorf("engine: encode: %w", err)
	}
	return frame, nil
}

// entrySize is an entry's record size, or 0 if !ok.
func (e *Engine) entrySize(key []byte, entry index.CommandIndex, ok bool) int64 {
	if !ok {
		return 0
	}
	return storage.RecordSize(len(key), entry.ValueSize)
}

// timestamp returns Unix ns. Informational only (invariant 4), so a pre-1970
// wraparound is harmless.
func (e *Engine) timestamp() uint64 {
	return uint64(e.opts.Now().UnixNano())
}

func (e *Engine) releaseLock() error {
	if err := e.lock.Close(); err != nil {
		return fmt.Errorf("engine: release lock: %w", err)
	}
	return nil
}
