package vfs

import "errors"

// ErrNoSpace reports a full device or quota. OSFS wraps ENOSPC with it.
var ErrNoSpace = errors.New("vfs: no space left on device")

// ErrLocked reports that FS.Lock found the lock already held.
var ErrLocked = errors.New("vfs: lock is held by another owner")
