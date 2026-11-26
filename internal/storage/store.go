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
	// Limits bound the length of a single ReadAt to the largest record they
	// allow. Zero fields mean the defaults.
	Limits Limits
}

func (o StoreOptions) withDefaults() StoreOptions {
	o.Limits = o.Limits.withDefaults()
	return o
}

// Store is the set of open data files of one directory, keyed by file ID: at
// most one active segment that takes appends, plus any number of sealed
// files (old segments and compaction outputs) that are only read.
//
// Concurrency (ARCHITECTURE §6):
//   - Append, CreateActive, Roll and Close change the active segment and must
//     be serialized by the caller (the engine's writer lock). Store.mu does not
//     order appends; it protects only the set.
//   - ReadAt, Register, Drop, SyncActive, Active and DiskBytes are safe to call
//     concurrently with each other and with the calls above, except that
//     nothing may run concurrently with Close.
//
// Once an append cannot be undone or an fsync fails, the Store is damaged: it
// refuses every later write with ErrSegmentDamaged (invariant 8). Reads keep
// working.
type Store struct {
	fsys vfs.FS
	dir  string
	opts StoreOptions

	// damaged is one-way; see ErrSegmentDamaged.
	damaged atomic.Bool

	// mu protects the set. The read lock is held across a ReadAt so a file
	// cannot be closed under a reader; the write lock is held only while
	// entries are added or removed, never during I/O.
	mu     sync.RWMutex
	segs   map[uint32]*segment
	active *segment // also present in segs; nil until CreateActive
	closed bool
}

// NewStore returns an empty Store for the data files in dir. It touches no
// files; CreateActive and Register populate it.
func NewStore(fsys vfs.FS, dir string, opts StoreOptions) *Store {
	return &Store{
		fsys: fsys,
		dir:  dir,
		opts: opts.withDefaults(),
		segs: make(map[uint32]*segment),
	}
}

// CreateActive creates seg_<id>.data as the active segment and syncs the
// directory. It fails if there already is an active segment, if id is not
// above every file in the set, or if the file already exists on disk.
//
// If the directory sync fails the segment stays installed but the Store is
// damaged and the error wraps ErrSegmentDamaged.
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

// Register opens an existing sealed file (a segment from an earlier run or a
// finished compaction output) and adds it to the set so it can be read. Its
// ID must be free and below the active segment's.
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

// Append writes frame, a complete encoded record, to the active segment with
// a single Write and returns where it landed. It requires external
// serialization (see Store).
//
// A failed or short write is truncated away and the error returned; the
// Store stays writable. If the truncate fails too, the error wraps
// ErrSegmentDamaged and the Store accepts no more writes. Out-of-space errors
// keep vfs.ErrNoSpace in their chain.
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

// ReadAt reads exactly n bytes at offset from file fileID with one positional
// read. Bytes returned by a completed Append are visible immediately. An
// unknown or dropped fileID returns ErrSegmentNotFound; n outside 1..the
// largest record the limits allow, or a range past the end of the file,
// returns ErrCorrupt.
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

// Roll seals the active segment and makes seg_<nextID>.data the active one:
// it syncs the current file, creates the next, installs it and syncs the
// directory (invariant 7). nextID must be above every file in the set. It
// requires external serialization (see Store).
//
// If the sync of the current file or of the directory fails, the Store is
// damaged and the error wraps ErrSegmentDamaged. If only creating the next
// file fails, the current segment stays active and writable.
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

// Drop removes the given sealed files from the set, then closes and deletes
// them and syncs the directory. A file leaves the set before it is closed,
// so a concurrent ReadAt either completes or gets ErrSegmentNotFound
// (ARCHITECTURE §6). Duplicate IDs are ignored.
//
// Drop checks every ID first and changes nothing if one is unknown
// (ErrSegmentNotFound) or is the active segment (ErrSegmentOrder). Once the
// files have left the set, close and delete errors are joined and returned;
// a failed directory sync damages the Store.
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

// SyncActive fsyncs the active segment. A failure damages the Store: an
// fsync error is never retried (ARCHITECTURE §7).
//
// SyncActive does not hold Store.mu during the fsync, so a concurrent Roll
// may seal the segment, and a Drop may then close it, before the fsync runs.
// That fsync then fails on a closed handle, but Roll already synced the
// segment before sealing it, so nothing is at risk and the error is dropped.
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

// Active returns the active segment's ID and its size in bytes; ok is false
// if there is none.
func (s *Store) Active() (id uint32, size int64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return 0, 0, false
	}
	return s.active.name.ID, s.active.size.Load(), true
}

// DiskBytes returns the total size of every file in the set.
func (s *Store) DiskBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for _, seg := range s.segs {
		total += seg.size.Load()
	}
	return total
}

// Close syncs the active segment, unless the Store is damaged, and closes
// every file. Later calls return ErrStoreClosed; a second Close returns nil.
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
	addActive addMode = iota + 1 // the first active segment
	addRoll                      // the next active segment, replacing cur
	addSealed                    // a sealed file below the active segment
)

// writable reports why the Store cannot take writes, if it cannot.
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

// checkAdd applies the ordering rules before any file is touched, so a call
// that is bound to fail leaves nothing behind on disk.
func (s *Store) checkAdd(id uint32, mode addMode, cur *segment) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.checkAddLocked(id, mode, cur)
}

// install re-applies the ordering rules under the write lock, because the set
// may have changed during the file I/O since checkAdd, and adds seg.
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

// checkAddLocked enforces invariant 4 on the set: IDs are unique, the active
// segment has the highest ID, and sealed files sit below it. s.mu must be held.
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

// discard closes and deletes a file that was created but never installed.
// The directory is not synced afterwards: the create was never synced either,
// and if the delete is lost in a crash, recovery finds only an empty segment,
// which replays to nothing.
func (s *Store) discard(seg *segment) error {
	errs := []error{seg.close()}
	if err := s.fsys.Remove(seg.path); err != nil {
		errs = append(errs, fmt.Errorf("storage: remove %s: %w", seg.path, err))
	}
	return errors.Join(errs...)
}

// syncDir makes creates and removes in the directory durable. Like any fsync
// failure, a failed directory sync damages the Store.
func (s *Store) syncDir() error {
	if err := s.fsys.SyncDir(s.dir); err != nil {
		s.damaged.Store(true)
		return fmt.Errorf("%w: sync directory %s: %w", ErrSegmentDamaged, s.dir, err)
	}
	return nil
}

// detach validates every ID and then removes the files from the set, all
// under one write lock, and returns them for closing outside it.
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
