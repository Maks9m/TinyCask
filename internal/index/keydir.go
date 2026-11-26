package index

import "sync"

// CommandIndex is the location of a key's newest record on disk. The fields are
// exactly the design doc's (ARCHITECTURE §4); the record's offset and size are
// derived from them by the helpers in package storage, never stored here.
type CommandIndex struct {
	FileID      uint32 // ID of the data file holding the record
	ValueSize   uint32 // length of the value in bytes
	ValueOffset int64  // absolute offset of the value's first byte in the file
	Timestamp   uint64 // informational only; ordering is by (FileID, offset)
}

// KeyDir is the concurrent in-memory map from each live key to its
// CommandIndex. It is safe for concurrent use. It never does I/O, so its lock
// is never held across a disk operation (ARCHITECTURE §6).
//
// Callers are expected to respect the engine's lock order (writer → KeyDir →
// Store); KeyDir itself calls into nothing that takes another lock.
type KeyDir struct {
	// mu protects index. Readers take RLock; Put, Delete and UpdateIf take Lock.
	mu    sync.RWMutex
	index map[string]CommandIndex
}

// New returns an empty KeyDir.
func New() *KeyDir {
	return &KeyDir{index: make(map[string]CommandIndex)}
}

// Get returns the entry for key and whether it exists.
func (kd *KeyDir) Get(key []byte) (CommandIndex, bool) {
	kd.mu.RLock()
	defer kd.mu.RUnlock()
	// The compiler does not allocate for string(key) used as a map index.
	e, ok := kd.index[string(key)]
	return e, ok
}

// Put sets the entry for key and returns the previous entry and whether one
// existed. The key bytes are copied, so the caller may reuse its slice.
func (kd *KeyDir) Put(key []byte, e CommandIndex) (prev CommandIndex, existed bool) {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	k := string(key) // copies the bytes: the map must not alias the caller's slice
	prev, existed = kd.index[k]
	kd.index[k] = e
	return prev, existed
}

// Delete removes key and returns the removed entry and whether one existed.
// Deleting an absent key is a no-op.
func (kd *KeyDir) Delete(key []byte) (prev CommandIndex, existed bool) {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	prev, existed = kd.index[string(key)]
	if existed {
		delete(kd.index, string(key))
	}
	return prev, existed
}

// Len returns the number of keys.
func (kd *KeyDir) Len() int {
	kd.mu.RLock()
	defer kd.mu.RUnlock()
	return len(kd.index)
}

// UpdateIf replaces the entry for key with e only if key is present and
// cond(current entry) returns true, and reports whether it did. An absent key
// is never inserted: this is the compactor's conditional swap (ARCHITECTURE
// §5.5), where an absent key means it was deleted during the merge and must
// stay deleted.
//
// cond runs while the KeyDir write lock is held, so it must be fast, must not
// block, and must not call back into the KeyDir.
func (kd *KeyDir) UpdateIf(key []byte, cond func(current CommandIndex) bool, e CommandIndex) bool {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	cur, ok := kd.index[string(key)]
	if !ok || !cond(cur) {
		return false
	}
	// string(key) in an assignment always copies, and Go replaces the stored
	// key with that copy, so the map never aliases the caller's slice.
	kd.index[string(key)] = e
	return true
}
