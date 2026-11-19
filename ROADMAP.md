# TinyCask Roadmap

Four weeks, one developer, part-time: **15–20 hours per week, about 3 hours a day**. Implementation is done ticket by ticket with Claude Opus 5.5 in Claude Code; the hours below are *your* time (prompting, reviewing the diff, running the verification), not model time.

## How to use this file

1. Pick the next unchecked ticket whose dependencies are done.
2. Start a fresh Claude Code session and say: **"Implement ticket T2.3 from ROADMAP.md."** Nothing else is needed; `CLAUDE.md` tells Opus to read the ticket and the architecture sections it names.
3. When Opus reports done, run the ticket's **Verification Command** yourself, skim the diff, then commit (`T2.3: rebuild KeyDir on open`).
4. Clear the session before the next ticket.

Each ticket is sized for one or two focused sessions. If a ticket is still not green after two sessions, stop and split it instead of pushing on.

## Plan at a glance

| Week | Theme | Tickets | Hours | You can demo |
| --- | --- | --- | --- | --- |
| 1 | Foundations | T1.1–T1.6 | 15 | Records appended to a segment file and scanned back, on real disk |
| 2 | Crash-safe engine | T2.1–T2.5 | 16 | Embedded engine that survives torn writes and a full disk in simulation |
| 3 | Compaction and server | T3.1–T3.6 | 17.5 | `redis-cli -p 6380 SET a 1`, kill -9, restart, `GET a`; compaction reclaims space |
| 4 | Proof and polish | T4.1–T4.5 + buffer | 14.5 + 3 | Model-checked crash safety, benchmark report, tagged `v0.1.0` |

Total: about 63 hours of tickets plus a 3-hour buffer, inside the 60–80 hour budget.

Critical path: T1.1 → T1.2/T1.3 → T1.5 → T1.6 → T2.2 → T2.3 → T2.5 → T3.1 → T3.2 → T3.3 → T3.5 → T3.6 → week 4.
Off the critical path (do them whenever their dependency is met, for example on a short day): T1.4, T2.1, T2.4, T3.4.

**Before week 1 (10 minutes, by hand):** `brew install golangci-lint`, and confirm `go version` prints 1.27.x.

---

## Week 1 — Foundations (15 h)

Goal: the leaf packages exist, are tested in isolation, and the toolchain is green from the first commit.

### T1.1 — Repository scaffolding (2 h)
- [ ] Done
- **Goal:** A buildable, lintable, testable empty project so every later ticket starts from `make check` passing.
- **Files/Packages Touched:** `go.mod`, `Makefile`, `.golangci.yml`, `.gitignore`, `.github/workflows/ci.yml`, `cmd/tinycask/main.go`, `doc.go` in each package listed in ARCHITECTURE §2.
- **Dependencies:** none.
- **Acceptance Criteria:**
  - `go.mod` declares `module github.com/Maks9m/TinyCask` and `go 1.27`.
  - `Makefile` has exactly the targets in `CLAUDE.md` (build, test, lint, cover, fuzz, bench, run, check); `fuzz` and `bench` may be no-ops until their packages exist.
  - `.golangci.yml` is valid for the installed golangci-lint major version (check `golangci-lint --version` first) and enables at least: errcheck, govet, staticcheck, unused, ineffassign, errorlint, gocritic, misspell, nolintlint (explanation required), and depguard rules that forbid `os`, `syscall` and `io/ioutil` in `internal/{index,storage,compactor,engine,server}` and forbid `internal/vfs/simfs` in non-test files.
  - `cmd/tinycask -version` prints a version string and exits 0.
  - CI runs build, vet, lint and `go test -race ./...` on push and pull request.
  - The existing `.gitignore` still covers `bin/`, `data/`, `coverage.out`, `*.test`, `.claude/settings.local.json`; extend it only if the scaffolding produces new artefacts.
- **Verification Command:** `make check && go run ./cmd/tinycask -version`

### T1.2 — VFS interfaces and OSFS (2.5 h)
- [ ] Done
- **Goal:** The file-system abstraction from ARCHITECTURE §11 with a real-disk implementation and a reusable conformance suite.
- **Files/Packages Touched:** `internal/vfs/` (`vfs.go`, `errors.go`, `osfs.go`, `osfs_unix.go`, `vfstest/suite.go`, tests).
- **Dependencies:** T1.1.
- **Acceptance Criteria:**
  - `FS` and `File` match ARCHITECTURE §11 exactly; sentinels `ErrNoSpace`, `ErrLocked` exist.
  - `OSFS`: `Create` fails on an existing file; `Write` appends even after `Truncate`; `Rename` replaces atomically; `List` is sorted; `SyncDir` fsyncs the directory; `Lock` is exclusive (a second `Lock` returns `ErrLocked`, and succeeds after the first is closed).
  - `vfstest.RunFSTests(t, factory)` covers every method including error cases (missing file, duplicate create, read past EOF).
  - `OSFS` passes the suite under `t.TempDir()`.
- **Verification Command:** `go test -race -count=1 ./internal/vfs/... && make check`

### T1.3 — Record codec (2.5 h)
- [ ] Done
- **Goal:** Encode and decode the frozen v1 record format (ARCHITECTURE §3.1) with full validation.
- **Files/Packages Touched:** `internal/storage/` (`record.go`, `errors.go`, `record_test.go`, `record_fuzz_test.go`, `testdata/`).
- **Dependencies:** T1.1.
- **Acceptance Criteria:**
  - Constants for header size (21), record types, and default limits; helpers `RecordSize` and `RecordOffset` as described in ARCHITECTURE §4.
  - `Encode` produces one buffer; `Decode` validates in the order header → type → limits → payload length → CRC and returns `ErrCorrupt` (wrapped, with a reason) for each failure.
  - Table tests: round-trip for PUT, empty-value PUT, DELETE; every single-bit flip in a sample record is detected; empty key, oversized key, oversized value and a tombstone with a non-zero value size are rejected.
  - A golden test compares an encoded sample against committed bytes in `testdata/`.
  - `FuzzDecodeRecord` never panics, never allocates more than the limits allow, and re-encodes any successfully decoded record to identical bytes.
- **Verification Command:** `go test -race -count=1 ./internal/storage/ && go test -run '^$' -fuzz FuzzDecodeRecord -fuzztime 20s ./internal/storage/ && make check`

### T1.4 — KeyDir index (2 h)
- [ ] Done
- **Goal:** The concurrent in-memory index from ARCHITECTURE §4.
- **Files/Packages Touched:** `internal/index/` (`keydir.go`, tests, benchmark).
- **Dependencies:** T1.1.
- **Acceptance Criteria:**
  - `CommandIndex` has exactly the four design-doc fields. `KeyDir` offers `Get`, `Put` (returns previous entry and whether it existed), `Delete` (same), `Len`, `UpdateIf(key, cond, new) bool`.
  - Keys are copied on insert; mutating the caller's slice afterwards does not affect the map.
  - A concurrent test with mixed readers and writers passes under `-race` and ends in a state equal to a mutex-protected reference map.
  - `UpdateIf` tests cover: condition true, condition false, key absent.
  - A benchmark inserts 1,000,000 keys of 16–32 bytes and reports heap bytes per key via `b.ReportMetric`.
- **Verification Command:** `go test -race -count=1 ./internal/index/ && go test -run '^$' -bench KeyDir -benchtime 1x ./internal/index/ && make check`

### T1.5 — Segments and the segment set (3.5 h)
- [ ] Done
- **Goal:** Append to an active segment, read from any segment, roll, and manage the set of open files.
- **Files/Packages Touched:** `internal/storage/` (`names.go`, `segment.go`, `store.go`, tests).
- **Dependencies:** T1.2, T1.3.
- **Acceptance Criteria:**
  - File naming and parsing for `seg_<id>.data`, `compact_<id>.data`, `compact_<id>.tmp` (10-digit IDs); unknown names are ignored, malformed IDs are an error.
  - `Store` can: create an active segment with a given ID; `Append(frame)` returning `(fileID, offset)`; `ReadAt(fileID, offset, n)`; `Roll(nextID)` which syncs and seals the active file, creates the next and calls `SyncDir`; register an existing sealed file; `Drop(ids)`; `SyncActive`; `DiskBytes`; `Close`.
  - `ReadAt` on an unknown or dropped ID returns `ErrSegmentNotFound`. Reads from the active segment see bytes just appended.
  - `Drop` closes files only after removing them from the set (ARCHITECTURE §6); a concurrent read/drop test passes under `-race`.
  - `Append` is documented as requiring external serialisation; `Store.mu` protects only the set.
- **Verification Command:** `go test -race -count=1 ./internal/storage/ && make check`

### T1.6 — Segment scanner (2.5 h)
- [ ] Done
- **Goal:** Iterate the records of one file in order and report precisely where and why iteration stopped — the primitive that recovery and compaction share.
- **Files/Packages Touched:** `internal/storage/` (`scanner.go`, tests).
- **Dependencies:** T1.5.
- **Acceptance Criteria:**
  - The scanner yields `(record, recordOffset, valueOffset)` for each valid record and ends with one of: clean EOF, or a corruption error carrying the offset of the first bad byte.
  - Cases covered: empty file, one record, many records, header cut short, payload cut short, CRC mismatch mid-file, oversized length field, unknown type.
  - It reads with `ReadAt` in bounded chunks and never loads a whole segment into memory.
  - No allocation is sized from an unvalidated length.
- **Verification Command:** `go test -race -count=1 -run 'Scan' ./internal/storage/ && make check`

**Week 1 exit check:** `make check` green; `make cover` shows `internal/storage`, `internal/index` and `internal/vfs` each above 85%.

---

## Week 2 — Crash-safe engine (16 h)

Goal: an embedded engine that passes the design doc's fault-injection scenarios 1 and 3.

### T2.1 — SimFS core (3 h)
- [ ] Done
- **Goal:** An in-memory `vfs.FS` that behaves like `OSFS`, ready to have faults added.
- **Files/Packages Touched:** `internal/vfs/simfs/` (`simfs.go`, `file.go`, tests).
- **Dependencies:** T1.2.
- **Acceptance Criteria:**
  - `SimFS` passes `vfstest.RunFSTests` unchanged.
  - Open handles keep working after rename or remove; reads see unsynced bytes; each file tracks its synced length.
  - `Lock` semantics match `OSFS`.
  - Safe for concurrent use (a parallel read/write test passes under `-race`).
  - No use of the wall clock or global randomness.
- **Verification Command:** `go test -race -count=1 ./internal/vfs/... && make check`

### T2.2 — Engine: open, put, get, delete, close (3 h)
- [ ] Done
- **Goal:** The façade from ARCHITECTURE §5.1–5.3, §5.6 and §10 working on an empty directory.
- **Files/Packages Touched:** `internal/engine/` (`engine.go`, `options.go`, `errors.go`, `stats.go`, tests).
- **Dependencies:** T1.4, T1.6, T2.1.
- **Acceptance Criteria:**
  - `Open` creates the directory, takes `LOCK` (a second `Open` on the same directory returns `ErrLocked`), and creates the first active segment. Replay of existing data is **not** in this ticket: `Open` on a non-empty directory may return a "not implemented" error.
  - `Put`/`Get`/`Delete` follow the flows exactly, including the single writer lock around append + KeyDir update and the size-limit errors.
  - Segments roll at `MaxSegmentSize`; keys written before a roll remain readable.
  - `Delete` of a missing key returns `ErrKeyNotFound` and writes nothing (file size unchanged).
  - `Close` is idempotent; every method afterwards returns `ErrClosed`.
  - `liveBytes` and `diskBytes` are maintained and exposed via `Stats()`; a test checks them after overwrites and deletes.
  - Tests run on `SimFS` with 4 KiB segments, plus one end-to-end test on `OSFS`.
- **Verification Command:** `go test -race -count=1 ./internal/engine/ && make check`

### T2.3 — Recovery and the invariant checker (3.5 h)
- [ ] Done
- **Goal:** Rebuild KeyDir on `Open` per ARCHITECTURE §5.4, and a test helper that proves the design doc's invariants.
- **Files/Packages Touched:** `internal/engine/` (`recovery.go`, `recovery_test.go`, `invariants_test.go`).
- **Dependencies:** T2.2.
- **Acceptance Criteria:**
  - Close-and-reopen returns every key with its latest value and no deleted key, across several segments.
  - Startup deletes `*.tmp` files and applies the supersede rule (tested with hand-built directories: a `compact_<M>.data` plus older and newer segments).
  - A torn or corrupt tail in the highest-ID file is truncated and the file synced; all earlier records survive.
  - The same damage in a lower-ID file makes `Open` fail with `ErrCorrupt` naming the file and offset, and leaves the directory untouched.
  - `Open` always creates a new active segment with ID `max+1`.
  - `checkInvariants(t, …)` re-scans the directory independently of the engine and asserts: KeyDir size equals the count of keys whose last record is a PUT, and every KeyDir entry reads back with a valid CRC and matching key. It is called in every reopen test.
  - `liveBytes` after recovery equals its value before the restart.
- **Verification Command:** `go test -race -count=1 -run 'Recover|Reopen|Invariant' ./internal/engine/ && make check`

### T2.4 — SimFS fault injection (3 h)
- [ ] Done
- **Goal:** The four deterministic faults from ARCHITECTURE §11.
- **Files/Packages Touched:** `internal/vfs/simfs/` (`faults.go`, tests).
- **Dependencies:** T2.1.
- **Acceptance Criteria:**
  - Fail the Nth call of a chosen operation (write, sync, create, rename, remove, truncate) with a chosen error; fires once.
  - Tear a write at global byte N: the prefix is applied, then the FS is crashed.
  - Quota: a write that does not fit applies the bytes that do and returns a short count with `vfs.ErrNoSpace`; removing files frees quota.
  - After a crash every FS and file call returns `ErrCrashed`; `Restart()` revives the FS but old handles stay dead; `Restart(LoseUnsynced)` cuts each file back to its synced length.
  - A counter of bytes written is readable so tests can choose crash points.
  - Each fault has its own unit test; the conformance suite still passes with no faults armed.
- **Verification Command:** `go test -race -count=1 ./internal/vfs/simfs/ && make check`

### T2.5 — Write-failure handling, sync policies, scenarios 1 and 3 (3.5 h)
- [ ] Done
- **Goal:** Make failed writes harmless (ARCHITECTURE §5.1 step 4–5, §7) and prove it with the design doc's scenarios.
- **Files/Packages Touched:** `internal/engine/` (`engine.go`, `sync.go`, `fault_test.go`).
- **Dependencies:** T2.3, T2.4.
- **Acceptance Criteria:**
  - A failed or short append truncates the active file to its previous size, leaves KeyDir and counters unchanged, and returns an error; the next `Put` succeeds and recovery sees no garbage.
  - A failed truncate or a failed fsync switches the engine to read-only: writes return `ErrReadOnly`, reads still work, `Stats().ReadOnly` is true.
  - `sync=always` fsyncs before returning; `sync=interval` runs a ticker goroutine that syncs outside the writer lock and stops on `Close`; `sync=never` syncs only on roll and `Close`. The ticker is driven by an injectable clock or channel in tests (no sleeping).
  - **Scenario 1** (`TestCrash_PartialWrite`): tear a `Put` after the header, restart, reopen: the record is gone, earlier data intact, invariants hold.
  - **Scenario 3** (`TestFault_NoSpace`): 1 MB quota, fill to about 999 KB, a 10 KB `Put` returns `ErrNoSpace`, KeyDir unchanged, a small `Put` and all `Get`s still work.
  - Power-loss test: with `sync=always` and `Restart(LoseUnsynced)` no acknowledged write is lost; with `sync=never` the recovered state is a prefix of the acknowledged history and invariants hold.
- **Verification Command:** `go test -race -count=1 -run 'TestCrash_|TestFault_|Sync' ./internal/engine/ && make check`

**Week 2 exit check:** `make check` green; scenarios 1 and 3 pass 50 times in a row (`go test -race -count=50 -run 'TestCrash_PartialWrite|TestFault_NoSpace' ./internal/engine/`).

---

## Week 3 — Compaction and the TCP server (17.5 h)

Goal: space is reclaimed safely, and the database is usable from `redis-cli`.

### T3.1 — Compactor: merge algorithm (4 h, two sessions)
- [ ] Done
- **Goal:** ARCHITECTURE §5.5 end to end on the happy path, callable as `Engine.Compact`.
- **Files/Packages Touched:** `internal/compactor/` (`compactor.go`, tests), `internal/engine/` (`compact.go`, tests), `internal/storage/` (temp-file create/commit helpers on `Store` if needed).
- **Dependencies:** T2.5.
- **Acceptance Criteria:**
  - `Engine.Compact` rolls the active segment, reserves the output ID and starts the new active segment under the writer lock, then runs the merge without holding it.
  - After a merge the directory contains exactly `compact_<M>.data` and the segments newer than `M`; disk usage drops; every key reads back correctly; deleted keys stay deleted after a reopen.
  - Liveness uses exact `(FileID, ValueOffset)` equality; the index swap uses `UpdateIf` in batches of at most 1024 keys.
  - The step order in ARCHITECTURE §5.5 is followed exactly, including fsync before rename and both `SyncDir` calls (asserted through SimFS call counters or an operation log).
  - Concurrency test: writers, deleters and readers run during a merge; the final state equals a reference model, before and after a reopen, under `-race`.
  - A second `Compact` while one runs returns `ErrCompactionRunning`.
  - An injected write error during the merge removes the `.tmp`, leaves all data readable and the engine writable.
- **Verification Command:** `go test -race -count=1 ./internal/compactor/ ./internal/engine/ -run 'Compact' && make check`

### T3.2 — Compaction crash safety and the read race (3 h)
- [ ] Done
- **Goal:** Prove a crash at any point of a merge loses nothing and resurrects nothing (design doc scenario 2), and close the Get-versus-drop race.
- **Files/Packages Touched:** `internal/engine/` (`engine.go` Get retry, `compact_crash_test.go`), `internal/compactor/` (cancellation).
- **Dependencies:** T3.1.
- **Acceptance Criteria:**
  - **Scenario 2** (`TestCrash_InterruptedCompaction`): crash while writing the `.tmp`; after reopen the `.tmp` is gone, the original segments are in use, no key lost.
  - Crash-point sweep: for every write, sync, rename and remove performed by one merge, crash at that point, reopen, compare with the model and run the invariant checker. Includes the window after the rename and before the inputs are deleted, with a workload that contains deleted keys (resurrection check).
  - `Get` retries the KeyDir lookup on `ErrSegmentNotFound` (at most 3 attempts); a stress test with continuous reads during repeated merges never returns an error or a wrong value.
  - Cancelling the context (and `Engine.Close`) stops a running merge promptly and removes its `.tmp`.
- **Verification Command:** `go test -race -count=1 -run 'TestCrash_InterruptedCompaction|CompactCrash|GetDuringCompact|CompactCancel' ./internal/engine/ && make check`

### T3.3 — Automatic compaction trigger (2 h)
- [ ] Done
- **Goal:** Start a merge when garbage crosses the threshold (ARCHITECTURE §5.5 "Trigger", §12).
- **Files/Packages Touched:** `internal/engine/` (`compact.go`, `options.go`, `stats.go`, tests).
- **Dependencies:** T3.2.
- **Acceptance Criteria:**
  - A background goroutine evaluates `1 - liveBytes/diskBytes ≥ CompactRatio && diskBytes ≥ CompactMinBytes` every `CompactInterval`; `0` disables it.
  - Tests drive the ticker through an injectable channel/clock: no merge below the threshold, one merge above it, no overlapping merges.
  - `Stats()` exposes key count, segment count, disk bytes, live bytes, garbage ratio, read-only flag and completed-compaction count.
  - A failed automatic merge is logged and retried on a later tick; it never makes the engine read-only by itself.
  - `Close` stops the goroutine and waits for it.
- **Verification Command:** `go test -race -count=1 -run 'AutoCompact|Stats' ./internal/engine/ && make check`

### T3.4 — RESP parser and writer (3 h)
- [ ] Done
- **Goal:** Read requests and write replies per ARCHITECTURE §9, independent of the engine.
- **Files/Packages Touched:** `internal/server/` (`resp.go`, `resp_test.go`, `resp_fuzz_test.go`).
- **Dependencies:** T1.1.
- **Acceptance Criteria:**
  - `ReadCommand(*bufio.Reader, limits)` returns the argument list for a RESP array of bulk strings or an inline line; binary-safe for bulk payloads (including `\r\n` and NUL bytes).
  - Limits are enforced before allocation: inline line ≤ 64 KiB, ≤ 8 elements, bulk length ≤ limit, negative or non-numeric lengths rejected, missing `\r\n` rejected. Violations return a typed protocol error.
  - A clean EOF between commands is distinguishable from an EOF in the middle of one.
  - Writers for simple string, error, integer, bulk and nil bulk.
  - Table tests cover truncated frames at every byte position of a sample request.
  - `FuzzParseFrame` never panics and never allocates beyond the limits.
- **Verification Command:** `go test -race -count=1 ./internal/server/ && go test -run '^$' -fuzz FuzzParseFrame -fuzztime 20s ./internal/server/ && make check`

### T3.5 — TCP server and command dispatch (3.5 h)
- [ ] Done
- **Goal:** Serve the command table of ARCHITECTURE §9 over TCP.
- **Files/Packages Touched:** `internal/server/` (`server.go`, `conn.go`, `commands.go`, tests).
- **Dependencies:** T3.3, T3.4.
- **Acceptance Criteria:**
  - All commands and aliases in the contract table behave as specified, case-insensitively, with the exact reply types.
  - Engine errors map to the documented error codes through one function; an unexpected error yields `-ERR internal` and a log line, never internal text.
  - Wrong arity and unknown commands keep the connection open; protocol errors close it.
  - Pipelining: 100 commands written in one packet produce 100 replies in order.
  - `MaxConns` is enforced without blocking the accept loop; idle and write deadlines are set.
  - `Shutdown(ctx)` stops accepting, lets in-flight commands finish, then closes remaining connections; no goroutine leaks (asserted by waiting on the server's `WaitGroup`).
  - Tests use a fake store for dispatch and one integration test with a real engine on `SimFS` over a `127.0.0.1:0` listener.
- **Verification Command:** `go test -race -count=1 ./internal/server/ && make check`

### T3.6 — Binary, flags and graceful shutdown (2 h)
- [ ] Done
- **Goal:** A runnable `tinycask` server with the flags of ARCHITECTURE §12 and a scripted smoke test.
- **Files/Packages Touched:** `cmd/tinycask/main.go`, `scripts/smoke.sh`, `Makefile` (`smoke` target).
- **Dependencies:** T3.5.
- **Acceptance Criteria:**
  - All flags in the configuration table exist with the documented defaults; invalid values produce a usage error before the data directory is touched.
  - SIGINT and SIGTERM trigger server shutdown, then `engine.Close()`, and exit code 0.
  - `scripts/smoke.sh` builds the binary, starts it on a free port in a temp directory, and checks with `redis-cli`: `PING`, `SET`/`GET`, `DEL`, a binary value, `INFO`, `COMPACT`; then `kill -9`, restart, the key is still there; then SIGTERM and exit code 0; then a second instance on the same directory fails with a "locked" message.
  - `redis-benchmark -p <port> -t set,get -n 10000 -q` completes without protocol errors (it may warn that `CONFIG` is unsupported).
- **Verification Command:** `make check && ./scripts/smoke.sh`

**Week 3 exit check:** `make check` and `./scripts/smoke.sh` green.

---

## Week 4 — Proof, measurements and polish (14.5 h + 3 h buffer)

Goal: evidence that the design doc's guarantees hold, numbers for the report, and a tagged release.

### T4.1 — Model-based crash test (4 h, two sessions)
- [ ] Done
- **Goal:** The randomized test from design doc §3.4: random operations and crash points checked against a model.
- **Files/Packages Touched:** `internal/engine/` (`model_test.go`).
- **Dependencies:** T3.3.
- **Acceptance Criteria:**
  - A seeded generator issues `Put`, `Delete`, `Get` and `Compact` over a small key space with small segments, with a crash at a random byte count; several crash/restart cycles per seed.
  - After each restart: every acknowledged operation is reflected; the interrupted operation is either fully applied or absent; `Get` results match the model; the invariant checker passes.
  - A second mode uses `Restart(LoseUnsynced)` with `sync=never` and asserts the recovered state equals the model at some prefix of the acknowledged history, no older than the last successful sync.
  - `-seed` and `-iterations` test flags exist; the seed is logged on failure and reproduces the failure exactly.
  - The default run (in `make test`) takes under 10 seconds; 2,000 iterations pass locally.
  - Any bug found is fixed in this ticket with a dedicated regression test.
- **Verification Command:** `go test -race -count=1 -run 'TestModel' ./internal/engine/ -args -iterations 2000 && make check`

### T4.2 — Engine benchmarks and memory footprint (3 h)
- [ ] Done
- **Goal:** The four workloads of design doc §4.1 as `go test -bench`, plus the 1M-key memory check of §4.3.
- **Files/Packages Touched:** `internal/engine/` (`bench_test.go`).
- **Dependencies:** T3.3.
- **Acceptance Criteria:**
  - Benchmarks on `OSFS` in `b.TempDir()` with 16-byte keys and 256-byte values: `BenchmarkGet`, `BenchmarkPut`, `BenchmarkMixed` (80/15/5), `BenchmarkMixedDuringCompaction` (a merge of roughly 200 MB running for the measured interval).
  - Each uses `b.RunParallel` and reports `p99-ns/op` and `ops/s` via `b.ReportMetric`, in addition to the defaults.
  - A memory benchmark loads 1,000,000 keys and reports heap in use; the result is recorded, with a note if it falls outside 100–150 MB.
  - Nothing asserts on timing; `make bench` runs them all.
- **Verification Command:** `go test -run '^$' -bench 'Get|Put|Mixed' -benchtime 2s ./internal/engine/ && make check`

### T4.3 — End-to-end benchmark script and report (2.5 h)
- [ ] Done
- **Goal:** Measured numbers through the TCP path, written up against the design doc's targets.
- **Files/Packages Touched:** `scripts/bench.sh`, `BENCHMARKS.md`, `Makefile` (`bench-e2e` target).
- **Dependencies:** T3.6, T4.2.
- **Acceptance Criteria:**
  - `scripts/bench.sh` starts a server in a temp directory and runs with `redis-benchmark`, 50 clients: GET only, SET only, a mixed run (three concurrent processes approximating 80/15/5, using a custom `DEL key:__rand_int__` command), and the mixed run again while `COMPACT` is executing.
  - `BENCHMARKS.md` records hardware, OS, Go version, sync policy, the commands used, and a table of P99 and throughput per scenario next to the design-doc target, marked met or not met. Unmet targets get a one-paragraph explanation; the numbers are reported as measured.
  - Includes the `go test -bench` results from T4.2.
- **Verification Command:** `./scripts/bench.sh && test -s BENCHMARKS.md && make check`

### T4.4 — Coverage and fuzz hardening (3 h)
- [ ] Done
- **Goal:** Reach the coverage targets of design doc §3.5 and shake out parser bugs.
- **Files/Packages Touched:** test files across `internal/`; production code only to fix defects found.
- **Dependencies:** T4.1.
- **Acceptance Criteria:**
  - `make cover` shows total coverage > 85%, and `internal/storage`, `internal/engine`, `internal/compactor` each ≥ 90%.
  - Every SimFS fault type is exercised by at least one engine test; list them in the final message.
  - Both fuzz targets run 5 minutes each with no failures; any crasher found is kept under `testdata/fuzz/` together with a fix.
  - `go test -race -count=20 ./...` passes (flake check).
  - Remaining uncovered lines in the three core packages are listed with a one-line reason each.
- **Verification Command:** `make cover && go test -race -count=20 ./... && go test -run '^$' -fuzz FuzzDecodeRecord -fuzztime 5m ./internal/storage/ && go test -run '^$' -fuzz FuzzParseFrame -fuzztime 5m ./internal/server/`

### T4.5 — README, documentation sync and release (2 h)
- [ ] Done
- **Goal:** Someone can clone, run and understand the project; the docs match the code.
- **Files/Packages Touched:** `README.md`; `ARCHITECTURE.md` and `docs/design-doc.md` **only for factual corrections, each confirmed by you** (the hook will ask).
- **Dependencies:** T4.3, T4.4.
- **Acceptance Criteria:**
  - `README.md` covers: what it is, quick start (`make run`, `redis-cli` examples), the command table, configuration flags, durability modes, how to run tests/fuzz/benchmarks, known limitations, and links to the design doc, architecture and benchmark report.
  - Opus produces a list of every place where the code differs from `ARCHITECTURE.md` or the design doc; you approve each fix (to code or to docs) individually.
  - Core LOC (non-test Go under `internal/` and `cmd/`) is counted and reported against the 2,500–3,000 budget.
  - `make check` and `./scripts/smoke.sh` pass on a clean clone; then tag `v0.1.0` (by you).
- **Verification Command:** `make check && ./scripts/smoke.sh && git status --short`

### Buffer (3 h)
Reserved for spillover. The likeliest consumers, in order: bugs surfaced by T4.1, the crash-point sweep in T3.2, and benchmark tuning in T4.3. If it is unused, spend it on a longer fuzz run and a second read-through of the diff of `internal/engine`.

---

## Risks and what to cut first

| Risk | Signal | Response |
| --- | --- | --- |
| Compaction (T3.1–T3.2) overruns | not green after 3 sessions | Ship manual `COMPACT` only; drop T3.3 (auto-trigger) |
| Model test keeps finding bugs | T4.1 exceeds 6 h | Good news, but time-box: fix what is found, lower the default iteration count, note open seeds in the README |
| Performance targets missed | T4.3 numbers below the doc's table | Report honestly in `BENCHMARKS.md`; do not add group commit or caching (out of scope) |
| `redis-benchmark` incompatibility | handshake commands fail the run | Answer the specific command with a harmless reply inside T3.5; do not implement more of Redis |
| A week slips | fewer than 4 tickets done by Friday | Cut in this order: T4.3 mixed-under-compaction run, T3.3, the `LoseUnsynced` mode of T4.1 |

Never cut: T2.3, T2.5, T3.2 — they are the project's crash-safety claim.
