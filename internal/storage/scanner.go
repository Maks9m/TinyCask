package storage

import (
	"errors"
	"fmt"
	"io"
)

// DefaultScanChunkSize is the default ReadAt size; one read covers many records.
const DefaultScanChunkSize = 64 << 10

// ScanOptions configures a Scanner. The zero value uses the defaults.
type ScanOptions struct {
	// Limits are checked on every header before its lengths are trusted.
	Limits Limits
	// ChunkSize is the preferred ReadAt size. A longer record is read in one
	// ReadAt of its exact length. <= 0 means DefaultScanChunkSize.
	ChunkSize int
}

func (o ScanOptions) withDefaults() ScanOptions {
	o.Limits = o.Limits.withDefaults()
	if o.ChunkSize <= 0 {
		o.ChunkSize = DefaultScanChunkSize
	}
	return o
}

// ScanError says where and why a scan stopped early. Offset is the end of the
// last valid record; recovery truncates the highest-ID file to it
// (ARCHITECTURE §5.4).
//
// Err wraps ErrCorrupt for bad bytes; anything else is an I/O error and says
// nothing about the file's contents (so recovery must not truncate).
type ScanError struct {
	Offset int64
	Err    error
}

// Error implements error.
func (e *ScanError) Error() string {
	return fmt.Sprintf("storage: scan stopped at offset %d: %v", e.Offset, e.Err)
}

// Unwrap lets errors.Is(err, ErrCorrupt) see through a ScanError.
func (e *ScanError) Unwrap() error { return e.Err }

// Scanner reads one data file's records in order, from 0 up to size. Shared
// by recovery and compaction. Usage, like bufio.Scanner:
//
//	sc := NewScanner(f, size, opts)
//	for sc.Next() {
//	    use(sc.Record(), sc.RecordOffset(), sc.ValueOffset())
//	}
//	if err := sc.Err(); err != nil { ... }
//
// It reads in chunks and holds at most one chunk or record, never the whole
// file. Headers are validated before their lengths size a read (invariant 10).
// Not safe for concurrent use.
type Scanner struct {
	r    io.ReaderAt
	size int64
	opts ScanOptions

	// buf is reused; it grows up to max(chunk, largest allowed record).
	buf []byte
	// win holds file bytes [winOff, winOff+len(win)); it aliases buf.
	win    []byte
	winOff int64

	off    int64 // next record to decode
	rec    Record
	recOff int64
	err    error
	done   bool
}

// NewScanner returns a Scanner over the first size bytes of r.
func NewScanner(r io.ReaderAt, size int64, opts ScanOptions) *Scanner {
	return &Scanner{r: r, size: size, opts: opts.withDefaults()}
}

// Next advances to the next record. It returns false at EOF or on failure
// (check Err), and keeps returning false after that.
func (s *Scanner) Next() bool {
	s.rec, s.recOff = Record{}, s.off
	if s.done {
		return false
	}
	if s.size < 0 {
		return s.fail(fmt.Errorf("storage: scan: negative size %d", s.size))
	}
	if s.off == s.size {
		s.done = true
		return false
	}

	hdr, err := s.window(s.off, HeaderSize, violShortHeader)
	if err != nil {
		return s.fail(err)
	}
	h, err := DecodeHeader(hdr, s.opts.Limits)
	if err != nil {
		return s.fail(err)
	}
	n := h.RecordSize() // safe: header passed the limits
	frame, err := s.window(s.off, n, violShortPayload)
	if err != nil {
		return s.fail(err)
	}
	rec, err := Decode(frame, s.opts.Limits)
	if err != nil {
		return s.fail(err)
	}

	s.rec, s.recOff = rec, s.off
	s.off += n
	return true
}

// Record returns the current record. Key and Value alias the scanner's buffer
// and are valid only until the next Next; copy to keep them.
func (s *Scanner) Record() Record { return s.rec }

// RecordOffset returns where the current record starts.
func (s *Scanner) RecordOffset() int64 { return s.recOff }

// ValueOffset returns where the current value starts (what KeyDir stores).
func (s *Scanner) ValueOffset() int64 { return ValueOffset(s.recOff, len(s.rec.Key)) }

// Offset returns the end of the last valid record (size after a clean EOF).
func (s *Scanner) Offset() int64 { return s.off }

// Err returns nil on clean EOF or while scanning, otherwise a *ScanError.
func (s *Scanner) Err() error { return s.err }

func (s *Scanner) fail(err error) bool {
	s.err = &ScanError{Offset: s.off, Err: err}
	s.done = true
	return false
}

// window returns file bytes [off, off+n), reading a new chunk if not buffered.
// n must already be validated. Too few bytes is corruption by rule.
func (s *Scanner) window(off, n int64, rule violation) ([]byte, error) {
	if avail := s.size - off; avail < n {
		return nil, corrupt(rule, "have %d of %d bytes", avail, n)
	}
	if start := off - s.winOff; start >= 0 && start+n <= int64(len(s.win)) {
		return s.win[start : start+n], nil
	}

	want := min(max(n, int64(s.opts.ChunkSize)), s.size-off)
	if int64(cap(s.buf)) < want {
		s.buf = make([]byte, want)
	}
	s.win = nil
	m, err := s.r.ReadAt(s.buf[:want], off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("storage: read %d bytes at %d: %w", want, off, err)
	}
	if err == nil && int64(m) < want {
		// Short read with no error breaks io.ReaderAt: a reader fault, not
		// corruption, or recovery would truncate good data.
		return nil, fmt.Errorf("storage: read %d bytes at %d: got %d: %w", want, off, m, io.ErrUnexpectedEOF)
	}
	if int64(m) < n {
		// EOF before size: the file is shorter than expected, record is cut.
		return nil, corrupt(rule, "file ended after %d of %d bytes", m, n)
	}
	s.win, s.winOff = s.buf[:m], off
	return s.win[:n], nil
}
