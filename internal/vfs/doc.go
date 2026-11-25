// Package vfs defines the FS and File interfaces through which TinyCask does
// all of its disk I/O, the real-disk implementation OSFS, and the sentinel
// errors shared by every implementation (ARCHITECTURE §11).
//
// os, syscall and other direct file-system calls are allowed only in this
// package's osfs*.go files.
package vfs
