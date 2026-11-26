package vfs

import "errors"

// ErrNoSpace reports that the device or quota is full. OSFS returns it, wrapped
// together with the original error, for ENOSPC.
var ErrNoSpace = errors.New("vfs: no space left on device")

// ErrLocked reports that FS.Lock found the lock already held by another owner.
var ErrLocked = errors.New("vfs: lock is held by another owner")
