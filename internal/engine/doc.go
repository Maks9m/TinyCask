// Package engine is the embedded store (Open, Put, Get, Delete, Compact,
// Close). It owns the writer lock, recovery, fsync policy and compaction
// scheduling (ARCHITECTURE §5–§8).
package engine
