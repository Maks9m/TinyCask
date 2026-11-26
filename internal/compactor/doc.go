// Package compactor merges all sealed segments into one compact_<M>.data with
// only the live records, then swaps the new locations into the KeyDir
// (ARCHITECTURE §8).
package compactor
