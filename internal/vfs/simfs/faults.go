package simfs

import (
	"errors"
	"slices"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// The four deterministic faults of ARCHITECTURE §11. Tests arm them; they
// fire at a point fixed by SimFS.mu's global call order, never by timing.

// ErrCrashed is returned by every call after a crash, and by handles opened
// before the last Restart.
var ErrCrashed = errors.New("simfs: file system crashed")

// ErrInjected is FailNth's error when given nil.
var ErrInjected = errors.New("simfs: injected fault")

// NoQuota removes the disk quota (default).
const NoQuota int64 = -1

// Op names an operation FailNth can fail.
type Op string

// Operations FailNth can fail.
const (
	OpWrite    Op = "write"
	OpSync     Op = "sync"
	OpCreate   Op = "create"
	OpRename   Op = "rename"
	OpRemove   Op = "remove"
	OpTruncate Op = "truncate"
)

// RestartOption changes what Restart does to file contents.
type RestartOption int

// LoseUnsynced makes Restart cut files to their synced length (power loss).
const LoseUnsynced RestartOption = 1

// nthFault fires on the call that brings remaining to zero.
type nthFault struct {
	remaining int
	err       error
}

// faults is the armed fault state, guarded by SimFS.mu.
type faults struct {
	nth       map[Op]nthFault
	quota     int64 // byte limit on named files; NoQuota if none
	tearArmed bool
	tearAt    int64 // global byte count where the tearing write stops
	written   int64 // bytes applied by Write since New, all files
	crashed   bool
	gen       uint64 // bumped by Restart; older handles are dead
}

// FailNth arms a one-shot fault: the nth call of op (n=1 is next) returns err
// and changes nothing (a failed write applies no bytes; use SetQuota/TearAt
// for short writes). Every call counts, even no-ops, except calls rejected
// for a crash or dead/closed handle. n<1 means 1, nil err means ErrInjected.
// Re-arming op replaces its fault.
func (s *SimFS) FailNth(op Op, n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		err = ErrInjected
	}
	s.flt.nth[op] = nthFault{remaining: max(n, 1), err: err}
}

// TearAt arms a one-shot torn write at global byte n (as BytesWritten counts).
// The write crossing n applies bytes up to n, crashes the FS and returns the
// short count with ErrCrashed. If already past n, the next non-empty write
// applies nothing and crashes.
func (s *SimFS) TearAt(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flt.tearArmed = true
	s.flt.tearAt = n
}

// SetQuota caps the total size of named files; negative (NoQuota) removes it.
// A write that doesn't fit applies what fits and returns vfs.ErrNoSpace.
//
// Simplifications: removing/replacing a file frees its bytes at once even if
// a handle is open (real FSes wait for last close), and writes to such a
// removed file are never charged. Truncate zero-extension isn't charged
// (sparse file). Only Write checks the quota.
func (s *SimFS) SetQuota(limit int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flt.quota = max(limit, NoQuota)
}

// BytesWritten returns bytes applied by Write since New (all files, across
// restarts, including short-write prefixes). Used to pick a TearAt point.
func (s *SimFS) BytesWritten() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flt.written
}

// Crash makes every call return ErrCrashed until Restart. Contents are kept.
func (s *SimFS) Crash() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flt.crashed = true
}

// Crashed reports whether the FS is crashed and waiting for Restart.
func (s *SimFS) Crashed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flt.crashed
}

// Restart simulates a reboot: clears the crash, kills all existing handles
// (calls return ErrCrashed), releases all locks (like process death with
// flock) and disarms FailNth/TearAt. Quota and BytesWritten persist.
// LoseUnsynced cuts files to their synced length; names always survive
// (namespace ops are durable at once, ARCHITECTURE §11).
func (s *SimFS) Restart(opts ...RestartOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lose := slices.Contains(opts, LoseUnsynced)
	s.flt.gen++
	s.flt.crashed = false
	s.flt.tearArmed = false
	clear(s.flt.nth)
	for _, ino := range s.files {
		ino.locked = false
		if lose {
			ino.data = ino.data[:ino.synced]
		}
	}
}

// alive returns ErrCrashed while the FS is crashed. Caller holds s.mu.
func (s *SimFS) alive() error {
	if s.flt.crashed {
		return ErrCrashed
	}
	return nil
}

// handleAlive returns ErrCrashed if crashed or gen predates the last Restart.
// Caller holds s.mu.
func (s *SimFS) handleAlive(gen uint64) error {
	if s.flt.crashed || gen != s.flt.gen {
		return ErrCrashed
	}
	return nil
}

// inject counts a call of op and returns (and disarms) the FailNth error if
// this is the chosen call. Caller holds s.mu.
func (s *SimFS) inject(op Op) error {
	f, ok := s.flt.nth[op]
	if !ok {
		return nil
	}
	f.remaining--
	if f.remaining > 0 {
		s.flt.nth[op] = f
		return nil
	}
	delete(s.flt.nth, op)
	return f.err
}

// admitWrite returns how many of n bytes land and, if not all, why:
// vfs.ErrNoSpace (quota) or ErrCrashed (tear), whichever cuts first (tear on
// a tie). A firing tear crashes the FS; the caller still applies the prefix.
// Caller holds s.mu.
func (s *SimFS) admitWrite(n int) (int, error) {
	allowed := int64(n)
	var stop error
	if s.flt.quota >= 0 {
		if avail := max(s.flt.quota-s.usedBytes(), 0); avail < allowed {
			allowed, stop = avail, vfs.ErrNoSpace
		}
	}
	if s.flt.tearArmed && n > 0 {
		if cut := max(s.flt.tearAt-s.flt.written, 0); cut < int64(n) && cut <= allowed {
			allowed, stop = cut, ErrCrashed
			s.flt.tearArmed = false
			s.flt.crashed = true
		}
	}
	return int(allowed), stop
}

// usedBytes returns the total size of named files. Caller holds s.mu.
func (s *SimFS) usedBytes() int64 {
	var used int64
	for _, ino := range s.files {
		used += int64(len(ino.data))
	}
	return used
}
