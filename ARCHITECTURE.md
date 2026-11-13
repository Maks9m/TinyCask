# TinyCask Architecture

Technical blueprint for the product requirements in [docs/design-doc.md](docs/design-doc.md) (v1.1).
The design doc says **what** and **why**; this file says **how**. If the two disagree, the design doc wins and this file gets fixed.

- Module: `github.com/Maks9m/TinyCask` · Go 1.27 · stdlib only at runtime, `testify` in tests
- Platforms: macOS and Linux (no Windows)

---

## 1. System overview

TinyCask is a single-node Bitcask-style key-value store: every write is appended to a log file, and an in-memory hash map (KeyDir) points each live key at its newest record on disk. A read is one map lookup plus one `pread`.

```text
 redis-cli / redis-benchmark / nc
              │  TCP :6380 (RESP2 subset + inline text)
              ▼
┌───────────────────────────┐
│ internal/server           │  goroutine per connection, parse → dispatch → reply
└─────────────┬─────────────┘
              ▼
┌───────────────────────────┐
│ internal/engine           │  façade: Open/Put/Get/Delete/Compact/Close
│  writer lock · recovery   │  background: fsync ticker, compaction trigger
└───┬───────────┬───────────┘
    │           │            ┌──────────────────────┐
    │           └───────────►│ internal/compactor   │ merge sealed segments
    ▼                        └───┬──────────────┬───┘
┌──────────────────┐             │              │
│ internal/index   │◄────────────┘              ▼
│ KeyDir (RWMutex) │             ┌──────────────────────────┐
└──────────────────┘             │ internal/storage         │ record codec,
                                 │ segments, scanner        │ segment set
                                 └────────────┬─────────────┘
                                              ▼
                                 ┌──────────────────────────┐
                                 │ internal/vfs             │ FS + File interfaces
                                 │  OSFS │ simfs.SimFS      │ real disk │ fault injection
                                 └──────────────────────────┘
```

## 2. Package layout and dependency rules

```text
cmd/tinycask/            main: flags, logging, wiring, signals
internal/vfs/            FS and File interfaces, OSFS, sentinel errors
internal/vfs/simfs/      in-memory FS with deterministic fault injection (tests only)
internal/index/          KeyDir
internal/storage/        record codec, file naming, segment, segment set (Store), scanner
internal/compactor/      merge algorithm
internal/engine/         façade, recovery, durability, compaction scheduling, errors
internal/server/         RESP parser/writer, TCP server, command dispatch
scripts/                 bench.sh (redis-benchmark driver)
```

Allowed imports (anything not listed is forbidden):

| Package | May import (besides stdlib) |
| --- | --- |
| `vfs` | nothing |
| `vfs/simfs` | `vfs` |
| `index` | nothing |
| `storage` | `vfs` |
| `compactor` | `storage`, `index` |
| `engine` | `storage`, `index`, `compactor`, `vfs` |
| `server` | `engine` (error sentinels and `Stats` only; the store itself is a consumer-defined interface) |
| `cmd/tinycask` | `engine`, `server`, `vfs` |

Hard rules:

1. **All disk I/O goes through `vfs.FS`.** `os` file functions, `syscall` and `io/ioutil` appear only in `internal/vfs/osfs*.go` (and `os` for flags/signals in `cmd/`). Enforced by `depguard` in `.golangci.yml`.
2. `simfs` is imported only from `_test.go` files.
3. `server` never imports `storage`, `index`, `compactor` or `vfs`.
4. No package-level mutable state. No `init()` side effects.

## 3. On-disk format ("schema")

There is no SQL database and there are no migrations. The schema is the record layout and the data-directory layout below. Both are **format v1 and frozen**: a golden-file test pins the exact bytes, and an incompatible change requires a new file extension plus a design-doc update.

### 3.1 Record (frame)

```text
| CRC32 4B | Timestamp 8B | Type 1B | KeySize 4B | ValSize 4B | Key | Value |
|<----------------------- header: 21 bytes ------------------>|
```

- Little-endian integers. CRC32 uses the IEEE polynomial and covers every byte after the CRC field.
- `Type`: `1` = PUT, `2` = DELETE (tombstone, `ValSize` must be 0). Anything else is corruption.
- Limits, checked on write **and before allocating on read**: key 1–1024 bytes, value 0–1 MiB.
- `Timestamp` is Unix nanoseconds, informational only. Ordering is always `(FileID, offset)`.

### 3.2 Data directory

| File | Meaning |
| --- | --- |
| `seg_<id>.data` | Regular segment. The highest ID is the active segment. |
| `compact_<id>.data` | Compaction output. **Supersedes every data file with a lower ID.** |
| `compact_<id>.tmp` | Unfinished compaction. Deleted at startup. |
| `LOCK` | Exclusive advisory lock held for the engine's lifetime. |

`<id>` is a `uint32` rendered as 10 zero-padded decimal digits. IDs only grow; gaps are normal.

## 4. In-memory data model

```go
// internal/index
type CommandIndex struct {      // unchanged from the design doc
    FileID      uint32
    ValueSize   uint32
    ValueOffset int64
    Timestamp   uint64
}
```

- `RecordOffset = ValueOffset - 21 - len(key)`, `RecordSize = 21 + len(key) + ValueSize`. Helpers for both live next to the codec in `storage`; nobody re-derives them by hand.
- KeyDir operations: `Get`, `Put` (returns the previous entry), `Delete` (returns the previous entry), `Len`, and `UpdateIf(key, cond, new)` for the compactor's conditional swap. KeyDir owns its `RWMutex` and never does I/O.
- Engine counters (atomics): `liveBytes` = total size of records KeyDir points at; `diskBytes` = total size of all data files. Garbage ratio = `1 - liveBytes/diskBytes`.

## 5. Core flows

### 5.1 Put

1. Validate sizes; encode the full frame into one buffer (outside any lock).
2. Take the **writer lock**.
3. If the engine is read-only or closed, fail. If `active.size + len(frame) > MaxSegmentSize`, roll: fsync and seal the active segment, create the next one, `SyncDir`.
4. Append the frame with a single `Write`.
   - On error or short write: `Truncate` the active file back to the pre-write offset. If truncate fails, enter read-only mode. Map out-of-space to `ErrNoSpace`. KeyDir is untouched.
5. If `sync=always`: `Sync()`. On failure, enter read-only mode and fail the write.
6. `KeyDir.Put`; adjust `liveBytes`/`diskBytes`.
7. Release the writer lock.

### 5.2 Get

1. `KeyDir.Get(key)`; miss → `ErrKeyNotFound`.
2. `Store.ReadAt(FileID, RecordOffset, RecordSize)`: one `pread` of the whole record.
3. Decode, verify CRC and that the stored key equals the requested key → return a copy of the value. Mismatch → `ErrCorrupt`.
4. If the store reports the segment is gone (a compaction dropped it between steps 1 and 2), go to step 1. At most 3 attempts.

Get never takes the writer lock.

### 5.3 Delete

Under the writer lock: if the key is absent return `ErrKeyNotFound` and write nothing; otherwise append a tombstone (same failure handling as Put) and `KeyDir.Delete`.

### 5.4 Open (recovery)

1. `MkdirAll`, take `LOCK` (held → `ErrLocked`).
2. List the directory. Delete every `*.tmp`. If any `compact_<M>.data` exists, take the highest `M` and delete every data file with ID < `M`. `SyncDir`.
3. Replay remaining files in ascending ID order, each from offset 0: PUT → `KeyDir.Put`, tombstone → `KeyDir.Delete`; maintain `liveBytes`.
4. A decode failure (short header, length over limit, short payload, CRC mismatch, unknown type):
   - in the **highest-ID file**: truncate to the last good offset, `Sync`, log a warning, continue;
   - in **any other file**: fail `Open` with `ErrCorrupt` naming the file and offset.
5. Create a new active segment with ID `max+1` (or 1). `SyncDir`.
6. Start background goroutines.

Startup always creates a fresh active segment and never appends to a file from a previous run.

### 5.5 Compaction

Engine side (single-flight; a second request returns `ErrCompactionRunning`):

1. Under the writer lock: roll the active segment `A` (fsync + seal), reserve `M = A+1` for the output, create the new active segment `A+2`. Inputs = every sealed file, all with ID < `M`.

Compactor side (`compactor.Run`), no writer lock held:

2. Create `compact_<M>.tmp`. Scan inputs in ascending ID order. For each PUT record: it is live iff `KeyDir.Get(key)` equals exactly this record's `(FileID, ValueOffset)`. Copy live records to the output and remember `(key, new ValueOffset, ValueSize, Timestamp)` in a pending list. Tombstones and dead records are skipped.
3. `Sync` the output, `Rename` to `compact_<M>.data`, `SyncDir`. From this instant the output is authoritative on disk.
4. Register the output in the Store as readable file `M`.
5. Swap the index in batches of ≤ 1024 keys per `KeyDir.Lock()`: `UpdateIf(key, entry.FileID < M, newEntry)`. Keys overwritten (FileID > M) or deleted (absent) during the merge are left alone; their copied records are just garbage in the output.
6. Remove the inputs from the Store (after this, readers that still hold an old entry get "segment gone" and retry), close and delete the files, `SyncDir`. Recompute `diskBytes`.

Failure handling: any error or context cancellation before step 3 deletes the `.tmp` and leaves everything as it was; the engine stays writable. A crash after step 3 is completed by recovery step 2.

Why it is safe:
- The output's ID is higher than every input and lower than every segment written during or after the merge, so replay order equals write order.
- Compaction always merges **all** older files, so dropping tombstones cannot resurrect a key from a file that was left out.
- The output is fsynced before the rename, because after the rename the inputs may be deleted.

Trigger: a ticker (default 10 s) starts a compaction when garbage ratio ≥ 0.5 and `diskBytes` ≥ 16 MiB; the `COMPACT` command starts one unconditionally and waits for it.

### 5.6 Close

Cancel the background context and wait for the goroutines (an in-flight compaction aborts and removes its `.tmp`). Under the writer lock: `Sync` the active segment, close all files, release `LOCK`. Later calls return `ErrClosed`. Close is idempotent.

## 6. Concurrency model

| Lock | Owner | Protects | Held during I/O? |
| --- | --- | --- | --- |
| writer `sync.Mutex` | `engine` | append + matching KeyDir update, roll, read-only transition | yes (the append itself) |
| `KeyDir.mu` `sync.RWMutex` | `index` | the map | **never** |
| `Store.mu` `sync.RWMutex` | `storage` | the segment set (ID → open file) | read lock: yes, for the `pread`; write lock: only for map changes |

- **Lock order: writer → KeyDir → Store.** Never acquire in the reverse direction. Never call into the engine while holding `KeyDir.mu`.
- The interval fsync grabs the active file under the writer lock, releases it, then calls `Sync` outside the lock so writers are not stalled.
- Files are closed only after they are removed from the Store under its write lock, which guarantees no read is in flight on them.
- Goroutines: accept loop, one per connection, fsync ticker (`sync=interval` only), compaction ticker. Every one is tied to a `context.Context` and a `sync.WaitGroup`; `Close`/shutdown waits for all of them. No goroutine outlives its owner.
- Shared flags (`closed`, `readOnly`, compaction-running) and counters are `sync/atomic`.

## 7. Durability

| `sync` | When data is fsynced | Loss on power failure |
| --- | --- | --- |
| `always` | after every append, before the reply | none |
| `interval` (default, 1 s) | background ticker | ≤ one interval |
| `never` | roll, compaction, Close | OS-dependent |

Regardless of policy: a segment is fsynced before it is sealed, a compaction output before its rename, and the directory after every create, rename and delete. Appends are one unbuffered `Write`, so an acknowledged write always survives a process crash. An fsync error is never retried: the engine goes read-only.

## 8. Error handling

- Sentinels live in `engine`: `ErrKeyNotFound`, `ErrInvalidKey`, `ErrKeyTooLarge`, `ErrValueTooLarge`, `ErrNoSpace`, `ErrReadOnly`, `ErrClosed`, `ErrCorrupt`, `ErrLocked`, `ErrCompactionRunning`. Lower layers define their own (`vfs.ErrNoSpace`, `vfs.ErrLocked`, `storage.ErrCorrupt`, `storage.ErrSegmentNotFound`) and `engine` translates them at its boundary.
- Wrap with `fmt.Errorf("context: %w", err)`; compare with `errors.Is`/`errors.As`, never by string.
- No `panic` on any input from disk or network. `panic` is acceptable only for programmer errors that tests catch (impossible states).
- Every returned error is handled or returned. `Close` errors on write paths are checked. Errors are logged once, at the boundary that handles them (`log/slog`).
- Read-only mode is one-way for the life of the process; a restart runs recovery.

## 9. Wire protocol (API contract)

Requests are either a RESP2 array of bulk strings (binary-safe) or one inline line `<COMMAND> <KEY> [<VALUE>]\r\n` split on single spaces. Command names are case-insensitive.

| Command | Reply | Notes |
| --- | --- | --- |
| `PING [msg]` | `+PONG` or bulk `msg` | |
| `SET k v` / `PUT k v` | `+OK` | |
| `GET k` | bulk value, or `$-1` if absent | |
| `DEL k` / `DELETE k` | `:1` deleted, `:0` absent | exactly one key |
| `COMPACT` | `+OK` when the merge finishes | `-ERR busy` if one is running |
| `INFO` | bulk string of `name:value` lines | keys, segments, disk_bytes, live_bytes, garbage_ratio, read_only |
| `QUIT` | `+OK`, then close | |

Errors are `-ERR <code> <detail>`:

| Engine error | Code |
| --- | --- |
| `ErrNoSpace` | `no_space` |
| `ErrReadOnly` | `read_only` |
| `ErrInvalidKey`, `ErrKeyTooLarge`, `ErrValueTooLarge`, wrong argument count | `invalid_argument` |
| `ErrCorrupt` | `corrupt` |
| `ErrCompactionRunning` | `busy` |
| unknown command | `unknown_command` |
| anything else | `internal` (details go to the log, not the client) |

Parser limits: inline line ≤ 64 KiB, ≤ 8 array elements, bulk length ≤ max value size. A framing violation gets `-ERR protocol ...` and the connection is closed. Replies are buffered and flushed when the read buffer is empty, so pipelined requests work.

Server behaviour: at most `MaxConns` connections (extra ones are closed immediately); idle read deadline 5 min; write deadline 10 s. On SIGINT/SIGTERM: stop accepting, let in-flight commands finish (up to 5 s), then `engine.Close()`.

## 10. Engine API

```go
func Open(fsys vfs.FS, dir string, opts Options) (*Engine, error)
func (e *Engine) Put(key, value []byte) error
func (e *Engine) Get(key []byte) ([]byte, error)
func (e *Engine) Delete(key []byte) error
func (e *Engine) Compact(ctx context.Context) error
func (e *Engine) Stats() Stats
func (e *Engine) Close() error
```

All methods are safe for concurrent use. `Get` returns a slice the caller owns. The engine does not retain `key`/`value` after returning.

## 11. VFS contract

```go
type FS interface {
    Create(name string) (File, error)      // new file, read + append; fails if it exists
    Open(name string) (File, error)        // existing file, read-write
    Remove(name string) error
    Rename(oldName, newName string) error  // atomic replace
    List(dir string) ([]string, error)     // base names, sorted
    MkdirAll(dir string) error
    SyncDir(dir string) error
    Lock(name string) (io.Closer, error)   // exclusive; ErrLocked if held
}

type File interface {
    io.Writer    // always appends
    io.ReaderAt
    io.Closer
    Sync() error
    Truncate(size int64) error
    Size() (int64, error)
}
```

- `OSFS` maps `ENOSPC` to `vfs.ErrNoSpace` and implements `Lock` with `flock`.
- `simfs.SimFS` keeps files in memory and offers four deterministic faults: fail the Nth call of an operation; tear a write at global byte N and crash; a disk quota (partial write + `ErrNoSpace`); and on restart, optionally discard bytes that were never `Sync`ed. After a crash every call returns `ErrCrashed` until `Restart()`. Namespace operations (create, rename, remove) are treated as immediately durable — a documented simplification.
- One conformance test suite runs against both implementations.

## 12. Configuration

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `:6380` | listen address |
| `-dir` | `./data` | data directory |
| `-sync` | `interval` | `always` \| `interval` \| `never` |
| `-sync-interval` | `1s` | fsync period for `interval` |
| `-max-segment-size` | `64MiB` | roll threshold |
| `-max-key-size` / `-max-value-size` | `1KiB` / `1MiB` | size limits |
| `-compact-ratio` | `0.5` | garbage ratio that triggers a merge |
| `-compact-min-bytes` | `16MiB` | no automatic merge below this size |
| `-compact-interval` | `10s` | how often the trigger is evaluated (`0` disables) |
| `-max-conns` | `1024` | connection limit |

Tests shrink the size options (for example 4 KiB segments) to exercise rolls and merges quickly.

## 13. Testing architecture

| Kind | Where | Notes |
| --- | --- | --- |
| Unit | each package | codec round-trip, golden bytes, KeyDir under `-race`, parser tables |
| Conformance | `internal/vfs` | same suite for `OSFS` (`t.TempDir()`) and `SimFS` |
| Integration | `internal/engine` | engine + storage + compactor on `SimFS` and on `OSFS` |
| Fault injection | `internal/engine` | the three design-doc scenarios, plus crash-after-rename |
| Model test | `internal/engine` | seeded random ops and crash points vs. a `map` model |
| Fuzz | `storage` (`FuzzDecodeRecord`), `server` (`FuzzParseFrame`) | never panic, never over-allocate |
| Benchmarks | `internal/engine`, `scripts/bench.sh` | report-only; results in `BENCHMARKS.md` |

A shared test helper checks the design doc's invariants after every recovery: KeyDir size equals the number of live keys in the log, and every entry decodes with a valid CRC.

## 14. MVP boundaries

**In scope:** everything in sections 3–12.

**Out of scope (do not build):** TTL, key listing or scans, multi-key commands, transactions, replication, hint files, group commit for `sync=always`, on-disk checksums beyond the record CRC, compression, authentication or TLS, metrics endpoints, Windows support, a custom load generator (use `redis-benchmark`).

**Known limitations accepted for v1:**
- Startup time is proportional to total data size (full replay).
- `sync=always` throughput is bounded by the device's fsync latency (very low on macOS).
- Compaction temporarily needs free disk space equal to the live data, and transient RAM for the pending list (roughly 32 bytes plus key per live key).
- Corruption in a sealed segment stops the database from opening; there is no salvage mode.

## 15. Decision log

| Decision | Rejected alternative | Reason |
| --- | --- | --- |
| Writer lock covers append **and** KeyDir update | separate locks | separate locks let the index and the log disagree on write order |
| Compaction output gets a reserved ID between inputs and new writes | reuse the highest input ID | reuse makes a `FileID` ambiguous while both files are open |
| Output supersedes all lower IDs | manifest file | one filename rule gives crash-safe cleanup with no extra file |
| Explicit `Type` byte | `ValSize = 0` as tombstone | empty values must be storable |
| Fail-stop on mid-log corruption | skip the bad record | lengths in a corrupt header cannot be trusted, so resync is guesswork |
| RESP2 subset + inline text | custom binary protocol | binary-safe, and `redis-cli`/`redis-benchmark` work unmodified |
| `KeyDir` keeps the design doc's struct | store record offset/size | both are derivable; no change to the approved model |
| Retry on "segment gone" in Get | hold a lock across lookup and read | keeps disk reads out of the index lock |
