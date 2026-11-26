//go:build unix

package vfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMapErr(t *testing.T) {
	t.Parallel()

	enospc := &os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}
	enoent := &os.PathError{Op: "open", Path: "/x", Err: syscall.ENOENT}
	ewouldblock := fmt.Errorf("flock: %w", syscall.EWOULDBLOCK)

	tests := []struct {
		name        string
		in          error
		wantNoSpace bool
	}{
		{name: "nil stays nil", in: nil},
		{name: "ENOSPC path error", in: enospc, wantNoSpace: true},
		{name: "bare ENOSPC", in: syscall.ENOSPC, wantNoSpace: true},
		{name: "wrapped ENOSPC", in: fmt.Errorf("ctx: %w", enospc), wantNoSpace: true},
		{name: "ENOENT untouched", in: enoent},
		{name: "EOF untouched", in: io.EOF},
		{name: "EWOULDBLOCK untouched", in: ewouldblock},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mapErr(tt.in)
			if !tt.wantNoSpace {
				// No translation other than ENOSPC: the same error comes back.
				assert.Equal(t, tt.in, got)
				return
			}
			assert.ErrorIs(t, got, ErrNoSpace)
			assert.ErrorIs(t, got, syscall.ENOSPC, "original cause kept")
			assert.ErrorIs(t, got, tt.in)
		})
	}
}

func TestMapErr_KeepsNotExist(t *testing.T) {
	t.Parallel()
	err := mapErr(&os.PathError{Op: "open", Path: "/x", Err: syscall.ENOENT})
	assert.ErrorIs(t, err, fs.ErrNotExist)
	assert.False(t, errors.Is(err, ErrNoSpace))
}
