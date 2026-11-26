// Package storage owns the on-disk format: the v1 record codec, file naming,
// append-only segments, the Store and the recovery scanner (ARCHITECTURE §3,
// §5, §6). All file access goes through vfs.FS.
package storage
