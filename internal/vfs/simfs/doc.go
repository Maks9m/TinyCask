// Package simfs is an in-memory vfs.FS with deterministic fault injection
// (failed calls, torn writes, disk quota, lost unsynced bytes; ARCHITECTURE §11).
//
// Tests only: never import it from a non-test file.
package simfs
