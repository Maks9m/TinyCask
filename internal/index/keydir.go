package index

import "sync"

// CommandIndex is where a key's newest record lives (ARCHITECTURE §4). Record
// offset and size are derived by storage helpers, not stored.
type CommandIndex struct {
	FileID      uint32
	ValueSize   uint32
	ValueOffset int64  // offset of the value's first byte in the file
	Timestamp   uint64 // informational; ordering is by (FileID, offset)
}

// KeyDir maps each live key to its CommandIndex. Safe for concurrent use; it
// never does I/O, so its lock is never held across disk work (ARCHITECTURE §6).
// Callers follow the lock order writer → KeyDir → Store.
type KeyDir struct {
	// mu protects index.
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
	e, ok := kd.index[string(key)] // no allocation for a map lookup
	return e, ok
}

// Put sets the entry for key and returns the previous one, if any. The key is
// copied, so the caller may reuse its slice.
func (kd *KeyDir) Put(key []byte, e CommandIndex) (prev CommandIndex, existed bool) {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	k := string(key) // copy: the map must not alias the caller's slice
	prev, existed = kd.index[k]
	kd.index[k] = e
	return prev, existed
}

// Delete removes key and returns the removed entry, if any.
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

// UpdateIf sets key to e only if key exists and cond(current) is true, and
// reports whether it did. Absent keys are never inserted: in the compactor's
// swap (ARCHITECTURE §5.5) they were deleted during the merge.
//
// cond runs under the write lock: keep it fast and don't call back into KeyDir.
func (kd *KeyDir) UpdateIf(key []byte, cond func(current CommandIndex) bool, e CommandIndex) bool {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	cur, ok := kd.index[string(key)]
	if !ok || !cond(cur) {
		return false
	}
	kd.index[string(key)] = e // copies the key
	return true
}
