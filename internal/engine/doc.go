// Package engine is the embedded TinyCask store: Open, Put, Get, Delete,
// Compact and Close on top of storage, index and compactor. It owns the
// writer lock, crash recovery, the fsync policy and compaction scheduling
// (ARCHITECTURE §5–§8).
package engine
