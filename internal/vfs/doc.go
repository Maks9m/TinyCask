// Package vfs defines the FS and File interfaces for all disk I/O, the real-disk
// OSFS, and shared sentinel errors (ARCHITECTURE §11).
//
// Direct os/syscall calls are allowed only in osfs*.go.
package vfs
