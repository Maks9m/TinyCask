package storage

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync/atomic"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// segment is one open data file: a regular segment or a compaction output.
//
// Records are only ever appended and never rewritten (invariant 4). Appends
// go straight to the file with one Write each; there is no userspace buffer,
// so a reader sees a record as soon as append returns.
type segment struct {
	name FileName
	path string
	f    vfs.File

	// size is the file length the store has accepted: it grows only after a
	// complete Write, so readers never see a torn or truncated-away tail. The
	// appender writes it; readers and DiskBytes load it concurrently.
	size atomic.Int64
	// sealed is set when the segment stops being the active one. A sealed
	// segment is synced and never appended to again.
	sealed atomic.Bool
}

// createSegment creates a new, empty data file in dir. It fails if the file
// already exists, so a segment from an earlier run is never appended to.
func createSegment(fsys vfs.FS, dir string, name FileName) (*segment, error) {
	path := filepath.Join(dir, name.String())
	f, err := fsys.Create(path)
	if err != nil {
		return nil, fmt.Errorf("storage: create %s: %w", path, err)
	}
	return &segment{name: name, path: path, f: f}, nil
}

// openSegment opens an existing data file in dir as a sealed segment.
func openSegment(fsys vfs.FS, dir string, name FileName) (*segment, error) {
	path := filepath.Join(dir, name.String())
	f, err := fsys.Open(path)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}
	size, err := f.Size()
	if err != nil {
		// Nothing was written through f, so a failed Close loses no data;
		// it is still reported.
		return nil, errors.Join(fmt.Errorf("storage: size of %s: %w", path, err), f.Close())
	}
	s := &segment{name: name, path: path, f: f}
	s.size.Store(size)
	s.sealed.Store(true)
	return s, nil
}

// append writes frame at the end of the segment with a single Write and
// returns the offset where it starts. The caller must serialize appends.
//
// A failed or short write is truncated away, so the file ends at the last
// complete record again and the error is returned. If that truncate fails too,
// the returned error also wraps ErrSegmentDamaged (invariant 8).
func (s *segment) append(frame []byte) (int64, error) {
	if s.sealed.Load() {
		return 0, fmt.Errorf("storage: append to sealed %s", s.path)
	}
	off := s.size.Load()
	n, err := s.f.Write(frame)
	if err == nil && n != len(frame) {
		// io.Writer forbids this, but a short write must never be trusted.
		err = io.ErrShortWrite
	}
	if err == nil {
		s.size.Store(off + int64(n))
		return off, nil
	}
	werr := fmt.Errorf("storage: append %d bytes to %s at %d (wrote %d): %w", len(frame), s.path, off, n, err)
	if terr := s.f.Truncate(off); terr != nil {
		return 0, errors.Join(werr, fmt.Errorf("%w: truncate %s to %d: %w", ErrSegmentDamaged, s.path, off, terr))
	}
	return 0, werr
}

// readAt reads exactly n bytes at off. A range past the accepted end of the
// segment is reported as corruption before anything is allocated: only a
// damaged index can ask for it.
func (s *segment) readAt(off, n int64) ([]byte, error) {
	if size := s.size.Load(); off < 0 || n < 0 || off > size-n {
		return nil, fmt.Errorf("%w: read %d bytes at %d in %s: outside the %d-byte file: %w",
			ErrCorrupt, n, off, s.path, size, io.ErrUnexpectedEOF)
	}
	buf := make([]byte, n)
	m, err := s.f.ReadAt(buf, off)
	if m == len(buf) {
		// io.ReaderAt may report io.EOF together with a full read.
		return buf, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: read %d bytes at %d in %s: got %d: %w",
			ErrCorrupt, n, off, s.path, m, io.ErrUnexpectedEOF)
	}
	return nil, fmt.Errorf("storage: read %d bytes at %d in %s: %w", n, off, s.path, err)
}

// sync flushes the segment to stable storage.
func (s *segment) sync() error {
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("storage: sync %s: %w", s.path, err)
	}
	return nil
}

// close closes the file handle. The caller must have removed the segment
// from the set first so that no read is in flight (ARCHITECTURE §6).
func (s *segment) close() error {
	if err := s.f.Close(); err != nil {
		return fmt.Errorf("storage: close %s: %w", s.path, err)
	}
	return nil
}
