package storage

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileName_Format(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "seg_0000000007.data", SegmentName(7))
	assert.Equal(t, "compact_0000000042.data", CompactName(42))
	assert.Equal(t, "compact_0000000042.tmp", CompactTmpName(42))
	assert.Equal(t, "seg_4294967295.data", SegmentName(math.MaxUint32))
	assert.Equal(t, "seg_0000000000.data", SegmentName(0))
	assert.Empty(t, FileName{Kind: 0, ID: 1}.String(), "unknown kind has no name")
}

func TestFileName_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, kind := range []FileKind{KindSegment, KindCompact, KindCompactTmp} {
		for _, id := range []uint32{0, 1, 9, 10, 123456789, math.MaxUint32 - 1, math.MaxUint32} {
			want := FileName{Kind: kind, ID: id}
			got, ok, err := ParseFileName(want.String())
			require.NoError(t, err, want.String())
			require.True(t, ok, want.String())
			assert.Equal(t, want, got)
		}
	}
}

func TestParseFileName_IgnoresUnknownNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"",
		"LOCK",
		"seg_0000000001",
		"seg_0000000001.dat",
		"seg_0000000001.data.bak",
		"seg_0000000001.tmp",
		"xseg_0000000001.data",
		"seg-0000000001.data",
		"SEG_0000000001.data",
		"compact_0000000001.tmp~",
		"compact_0000000001.TMP",
		".seg_0000000001.data",
		"0000000001.data",
	} {
		got, ok, err := ParseFileName(name)
		require.NoError(t, err, "%q", name)
		assert.False(t, ok, "%q", name)
		assert.Zero(t, got, "%q", name)
	}
}

func TestParseFileName_RejectsMalformedIDs(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"seg_.data",
		"seg_1.data",
		"seg_000000001.data",   // 9 digits
		"seg_00000000001.data", // 11 digits
		"seg_000000000a.data",
		"seg_+000000001.data",
		"seg_-000000001.data",
		"seg_ 000000001.data",
		"seg_4294967296.data", // MaxUint32 + 1
		"seg_9999999999.data",
		"compact_12.data",
		"compact_000000000x.tmp",
		"compact_99999999999.tmp",
	} {
		got, ok, err := ParseFileName(name)
		require.ErrorIs(t, err, ErrBadFileName, "%q", name)
		assert.Contains(t, err.Error(), name)
		assert.False(t, ok, "%q", name)
		assert.Zero(t, got, "%q", name)
	}
}

func TestFileName_Readable(t *testing.T) {
	t.Parallel()
	assert.True(t, FileName{Kind: KindSegment}.Readable())
	assert.True(t, FileName{Kind: KindCompact}.Readable())
	assert.False(t, FileName{Kind: KindCompactTmp}.Readable())
	assert.False(t, FileName{}.Readable())
}

func TestFileKind_String(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "segment", KindSegment.String())
	assert.Equal(t, "compact", KindCompact.String())
	assert.Equal(t, "compact-tmp", KindCompactTmp.String())
	assert.Equal(t, "FileKind(9)", FileKind(9).String())
}
