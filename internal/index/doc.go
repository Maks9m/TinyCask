// Package index implements the KeyDir: the concurrent in-memory map from each
// live key to the location of its newest record on disk (ARCHITECTURE §4).
//
// It does no I/O and imports no other TinyCask package.
package index
