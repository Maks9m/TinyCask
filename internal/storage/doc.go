// Package storage owns the on-disk format: the frozen v1 record codec, data
// file naming, append-only segments, the set of open segments (Store) and the
// sequential scanner used by recovery (ARCHITECTURE §3, §5, §6).
//
// All file access goes through vfs.FS.
package storage
