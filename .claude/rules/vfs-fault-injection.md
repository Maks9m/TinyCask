---
paths:
  - "internal/vfs/**"
---

# VFS and fault-injection rules

## Interface discipline
- `vfs.FS` and `vfs.File` are deliberately tiny (ARCHITECTURE §11). Do not add a method unless a ticket says so; every method must be implemented and conformance-tested for both `OSFS` and `SimFS`.
- This is the only package tree allowed to import `os` file APIs and `syscall`. Platform code goes in `osfs_unix.go` behind a `//go:build unix` tag.
- `File.Write` always appends. `Create` fails if the file exists. `Rename` atomically replaces the target. `List` returns sorted base names.
- Return `fs.ErrNotExist` / `fs.ErrExist` (wrapped) for missing and duplicate files so callers can use `errors.Is`.
- `OSFS` translates `ENOSPC` into `vfs.ErrNoSpace` and a held lock into `vfs.ErrLocked`. No other error translation.

## SimFS semantics
- Purely in memory, safe for concurrent use, and fully deterministic: no wall clock, no global `rand`. Any randomness comes from a seed passed by the test.
- Reads see all written bytes immediately, synced or not, exactly like the page cache.
- An open handle keeps working after its file is renamed or removed, as on POSIX.
- Each file tracks its synced length. `Restart(LoseUnsynced)` cuts every file back to that length; plain `Restart()` keeps everything (process crash, not power loss).
- Namespace operations (create, rename, remove) are immediately durable. This is a documented simplification; do not build directory-entry durability.

## Faults (exactly these four)
1. Fail the Nth call of a given operation with a given error.
2. Tear a write at global byte N, apply the prefix, then crash.
3. Disk quota: write the bytes that fit, return a short count with `vfs.ErrNoSpace`.
4. Discard unsynced bytes on restart.

- After a crash, **every** call on the FS and on every previously opened file returns `simfs.ErrCrashed` until `Restart`. Handles opened before the crash stay dead afterwards.
- A fault fires once and is then cleared, unless the test arms it again.
- Fault configuration is done through methods on `*SimFS`, never through package globals or environment variables.

## Tests
- The conformance suite is a function `RunFSTests(t, newFS func(t) vfs.FS)` invoked for both implementations. New `FS` behaviour is specified there first.
- `OSFS` tests use `t.TempDir()` only.
