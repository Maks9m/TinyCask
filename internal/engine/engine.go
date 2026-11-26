package engine

import (
	"bytes"
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
	// lockFileName is the advisory lock held for the engine's lifetime.
	lockFileName = "LOCK"
	// firstSegmentID is the active segment's ID in an empty directory.
	firstSegmentID uint32 = 1
	// maxGetAttempts bounds Get's retries when a compaction drops the file
	// between the KeyDir lookup and the read (ARCHITECTURE §5.2).
	maxGetAttempts = 3
)

// Engine is the embedded key-value store (ARCHITECTURE §10). All methods are
// safe for concurrent use. It never retains the caller's key or value slices.
type Engine struct {
	dir    string
	opts   Options
	limits storage.Limits
	lock   io.Closer
	store  *storage.Store
	keydir *index.KeyDir

	closed   atomic.Bool // one-way; set under mu
	readOnly atomic.Bool // one-way (invariant 8)

	// liveBytes is the total size of the records KeyDir points at; it moves
	// only next to a KeyDir change. diskBytes is the total size of all data
	// files; it moves only next to an append. Both are written under mu.
	liveBytes atomic.Int64
	diskBytes atomic.Int64

	// mu is the writer lock (invariant 2). It covers each append together
	// with its KeyDir update and counters, rolls, and Close. Lock order:
	// mu → KeyDir → Store (invariant 3).
	mu sync.Mutex
}

// Open opens the engine on dir, creating the directory if needed. It takes
// the directory's LOCK (ErrLocked if another engine holds it) and creates the
// first active segment.
//
// Replaying existing data is T2.3: a directory that already holds data files
// is rejected.
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
	if err = e.start(fsys); err != nil {
		return nil, errors.Join(err, e.store.Close(), e.releaseLock())
	}
	return e, nil
}

// start checks that dir holds no data files and creates the first active
// segment. Called by Open before the engine is shared.
func (e *Engine) start(fsys vfs.FS) error {
	names, err := fsys.List(e.dir)
	if err != nil {
		return fmt.Errorf("engine: list %s: %w", e.dir, err)
	}
	for _, base := range names {
		_, isData, perr := storage.ParseFileName(base)
		if perr != nil {
			return fmt.Errorf("engine: open %s: %w", e.dir, perr)
		}
		if isData {
			return fmt.Errorf("engine: open %s (found %s): %w", e.dir, base, errRecoveryNotImplemented)
		}
	}
	if err = e.store.CreateActive(firstSegmentID); err != nil {
		return translate(fmt.Errorf("engine: create first segment: %w", err))
	}
	return nil
}

// Put stores value under key (ARCHITECTURE §5.1).
func (e *Engine) Put(key, value []byte) error {
	// Checked first so a closed engine reports ErrClosed for any input;
	// writableLocked re-checks under the lock to cover a racing Close.
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
	// The codec bounded len(value) by MaxValueSize, a uint32.
	valueSize := uint32(len(value))
	prev, existed := e.keydir.Put(key, index.CommandIndex{
		FileID:      fileID,
		ValueSize:   valueSize,
		ValueOffset: storage.ValueOffset(off, len(key)),
		Timestamp:   ts,
	})
	e.liveBytes.Add(storage.RecordSize(len(key), valueSize) - e.entrySize(key, prev, existed))
	return nil
}

// Get returns a copy of the value stored under key, or ErrKeyNotFound
// (ARCHITECTURE §5.2). It never takes the writer lock.
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

// readValue reads and verifies the record entry points at, with one ReadAt
// and no KeyDir lock held.
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
	// Clone so the caller owns exactly the value, not the whole frame.
	return bytes.Clone(rec.Value), nil
}

// Delete removes key by appending a tombstone (ARCHITECTURE §5.3). A missing
// key returns ErrKeyNotFound and writes nothing.
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
	// Checked under the writer lock, so no Put or Delete can slip in between
	// this lookup and the tombstone.
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

// Close syncs the active segment, closes every file and releases LOCK
// (ARCHITECTURE §5.6). It is idempotent; every other method then returns
// ErrClosed. The engine is closed even if Close returns an error.
func (e *Engine) Close() error {
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

// appendLocked rolls if needed and appends frame, keeping diskBytes next to
// the append. e.mu must be held.
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
	e.diskBytes.Add(int64(len(frame)))
	return fileID, off, nil
}

// maybeRollLocked seals the active segment if n more bytes would push it past
// MaxSegmentSize. An empty segment is never rolled, so a record larger than
// the threshold lands alone in a fresh segment. e.mu must be held.
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

// writableLocked returns why the engine cannot take a write, or nil.
// e.mu must be held, so the flags cannot change underneath a write.
func (e *Engine) writableLocked() error {
	if e.closed.Load() {
		return ErrClosed
	}
	if e.readOnly.Load() {
		return ErrReadOnly
	}
	return nil
}

// fail switches to read-only if the Store reports a damaged segment
// (invariant 8) and translates err. e.mu must be held.
func (e *Engine) fail(err error) error {
	if errors.Is(err, storage.ErrSegmentDamaged) {
		e.readOnly.Store(true)
	}
	return translate(err)
}

// checkKey enforces the key limits before any work is done.
func (e *Engine) checkKey(key []byte) error {
	switch {
	case len(key) < storage.MinKeySize:
		return ErrInvalidKey
	case uint64(len(key)) > uint64(e.limits.MaxKeySize):
		return fmt.Errorf("%w: %d bytes, limit %d", ErrKeyTooLarge, len(key), e.limits.MaxKeySize)
	}
	return nil
}

// encode builds the frame outside the writer lock. Sizes were checked
// already, so a codec error here is a bug, reported rather than hidden.
func (e *Engine) encode(r storage.Record) ([]byte, error) {
	frame, err := storage.Encode(r, e.limits)
	if err != nil {
		return nil, fmt.Errorf("engine: encode: %w", err)
	}
	return frame, nil
}

// entrySize is the record size of a KeyDir entry, or 0 if there was none.
func (e *Engine) entrySize(key []byte, entry index.CommandIndex, ok bool) int64 {
	if !ok {
		return 0
	}
	return storage.RecordSize(len(key), entry.ValueSize)
}

// timestamp returns the clock as Unix nanoseconds. It is informational only
// (invariant 4), so a pre-1970 clock wrapping around is harmless.
func (e *Engine) timestamp() uint64 {
	return uint64(e.opts.Now().UnixNano())
}

func (e *Engine) releaseLock() error {
	if err := e.lock.Close(); err != nil {
		return fmt.Errorf("engine: release lock: %w", err)
	}
	return nil
}
