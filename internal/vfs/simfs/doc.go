// Package simfs is an in-memory vfs.FS with deterministic fault injection:
// failing the Nth call of an operation, tearing a write and crashing, a disk
// quota, and discarding unsynced bytes on restart (ARCHITECTURE §11).
//
// It exists for tests only and must never be imported from a non-test file.
package simfs
