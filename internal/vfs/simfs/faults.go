package simfs

import (
	"errors"
	"slices"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// The four deterministic faults of ARCHITECTURE §11. Every fault is armed
// explicitly by the test and fires at a point fixed by the global order that
// SimFS.mu gives all calls, so a run never depends on timing or randomness.

// ErrCrashed is returned by every FS and file call after a crash, and by every
// call on a handle opened before the last Restart.
var ErrCrashed = errors.New("simfs: file system crashed")

// ErrInjected is the error FailNth uses when it is given a nil error.
var ErrInjected = errors.New("simfs: injected fault")

// NoQuota passed to SetQuota removes the disk quota. It is the default.
const NoQuota int64 = -1

// Op names an operation that FailNth can fail.
type Op string

// Operations that FailNth can fail. Any other Op value never fires.
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

// LoseUnsynced makes Restart cut every file back to its synced length,
// simulating a power loss that drops the page cache.
const LoseUnsynced RestartOption = 1

// nthFault fails the call of its operation that brings remaining to zero.
type nthFault struct {
	remaining int
	err       error
}

// faults is the armed fault state. It is guarded by SimFS.mu.
type faults struct {
	nth       map[Op]nthFault
	quota     int64 // byte limit on the files reachable by name; NoQuota if none
	tearArmed bool
	tearAt    int64 // global byte count at which the tearing write stops
	written   int64 // bytes applied by Write since New, across all files
	crashed   bool
	gen       uint64 // incremented by Restart; handles from older generations are dead
}

// FailNth arms a one-shot fault: the nth call of op from now on (n = 1 is the
// next call) returns err and changes nothing; later calls succeed again. A
// failed write applies no bytes; use SetQuota or TearAt for short writes.
// Every call of op counts, including one that would have failed anyway (a
// Create of an existing name) or done nothing (an empty write); calls
// rejected because the FS is crashed or the handle is dead or closed do not.
// n < 1 is treated as 1, a nil err as ErrInjected. Arming op again replaces
// its previous fault; faults on different operations are independent.
func (s *SimFS) FailNth(op Op, n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		err = ErrInjected
	}
	s.flt.nth[op] = nthFault{remaining: max(n, 1), err: err}
}

// TearAt arms a torn write at global byte n, counted like BytesWritten. The
// first write that would take the counter past n applies only the bytes up to
// n, then crashes the FS and returns the short count with ErrCrashed. If the
// counter is already at or past n, the next non-empty write applies nothing
// and crashes. The tear fires once.
func (s *SimFS) TearAt(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flt.tearArmed = true
	s.flt.tearAt = n
}

// SetQuota limits the total size of all files reachable by name to limit
// bytes; NoQuota (any negative limit) removes the limit. A write that does not
// fit applies the bytes that do and returns the short count with
// vfs.ErrNoSpace. Removing, truncating or replacing a file frees its bytes at
// once, even if a handle keeps the removed file open (a simplification: a
// real file system frees them on the last close). For the same reason bytes
// written through a handle to such a removed file are limited by the quota
// but never charged to it; TinyCask never writes to a removed file. Only
// Write checks the quota; zero-extension by Truncate is not charged, as for a
// sparse file.
func (s *SimFS) SetQuota(limit int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flt.quota = max(limit, NoQuota)
}

// BytesWritten returns the number of bytes Write has applied since New,
// across all files and restarts, including the prefix of a short or torn
// write. Tests read it to choose a crash point for TearAt.
func (s *SimFS) BytesWritten() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flt.written
}

// Crash crashes the FS now: every FS and file call returns ErrCrashed until
// Restart. File contents, synced or not, are kept until then.
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

// Restart simulates a reboot, whether or not the FS crashed: it clears the
// crash, kills every handle opened so far (their calls, Close included,
// return ErrCrashed), releases every lock as process death releases flock,
// and disarms pending FailNth and TearAt faults. The quota and the
// BytesWritten counter persist. With LoseUnsynced every file is cut back to
// its synced length; without it unsynced bytes survive. Names survive either
// way, because namespace operations are durable at once (ARCHITECTURE §11).
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

// handleAlive returns ErrCrashed while the FS is crashed or if a handle of
// generation gen predates the last Restart. Caller holds s.mu.
func (s *SimFS) handleAlive(gen uint64) error {
	if s.flt.crashed || gen != s.flt.gen {
		return ErrCrashed
	}
	return nil
}

// inject counts a call of op and returns the armed error if this call is the
// one FailNth chose, disarming it. Caller holds s.mu.
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

// admitWrite decides how many of the n bytes of a write land and, if not all,
// returns the error that stops the rest: vfs.ErrNoSpace from the quota or
// ErrCrashed from the tear, whichever cuts first (the tear on a tie). A tear
// that fires crashes the FS and disarms itself; the caller still applies the
// admitted prefix. Caller holds s.mu.
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

// usedBytes returns the total size of the files reachable by name. Caller
// holds s.mu.
func (s *SimFS) usedBytes() int64 {
	var used int64
	for _, ino := range s.files {
		used += int64(len(ino.data))
	}
	return used
}
