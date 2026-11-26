//go:build unix

package vfs

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// flockExclusive takes a non-blocking exclusive flock on f, retrying on EINTR.
// A lock held by another file description, even in this process, is ErrLocked.
func flockExclusive(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("raw conn: %w", err)
	}
	var lockErr error
	ctrlErr := rc.Control(func(fd uintptr) {
		for {
			lockErr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
			if !errors.Is(lockErr, syscall.EINTR) {
				return
			}
		}
	})
	if ctrlErr != nil {
		return fmt.Errorf("raw conn control: %w", ctrlErr)
	}
	if errors.Is(lockErr, syscall.EWOULDBLOCK) {
		return fmt.Errorf("%w: %w", ErrLocked, lockErr)
	}
	if lockErr != nil {
		return fmt.Errorf("flock: %w", mapErr(lockErr))
	}
	return nil
}
