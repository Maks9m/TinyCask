package storage

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// StoreOptions configures a Store. The zero value uses the defaults.
type StoreOptions struct {
	// Limits cap a single ReadAt at the largest allowed record.
	Limits Limits
}

func (o StoreOptions) withDefaults() StoreOptions {
	o.Limits = o.Limits.withDefaults()
	return o
}

// Store is the set of open data files in one directory, keyed by file ID: at
// most one active segment (takes appends) plus read-only sealed files.
//
// Concurrency (ARCHITECTURE §6):
//   - Append, CreateActive, Roll, Close: caller serializes (engine writer
//     lock). Store.mu protects only the set, not append order.
//   - ReadAt, Register, Drop, SyncActive, Active, DiskBytes: safe concurrently
//     with everything except Close.
//
// After an unrecoverable append or failed fsync the Store is damaged: writes
// fail with ErrSegmentDamaged (invariant 8); reads still work.
type Store struct {
	fsys vfs.FS
	dir  string
	opts StoreOptions

	damaged atomic.Bool // one-way

	// mu protects the set. RLock is held across ReadAt so a file can't be
	// closed under a reader; Lock only while editing the map, never during I/O.
	mu     sync.RWMutex
	segs   map[uint32]*segment
	active *segment // also in segs; nil until CreateActive
	closed bool
}

// NewStore returns an empty Store for dir without touching any files.
func NewStore(fsys vfs.FS, dir string, opts StoreOptions) *Store {
	return &Store{
		fsys: fsys,
		dir:  dir,
		opts: opts.withDefaults(),
		segs: make(map[uint32]*segment),
	}
}

// CreateActive creates seg_<id>.data as the active segment and syncs the
// directory. id must be above every file in the set and the file must not
// exist. A failed directory sync damages the Store.
func (s *Store) CreateActive(id uint32) error {
	if err := s.writable(); err != nil {
		return err
	}
	if err := s.checkAdd(id, addActive, nil); err != nil {
		return err
	}
	seg, err := createSegment(s.fsys, s.dir, FileName{Kind: KindSegment, ID: id})
	if err != nil {
		return err
	}
	if err := s.install(seg, addActive, nil); err != nil {
		return errors.Join(err, s.discard(seg))
	}
	return s.syncDir()
}

// Register opens an existing sealed file (old segment or compaction output)
// for reading. Its ID must be free and below the active segment's.
func (s *Store) Register(name FileName) error {
	if !name.Readable() {
		return fmt.Errorf("storage: register %s: kind %v holds no readable records", name, name.Kind)
	}
	if err := s.checkAdd(name.ID, addSealed, nil); err != nil {
		return err
	}
	seg, err := openSegment(s.fsys, s.dir, name)
	if err != nil {
		return err
	}
	if err := s.install(seg, addSealed, nil); err != nil {
		// seg was only read, so closing it cannot lose data.
		return errors.Join(err, seg.close())
	}
	return nil
}

// Append writes an encoded record to the active segment with one Write and
// returns where it landed. Caller serializes (see Store).
//
// A failed write is truncated away and the Store stays writable; if the
// truncate fails, the Store is damaged. vfs.ErrNoSpace stays in the chain.
func (s *Store) Append(frame []byte) (fileID uint32, offset int64, err error) {
	if err = s.writable(); err != nil {
		return 0, 0, err
	}
	seg, err := s.activeSegment()
	if err != nil {
		return 0, 0, err
	}
	off, err := seg.append(frame)
	if err != nil {
		if errors.Is(err, ErrSegmentDamaged) {
			s.damaged.Store(true)
		}
		return 0, 0, err
	}
	return seg.name.ID, off, nil
}

// ReadAt reads exactly n bytes at offset from fileID in one positional read;
// completed appends are visible at once. Unknown or dropped file:
// ErrSegmentNotFound. n out of 1..MaxRecordSize or past EOF: ErrCorrupt.
func (s *Store) ReadAt(fileID uint32, offset, n int64) ([]byte, error) {
	if maxN := s.opts.Limits.MaxRecordSize(); n < 1 || n > maxN {
		return nil, fmt.Errorf("%w: read of %d bytes from file %d: want 1..%d", ErrCorrupt, n, fileID, maxN)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	seg, ok := s.segs[fileID]
	if !ok {
		return nil, fmt.Errorf("%w: file %d", ErrSegmentNotFound, fileID)
	}
	return seg.readAt(offset, n)
}

// Roll seals the active segment and makes seg_<nextID>.data active: sync
// current, create next, install, sync dir (invariant 7). Caller serializes.
//
// A failed sync damages the Store. If only the create fails, the current
// segment stays active and writable.
func (s *Store) Roll(nextID uint32) error {
	if err := s.writable(); err != nil {
		return err
	}
	cur, err := s.activeSegment()
	if err != nil {
		return err
	}
	if err = s.checkAdd(nextID, addRoll, cur); err != nil {
		return err
	}
	if err = cur.sync(); err != nil {
		s.damaged.Store(true)
		return fmt.Errorf("%w: seal: %w", ErrSegmentDamaged, err)
	}
	next, err := createSegment(s.fsys, s.dir, FileName{Kind: KindSegment, ID: nextID})
	if err != nil {
		return err
	}
	if err = s.install(next, addRoll, cur); err != nil {
		return errors.Join(err, s.discard(next))
	}
	return s.syncDir()
}

// Drop removes sealed files from the set, then closes, deletes and syncs the
// dir. Leaving the set before closing means a concurrent ReadAt either
// finishes or gets ErrSegmentNotFound (ARCHITECTURE §6).
//
// All IDs are checked first; an unknown or active ID changes nothing. After
// that, close/remove errors are joined; a failed dir sync damages the Store.
func (s *Store) Drop(ids []uint32) error {
	dropped, err := s.detach(ids)
	if err != nil || len(dropped) == 0 {
		return err
	}
	var errs []error
	for _, seg := range dropped {
		errs = append(errs, seg.close())
		if err := s.fsys.Remove(seg.path); err != nil {
			errs = append(errs, fmt.Errorf("storage: remove %s: %w", seg.path, err))
		}
	}
	errs = append(errs, s.syncDir())
	return errors.Join(errs...)
}

// SyncActive fsyncs the active segment. A failure damages the Store; fsync is
// never retried (ARCHITECTURE §7).
//
// Store.mu isn't held during the fsync, so Roll+Drop may seal and close the
// segment first. That error is ignored: Roll already synced it.
func (s *Store) SyncActive() error {
	if err := s.writable(); err != nil {
		return err
	}
	seg, err := s.activeSegment()
	if err != nil {
		return err
	}
	if err := seg.sync(); err != nil {
		if seg.sealed.Load() {
			return nil
		}
		s.damaged.Store(true)
		return fmt.Errorf("%w: %w", ErrSegmentDamaged, err)
	}
	return nil
}

// Active returns the active segment's ID and size; ok is false if none.
func (s *Store) Active() (id uint32, size int64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return 0, 0, false
	}
	return s.active.name.ID, s.active.size.Load(), true
}

// DiskBytes returns the total size of all files in the set.
func (s *Store) DiskBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for _, seg := range s.segs {
		total += seg.size.Load()
	}
	return total
}

// Close syncs the active segment (unless damaged) and closes all files. Later
// calls return ErrStoreClosed; a second Close returns nil.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	segs, active := s.segs, s.active
	s.segs, s.active = nil, nil
	s.mu.Unlock()

	var errs []error
	if active != nil && !s.damaged.Load() {
		if err := active.sync(); err != nil {
			s.damaged.Store(true)
			errs = append(errs, fmt.Errorf("%w: %w", ErrSegmentDamaged, err))
		}
	}
	for _, id := range slices.Sorted(maps.Keys(segs)) {
		errs = append(errs, segs[id].close())
	}
	return errors.Join(errs...)
}

// addMode is the rule a new file must satisfy to join the set.
type addMode uint8

const (
	addActive addMode = iota + 1 // first active segment
	addRoll                      // next active segment, replacing cur
	addSealed                    // sealed file below the active one
)

// writable returns why the Store can't take writes, or nil.
func (s *Store) writable() error {
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrStoreClosed
	}
	if s.damaged.Load() {
		return ErrSegmentDamaged
	}
	return nil
}

func (s *Store) activeSegment() (*segment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	if s.active == nil {
		return nil, ErrNoActiveSegment
	}
	return s.active, nil
}

// checkAdd checks ordering before any file is touched, so a doomed call
// leaves nothing on disk.
func (s *Store) checkAdd(id uint32, mode addMode, cur *segment) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkAddLocked(id, mode, cur)
}

// install re-checks ordering under the write lock (the set may have changed
// during I/O since checkAdd) and adds seg.
func (s *Store) install(seg *segment, mode addMode, cur *segment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkAddLocked(seg.name.ID, mode, cur); err != nil {
		return err
	}
	s.segs[seg.name.ID] = seg
	switch mode {
	case addActive:
		s.active = seg
	case addRoll:
		cur.sealed.Store(true)
		s.active = seg
	case addSealed:
	}
	return nil
}

// checkAddLocked enforces invariant 4: unique IDs, active is highest, sealed
// below it. s.mu must be held.
func (s *Store) checkAddLocked(id uint32, mode addMode, cur *segment) error {
	if s.closed {
		return ErrStoreClosed
	}
	if _, dup := s.segs[id]; dup {
		return fmt.Errorf("%w: file %d is already open", ErrSegmentOrder, id)
	}
	switch mode {
	case addActive, addRoll:
		if mode == addActive && s.active != nil {
			return fmt.Errorf("%w: active segment %d already exists", ErrSegmentOrder, s.active.name.ID)
		}
		if mode == addRoll && s.active != cur {
			return fmt.Errorf("%w: active segment changed during roll", ErrSegmentOrder)
		}
		for other := range s.segs {
			if other > id {
				return fmt.Errorf("%w: new active segment %d is not above file %d", ErrSegmentOrder, id, other)
			}
		}
	case addSealed:
		if s.active != nil && id > s.active.name.ID {
			return fmt.Errorf("%w: sealed file %d is above active segment %d", ErrSegmentOrder, id, s.active.name.ID)
		}
	}
	return nil
}

// discard closes and deletes a created-but-never-installed file. No dir sync:
// if the delete is lost in a crash, recovery just finds an empty segment.
func (s *Store) discard(seg *segment) error {
	errs := []error{seg.close()}
	if err := s.fsys.Remove(seg.path); err != nil {
		errs = append(errs, fmt.Errorf("storage: remove %s: %w", seg.path, err))
	}
	return errors.Join(errs...)
}

// syncDir makes dir changes durable; failure damages the Store.
func (s *Store) syncDir() error {
	if err := s.fsys.SyncDir(s.dir); err != nil {
		s.damaged.Store(true)
		return fmt.Errorf("%w: sync directory %s: %w", ErrSegmentDamaged, s.dir, err)
	}
	return nil
}

// detach validates all IDs and removes them from the set under one lock,
// returning the segments to close outside it.
func (s *Store) detach(ids []uint32) ([]*segment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	for _, id := range ids {
		seg, ok := s.segs[id]
		if !ok {
			return nil, fmt.Errorf("%w: drop file %d", ErrSegmentNotFound, id)
		}
		if seg == s.active {
			return nil, fmt.Errorf("%w: cannot drop active segment %d", ErrSegmentOrder, id)
		}
	}
	dropped := make([]*segment, 0, len(ids))
	for _, id := range ids {
		if seg, ok := s.segs[id]; ok {
			delete(s.segs, id)
			dropped = append(dropped, seg)
		}
	}
	return dropped, nil
}
