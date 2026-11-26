package storage

import (
	"errors"
	"fmt"
	"io"
)

// DefaultScanChunkSize is how many bytes a Scanner asks for per ReadAt by
// default. Records are usually far smaller, so one read covers many of them.
const DefaultScanChunkSize = 64 << 10

// ScanOptions configures a Scanner. The zero value uses the defaults.
type ScanOptions struct {
	// Limits are checked against every header before its lengths are trusted.
	// Zero fields mean the defaults.
	Limits Limits
	// ChunkSize is the preferred length of one ReadAt. A record longer than
	// this is read with one ReadAt of exactly its (validated) length. Zero or
	// negative means DefaultScanChunkSize.
	ChunkSize int
}

func (o ScanOptions) withDefaults() ScanOptions {
	o.Limits = o.Limits.withDefaults()
	if o.ChunkSize <= 0 {
		o.ChunkSize = DefaultScanChunkSize
	}
	return o
}

// ScanError reports why a scan stopped before the end of the file and where:
// Offset is the first byte of the record that could not be read, which is
// also the end of the last valid record. Recovery truncates the highest-ID
// file to exactly this length (ARCHITECTURE §5.4).
//
// Err wraps ErrCorrupt when the bytes are not a valid record. Any other Err is
// an I/O error from the reader, and says nothing about the file's contents.
type ScanError struct {
	Offset int64
	Err    error
}

// Error reports the stop offset and the cause.
func (e *ScanError) Error() string {
	return fmt.Sprintf("storage: scan stopped at offset %d: %v", e.Offset, e.Err)
}

// Unwrap returns Err, so errors.Is(err, ErrCorrupt) sees through a ScanError.
func (e *ScanError) Unwrap() error { return e.Err }

// Scanner iterates the records of one data file in order, from offset 0 up to
// a given size. It is the primitive recovery and compaction share.
//
// Use it like bufio.Scanner:
//
//	sc := NewScanner(f, size, opts)
//	for sc.Next() {
//	    use(sc.Record(), sc.RecordOffset(), sc.ValueOffset())
//	}
//	if err := sc.Err(); err != nil { ... }
//
// The scanner reads with ReadAt in chunks of about ScanOptions.ChunkSize
// bytes and keeps at most one chunk or one record in memory, never the whole
// file. Every header is validated against the limits before the record's
// length decides how much to read or allocate (invariant 10).
//
// A Scanner is not safe for concurrent use.
type Scanner struct {
	r    io.ReaderAt
	size int64
	opts ScanOptions

	// buf is reused across reads; it only grows, and never past the larger of
	// the chunk size and the largest record the limits allow.
	buf []byte
	// win holds the file bytes [winOff, winOff+len(win)); it aliases buf.
	win    []byte
	winOff int64

	// off is the offset of the next record to decode.
	off    int64
	rec    Record
	recOff int64
	err    error
	done   bool
}

// NewScanner returns a Scanner over the first size bytes of r. size is
// normally the file's length; the scanner never reads past it.
func NewScanner(r io.ReaderAt, size int64, opts ScanOptions) *Scanner {
	return &Scanner{r: r, size: size, opts: opts.withDefaults()}
}

// Next advances to the next record and reports whether there is one. It
// returns false at a clean end of file and when the scan fails; Err tells the
// two apart. Once Next has returned false it always does.
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
	// The header passed the limits, so n <= Limits.MaxRecordSize().
	n := h.RecordSize()
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

// Record returns the record Next just decoded. Its Key and Value alias the
// scanner's buffer and are valid only until the next call of Next; copy them
// to keep them.
func (s *Scanner) Record() Record { return s.rec }

// RecordOffset returns where the current record starts in the file.
func (s *Scanner) RecordOffset() int64 { return s.recOff }

// ValueOffset returns where the current record's value starts in the file:
// the offset KeyDir stores.
func (s *Scanner) ValueOffset() int64 { return ValueOffset(s.recOff, len(s.rec.Key)) }

// Offset returns how far the scan has got: the end of the last valid record.
// After a clean end of file it equals the size; after a failure it equals
// the ScanError's Offset.
func (s *Scanner) Offset() int64 { return s.off }

// Err returns nil if the scan reached the end of the file cleanly or has not
// finished, and otherwise a *ScanError saying where and why it stopped.
func (s *Scanner) Err() error { return s.err }

func (s *Scanner) fail(err error) bool {
	s.err = &ScanError{Offset: s.off, Err: err}
	s.done = true
	return false
}

// window returns the n file bytes starting at off, reading a new chunk if
// they are not already buffered. n must already be validated: it is at most
// the largest record the limits allow. If the file holds fewer than n bytes
// from off, the error is corruption by the given rule.
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
		// io.ReaderAt forbids a short read without an error. Only io.EOF
		// proves the file is short; anything else is a reader fault and must
		// not be mistaken for corruption, or recovery would truncate.
		return nil, fmt.Errorf("storage: read %d bytes at %d: got %d: %w", want, off, m, io.ErrUnexpectedEOF)
	}
	if int64(m) < n {
		// The reader ended before the size it was given: the file is
		// shorter than the caller believed, so the record is cut short.
		return nil, corrupt(rule, "file ended after %d of %d bytes", m, n)
	}
	s.win, s.winOff = s.buf[:m], off
	return s.win[:n], nil
}
