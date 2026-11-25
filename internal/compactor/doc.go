// Package compactor merges every sealed segment into a single compact_<M>.data
// file that keeps only the records the KeyDir still points at, then swaps the
// new locations into the index (ARCHITECTURE §8).
package compactor
