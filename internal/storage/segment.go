package storage

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync/atomic"

	"github.com/Maks9m/TinyCask/internal/vfs"
)

// segment is one open data file (regular segment or compaction output).
// Append-only (invariant 4), one Write per record, no userspace buffer, so a
// record is readable as soon as append returns.
type segment struct {
	name FileName
	path string
	f    vfs.File

	// size is the accepted length: it grows only after a complete Write, so
	// readers never see a torn tail. Written by the appender, read concurrently.
	size atomic.Int64
	// sealed: no longer active, already synced, never appended to again.
	sealed atomic.Bool
}

// createSegment creates a new empty file; it fails if it exists, so a file from
// an earlier run is never appended to.
func createSegment(fsys vfs.FS, dir string, name FileName) (*segment, error) {
	path := filepath.Join(dir, name.String())
	f, err := fsys.Create(path)
	if err != nil {
		return nil, fmt.Errorf("storage: create %s: %w", path, err)
	}
	return &segment{name: name, path: path, f: f}, nil
}

// openSegment opens an existing file as a sealed segment.
func openSegment(fsys vfs.FS, dir string, name FileName) (*segment, error) {
	path := filepath.Join(dir, name.String())
	f, err := fsys.Open(path)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}
	size, err := f.Size()
	if err != nil {
		// Read-only handle: a failed Close loses nothing, but still report it.
		return nil, errors.Join(fmt.Errorf("storage: size of %s: %w", path, err), f.Close())
	}
	s := &segment{name: name, path: path, f: f}
	s.size.Store(size)
	s.sealed.Store(true)
	return s, nil
}

// append writes frame with one Write and returns its offset. Caller serializes.
// A failed or short write is truncated away; if the truncate fails too, the
// error also wraps ErrSegmentDamaged (invariant 8).
func (s *segment) append(frame []byte) (int64, error) {
	if s.sealed.Load() {
		return 0, fmt.Errorf("storage: append to sealed %s", s.path)
	}
	off := s.size.Load()
	n, err := s.f.Write(frame)
	if err == nil && n != len(frame) {
		// io.Writer forbids this, but never trust a short write.
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

// readAt reads exactly n bytes at off. A range past the accepted size is
// corruption (only a bad index asks for it), rejected before allocating.
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

func (s *segment) sync() error {
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("storage: sync %s: %w", s.path, err)
	}
	return nil
}

// close closes the file. Remove it from the set first so no read is in flight
// (ARCHITECTURE §6).
func (s *segment) close() error {
	if err := s.f.Close(); err != nil {
		return fmt.Errorf("storage: close %s: %w", s.path, err)
	}
	return nil
}
