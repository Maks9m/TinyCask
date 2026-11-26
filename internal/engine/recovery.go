package engine

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"

	"github.com/Maks9m/TinyCask/internal/index"
	"github.com/Maks9m/TinyCask/internal/storage"
	"github.com/Maks9m/TinyCask/internal/vfs"
)

// recoveryPlan is decided from the directory listing alone, before any file
// is opened or changed.
type recoveryPlan struct {
	// remove: every *.tmp and every file superseded by the highest
	// compact_<M>.data (invariant 6).
	remove []storage.FileName
	// replay: the rest, by ascending ID = write order (invariant 4).
	replay []storage.FileName
	nextID uint32 // new active segment: one above every file seen
}

// tailCut is a torn or corrupt tail in the highest-ID file.
type tailCut struct {
	name   storage.FileName
	offset int64 // end of the last valid record; cut back to here
	size   int64 // size before the cut
}

// recover rebuilds KeyDir and counters and creates the active segment
// (ARCHITECTURE §5.4). Runs inside Open before sharing, so no writer lock.
//
// All files are scanned before anything on disk changes, so corruption in a
// sealed file leaves the directory untouched. Then: remove stale files,
// repair the tail, register files, create the active segment. Each step is
// idempotent, so after an I/O error the next Open finishes the job.
func (e *Engine) recover(fsys vfs.FS) error {
	names, err := fsys.List(e.dir)
	if err != nil {
		return fmt.Errorf("engine: list %s: %w", e.dir, err)
	}
	plan, err := planRecovery(names)
	if err != nil {
		return fmt.Errorf("engine: recover %s: %w", e.dir, err)
	}

	var cut *tailCut
	for i, name := range plan.replay {
		last := i == len(plan.replay)-1
		if cut, err = e.replayFile(fsys, name, last); err != nil {
			return err
		}
	}

	if err = e.removeStale(fsys, plan.remove); err != nil {
		return err
	}
	if cut != nil {
		if err = e.repairTail(fsys, *cut); err != nil {
			return err
		}
	}
	for _, name := range plan.replay {
		if err = e.store.Register(name); err != nil {
			return translate(fmt.Errorf("engine: recover: register %s: %w", name, err))
		}
	}
	if err = e.store.CreateActive(plan.nextID); err != nil {
		return translate(fmt.Errorf("engine: create active segment %d: %w", plan.nextID, err))
	}
	// Registered sizes already reflect the tail cut.
	e.diskBytes.Store(e.store.DiskBytes())
	return nil
}

// planRecovery applies the naming rules (ARCHITECTURE §5.4 step 2). Non-data
// names like LOCK are ignored; a malformed data name fails.
func planRecovery(names []string) (recoveryPlan, error) {
	var (
		files      []storage.FileName
		maxID      uint32
		compactID  uint32
		hasCompact bool
	)
	for _, base := range names {
		name, ok, err := storage.ParseFileName(base)
		if err != nil {
			return recoveryPlan{}, err
		}
		if !ok {
			continue
		}
		files = append(files, name)
		maxID = max(maxID, name.ID)
		if name.Kind == storage.KindCompact && (!hasCompact || name.ID > compactID) {
			compactID, hasCompact = name.ID, true
		}
	}

	plan := recoveryPlan{nextID: firstSegmentID}
	if len(files) > 0 {
		// *.tmp counts too, so a reserved ID is never reused.
		if maxID == math.MaxUint32 {
			return recoveryPlan{}, fmt.Errorf("file IDs exhausted at %d", maxID)
		}
		plan.nextID = maxID + 1
	}
	for _, f := range files {
		if f.Kind == storage.KindCompactTmp || (hasCompact && f.ID < compactID) {
			plan.remove = append(plan.remove, f)
			continue
		}
		plan.replay = append(plan.replay, f)
	}
	// The listing is sorted by name (compact_* before seg_*), so re-sort by ID.
	slices.SortFunc(plan.replay, func(a, b storage.FileName) int { return cmp.Compare(a.ID, b.ID) })
	for i := 1; i < len(plan.replay); i++ {
		if a, b := plan.replay[i-1], plan.replay[i]; a.ID == b.ID {
			// KeyDir entries name files by ID only, so IDs must be unique.
			return recoveryPlan{}, fmt.Errorf("%w: %s and %s share file ID %d", ErrCorrupt, a, b, a.ID)
		}
	}
	return plan, nil
}

// replayFile scans one file into KeyDir and liveBytes (read-only).
//
// Corruption in the last file is a torn tail: earlier records count and a
// tailCut is returned (invariant 9). Elsewhere it is ErrCorrupt with file and
// offset. An I/O error says nothing about the bytes, so it fails without a cut.
func (e *Engine) replayFile(fsys vfs.FS, name storage.FileName, last bool) (*tailCut, error) {
	path := filepath.Join(e.dir, name.String())
	f, err := fsys.Open(path)
	if err != nil {
		return nil, fmt.Errorf("engine: recover: open %s: %w", path, err)
	}
	size, scanErr := f.Size()
	var sc *storage.Scanner
	if scanErr == nil {
		sc = storage.NewScanner(f, size, storage.ScanOptions{Limits: e.limits})
		scanErr = e.replayRecords(sc, name.ID)
	}
	// Read-only handle: a failed Close loses nothing, but still report it.
	if cerr := f.Close(); cerr != nil {
		return nil, errors.Join(scanErr, fmt.Errorf("engine: recover: close %s: %w", path, cerr))
	}
	switch {
	case scanErr == nil:
		return nil, nil
	case sc == nil:
		return nil, fmt.Errorf("engine: recover: size of %s: %w", path, scanErr)
	case !errors.Is(scanErr, storage.ErrCorrupt):
		return nil, fmt.Errorf("engine: recover: scan %s: %w", path, scanErr)
	case last:
		return &tailCut{name: name, offset: sc.Offset(), size: size}, nil
	default:
		return nil, translate(fmt.Errorf("engine: recover: file %s is corrupt at offset %d: %w",
			path, sc.Offset(), scanErr))
	}
}

// replayRecords applies records in order: PUT points the key here, DELETE
// removes it. Returns sc.Err().
func (e *Engine) replayRecords(sc *storage.Scanner, fileID uint32) error {
	for sc.Next() {
		rec := sc.Record()
		switch rec.Type {
		case storage.TypePut:
			valueSize := uint32(len(rec.Value)) // safe: bounded by MaxValueSize
			prev, existed := e.keydir.Put(rec.Key, index.CommandIndex{
				FileID:      fileID,
				ValueSize:   valueSize,
				ValueOffset: sc.ValueOffset(),
				Timestamp:   rec.Timestamp,
			})
			e.liveBytes.Add(storage.RecordSize(len(rec.Key), valueSize) - e.entrySize(rec.Key, prev, existed))
		case storage.TypeDelete:
			prev, existed := e.keydir.Delete(rec.Key)
			e.liveBytes.Add(-e.entrySize(rec.Key, prev, existed))
		default:
			// Decode rejects unknown types; reaching here is a codec bug.
			return fmt.Errorf("%w: file %d offset %d: record type %d", ErrCorrupt, fileID, sc.RecordOffset(), rec.Type)
		}
	}
	return sc.Err()
}

// removeStale deletes *.tmp and superseded files, then syncs the dir so the
// deletes are durable (invariant 7).
func (e *Engine) removeStale(fsys vfs.FS, stale []storage.FileName) error {
	if len(stale) == 0 {
		return nil
	}
	for _, name := range stale {
		path := filepath.Join(e.dir, name.String())
		if err := fsys.Remove(path); err != nil {
			return fmt.Errorf("engine: recover: remove %s: %w", path, err)
		}
	}
	if err := fsys.SyncDir(e.dir); err != nil {
		return fmt.Errorf("engine: recover: sync directory %s: %w", e.dir, err)
	}
	return nil
}

// repairTail truncates the highest-ID file to its last valid record and fsyncs
// it (invariants 7, 9). Failure fails Open: the file's state is unknown.
func (e *Engine) repairTail(fsys vfs.FS, cut tailCut) error {
	path := filepath.Join(e.dir, cut.name.String())
	f, err := fsys.Open(path)
	if err != nil {
		return fmt.Errorf("engine: recover: open %s for repair: %w", path, err)
	}
	var werr error
	if err = f.Truncate(cut.offset); err != nil {
		werr = fmt.Errorf("engine: recover: truncate %s to %d: %w", path, cut.offset, err)
	} else if err = f.Sync(); err != nil {
		werr = fmt.Errorf("engine: recover: sync %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		werr = errors.Join(werr, fmt.Errorf("engine: recover: close %s: %w", path, err))
	}
	if werr != nil {
		return werr
	}
	e.opts.Logger.Warn("engine: truncated torn tail",
		"file", path, "offset", cut.offset, "dropped_bytes", cut.size-cut.offset)
	return nil
}
