---
paths:
  - "internal/storage/**"
  - "internal/index/**"
  - "internal/compactor/**"
  - "internal/engine/**"
---

# Storage engine rules

Read ARCHITECTURE §3–§8 before changing anything here. These rules are the parts that are easy to get subtly wrong.

## Record format (frozen, v1)
- Layout: `CRC32(4) | Timestamp(8) | Type(1) | KeySize(4) | ValSize(4) | Key | Value`, little-endian, header 21 bytes.
- CRC32-IEEE over everything after the CRC field. `Type` 1 = PUT, 2 = DELETE with `ValSize == 0`.
- Encode a record into **one** buffer and write it with **one** `Write` call.
- Decode order: header length → type → size limits → payload length → CRC. Reject before allocating the payload.
- Never change `testdata/` golden bytes to make a test pass. A format change needs the user's approval and a design-doc update.

## Writes
- Only the engine appends, and only while holding the writer lock.
- Inside that same critical section, in this order: append → (fsync if `sync=always`) → KeyDir update → counters.
- On a failed or short append: truncate the active file to the pre-write offset. If the truncate fails, or any fsync fails, set read-only and return the error. Do not retry fsync.
- Roll before the append that would exceed the segment limit, never after a partial one. Sealing means: `Sync`, mark immutable, create the next file, `SyncDir`.
- No `bufio.Writer` or any other userspace buffer in front of a segment file.

## Reads
- A read is: KeyDir lookup → one `ReadAt` of the whole record → decode → compare key → copy value out.
- Derive `RecordOffset`/`RecordSize` with the shared helpers; never inline the arithmetic.
- `storage.ErrSegmentNotFound` during Get means a compaction won the race: redo the lookup (bounded retries). It is not a corruption.
- Never hold `KeyDir.mu` across a disk read.

## Locks
- Order: writer → KeyDir → Store. If you need them in another order, the design is wrong; stop and ask.
- `Store.mu` write lock is held only to add or remove map entries. Close and delete files after releasing it.

## Recovery
- Delete `*.tmp` first, then apply the supersede rule (highest `compact_<M>.data` removes every data file with ID < M), then replay ascending by ID.
- Truncate a bad tail only in the highest-ID file, then `Sync` it. A bad record in any other file fails `Open` with `ErrCorrupt` that names the file and offset.
- Always create a new active segment on open; never append to a file from a previous run.

## Compaction
- Inputs are **all** sealed files. Never merge a subset: dropped tombstones would resurrect keys.
- The output ID is reserved under the writer lock, between the inputs and the new active segment.
- A record is live only if KeyDir points at exactly its `(FileID, ValueOffset)`.
- Order is fixed: write tmp → `Sync` → `Rename` → `SyncDir` → register in Store → batched `UpdateIf` → drop inputs → delete files → `SyncDir`.
- The index swap takes `KeyDir.Lock()` in batches of at most 1024 keys.
- Any failure before the rename removes the `.tmp` and leaves the engine fully usable.
- One compaction at a time; it must stop promptly when its context is cancelled.

## Accounting
- `liveBytes` changes only where KeyDir changes; `diskBytes` only where file sizes change. Keep each pair of updates adjacent.
