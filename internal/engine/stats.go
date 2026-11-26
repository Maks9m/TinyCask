package engine

// Stats is a point-in-time snapshot of the engine's counters.
type Stats struct {
	// Keys is the number of live keys in KeyDir.
	Keys int
	// LiveBytes is the total size of the records KeyDir points at.
	LiveBytes int64
	// DiskBytes is the total size of all data files.
	DiskBytes int64
	// GarbageRatio is 1 - LiveBytes/DiskBytes, in [0, 1]; 0 when DiskBytes is 0.
	GarbageRatio float64
	// ReadOnly is true after a failed truncate or fsync (invariant 8).
	ReadOnly bool
}

// Stats returns the current counters (final values after Close). Read without
// the writer lock, so under load fields may be a few records apart.
func (e *Engine) Stats() Stats {
	live := e.liveBytes.Load()
	disk := e.diskBytes.Load()
	return Stats{
		Keys:         e.keydir.Len(),
		LiveBytes:    live,
		DiskBytes:    disk,
		GarbageRatio: garbageRatio(live, disk),
		ReadOnly:     e.readOnly.Load(),
	}
}

// garbageRatio is clamped because live and disk are loaded separately.
func garbageRatio(live, disk int64) float64 {
	if disk <= 0 {
		return 0
	}
	return min(max(1-float64(live)/float64(disk), 0), 1)
}
