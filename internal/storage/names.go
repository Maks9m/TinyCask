package storage

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// FileKind says which of the data-directory files a name refers to
// (ARCHITECTURE §3.2).
type FileKind uint8

const (
	// KindSegment is a regular segment, seg_<id>.data.
	KindSegment FileKind = iota + 1
	// KindCompact is a finished compaction output, compact_<id>.data. It
	// supersedes every data file with a lower ID.
	KindCompact
	// KindCompactTmp is an unfinished compaction output, compact_<id>.tmp.
	// Recovery deletes it.
	KindCompactTmp
)

// idDigits is the fixed width of the ID in a file name: enough for any uint32.
const idDigits = 10

// fileNamePattern is the prefix and suffix around the ID for one FileKind.
type fileNamePattern struct {
	kind           FileKind
	prefix, suffix string
}

// fileNamePatterns lists every kind ParseFileName recognizes. It is a
// function, not a package-level var, so there is no mutable package state
// (ARCHITECTURE §2, hard rule 4).
func fileNamePatterns() [3]fileNamePattern {
	return [...]fileNamePattern{
		{KindSegment, "seg_", ".data"},
		{KindCompact, "compact_", ".data"},
		{KindCompactTmp, "compact_", ".tmp"},
	}
}

// String returns the kind's name for logs and error messages.
func (k FileKind) String() string {
	switch k {
	case KindSegment:
		return "segment"
	case KindCompact:
		return "compact"
	case KindCompactTmp:
		return "compact-tmp"
	}
	return "FileKind(" + strconv.Itoa(int(k)) + ")"
}

// FileName identifies one data file by kind and ID.
type FileName struct {
	Kind FileKind
	ID   uint32
}

// String returns the base name of the file, for example seg_0000000007.data.
// It returns an empty string for an unknown kind.
func (n FileName) String() string {
	for _, p := range fileNamePatterns() {
		if p.kind == n.Kind {
			return fmt.Sprintf("%s%0*d%s", p.prefix, idDigits, n.ID, p.suffix)
		}
	}
	return ""
}

// Readable reports whether the file holds records the store may serve: a
// segment or a finished compaction output.
func (n FileName) Readable() bool {
	return n.Kind == KindSegment || n.Kind == KindCompact
}

// SegmentName returns the base name of regular segment id.
func SegmentName(id uint32) string { return FileName{KindSegment, id}.String() }

// CompactName returns the base name of finished compaction output id.
func CompactName(id uint32) string { return FileName{KindCompact, id}.String() }

// CompactTmpName returns the base name of unfinished compaction output id.
func CompactTmpName(id uint32) string { return FileName{KindCompactTmp, id}.String() }

// ParseFileName parses a base name from the data directory.
//
// A name that matches none of the data-file patterns (for example LOCK) is
// not a data file: ok is false and err is nil, and the caller ignores it. A
// name with a data-file prefix and suffix but a malformed ID returns an error
// wrapping ErrBadFileName.
func ParseFileName(base string) (name FileName, ok bool, err error) {
	for _, p := range fileNamePatterns() {
		digits, found := strings.CutPrefix(base, p.prefix)
		if !found {
			continue
		}
		if digits, found = strings.CutSuffix(digits, p.suffix); !found {
			continue
		}
		id, err := parseID(digits)
		if err != nil {
			return FileName{}, false, fmt.Errorf("%w: %q: %w", ErrBadFileName, base, err)
		}
		return FileName{Kind: p.kind, ID: id}, true, nil
	}
	return FileName{}, false, nil
}

// parseID accepts exactly idDigits ASCII digits whose value fits in a uint32.
func parseID(s string) (uint32, error) {
	if len(s) != idDigits {
		return 0, fmt.Errorf("id %q has %d characters, want %d digits", s, len(s), idDigits)
	}
	var v uint64
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("id %q has a non-digit at position %d", s, i)
		}
		v = v*10 + uint64(c-'0')
	}
	if v > math.MaxUint32 {
		return 0, fmt.Errorf("id %s is larger than %d", s, uint64(math.MaxUint32))
	}
	return uint32(v), nil
}
