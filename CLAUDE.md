# TinyCask

Bitcask-style key-value store in Go: append-only log segments, an in-memory KeyDir index, background compaction, and a TCP server speaking a RESP2 subset. Solo university project, built ticket by ticket.

## Sources of truth

| File | Role | Editable? |
| --- | --- | --- |
| `docs/design-doc.md` | Requirements (Ukrainian, professor-approved) | Only when the user explicitly asks |
| `ARCHITECTURE.md` | How it is built: packages, formats, flows, locks | Only when the user explicitly asks |
| `ROADMAP.md` | Tickets with acceptance criteria | Tick checkboxes only |

- Before coding, read the ticket in `ROADMAP.md` and the `ARCHITECTURE.md` sections it names.
- Work on **exactly one ticket** per session. Do not start the next one unasked.
- If a ticket cannot be done without changing the architecture or the design doc, stop and ask. A hook asks the user to confirm any edit to `docs/`, `ARCHITECTURE.md` or `.env`.

## Stack (locked)

- Go **1.27**, module `github.com/Maks9m/TinyCask`
- Runtime code: **standard library only**. Tests may use `github.com/stretchr/testify`. No other dependency without the user's approval.
- Dev tools: `make`, `golangci-lint` v2, `redis-cli` / `redis-benchmark` (smoke tests and benchmarks)
- Targets: macOS and Linux. No Windows code paths.
- No SQL database, no HTTP API, no migrations. "Schema" means the on-disk record format (ARCHITECTURE §3).

## Commands

The `Makefile` is created in ticket T1.1; each target wraps the command shown.

| Task | Command | Runs |
| --- | --- | --- |
| Build | `make build` | `go build ./... && go build -o bin/tinycask ./cmd/tinycask` |
| Test | `make test` | `go test -race -count=1 ./...` |
| One test | — | `go test -race -count=1 -run 'TestName' ./internal/engine/` |
| Lint | `make lint` | `golangci-lint run ./...` |
| Vet | — | `go vet ./...` |
| Coverage | `make cover` | `go test -race -coverprofile=coverage.out ./... && go tool cover -func=coverage.out` |
| Fuzz | `make fuzz` | 30 s each: `FuzzDecodeRecord` (`./internal/storage/`), `FuzzParseFrame` (`./internal/server/`) |
| Bench | `make bench` | `go test -run '^$' -bench . -benchmem ./internal/engine/` |
| Run | `make run` | `go run ./cmd/tinycask -dir ./data` |
| Everything | `make check` | build + vet + lint + test |
| Migrate | n/a | there is no database |

Smoke test against a running server: `redis-cli -p 6380 PING`.

## Package map

`cmd/tinycask` → `internal/server` → `internal/engine` → `internal/{compactor,index,storage}` → `internal/vfs` (+ `internal/vfs/simfs` for tests). The full import table is ARCHITECTURE §2; imports outside it are bugs.

## Architectural invariants

1. **All disk I/O goes through `vfs.FS`.** No `os` file calls, `syscall` or `io/ioutil` outside `internal/vfs/osfs*.go`.
2. **One writer lock covers the append and the matching KeyDir update.** They are never split across two critical sections.
3. **Lock order: writer → KeyDir → Store.** Never the reverse. Never do I/O while holding `KeyDir.mu`.
4. **Records are immutable and ordered by `(FileID, offset)`.** Nothing is rewritten in place; timestamps never decide ordering.
5. **The record format is v1 and frozen** (21-byte header, little-endian, CRC32-IEEE). The golden test must not be regenerated to make a change pass.
6. **A `compact_<M>.data` file supersedes every data file with ID < M**, and a compaction always merges all sealed files.
7. **Sync before trust:** fsync a segment before sealing it, a compaction output before renaming it, and the directory after create/rename/delete.
8. **A failed append is truncated away; a failed truncate or fsync makes the engine read-only.** Never continue writing after either.
9. **A torn tail is repaired only in the highest-ID file.** Corruption anywhere else fails `Open` with `ErrCorrupt`.
10. **Never trust lengths read from disk or network.** Check them against the limits before allocating.
11. **Every goroutine has an owner** that cancels it via `context` and waits for it via `sync.WaitGroup`.

## Prohibited

- `panic`, `log.Fatal` or `os.Exit` outside `cmd/tinycask/main.go`
- Ignored errors (`_ = f.Close()` on a write path, unchecked `Sync`, bare `err` shadowing that drops an error)
- Comparing errors by string; use `errors.Is` / `errors.As`
- `time.Sleep` to wait for another goroutine, in code or in tests
- Package-level mutable state, `init()` with side effects, `interface{}`/`any` in the engine's public API
- Userspace write buffering (`bufio.Writer`) on segment files
- Importing `simfs` from non-test files
- New third-party dependencies, new packages, or new top-level directories not listed in ARCHITECTURE §2
- Features listed as out of scope in ARCHITECTURE §14 (TTL, scans, hint files, group commit, auth, metrics…)
- Weakening, skipping or deleting a failing test to get green; `//nolint` without a reason comment
- `git commit` and `git push`, always: the user does both by hand (a hook blocks them). No branch changes unless the user asks

## Hooks you will meet

- After every `Edit`/`Write` of a `.go` file, `gofmt -w` runs on it. Do not spend effort on formatting; re-read a file before editing it again if the hook reports it changed.
- When you finish a turn, `go build ./... && go vet ./...` runs; a failure is returned to you to fix.
- `git commit` and `git push` are blocked in every form. Do not look for a way around it; end the ticket with a suggested commit message instead.
- `rm -rf` and `git reset --hard` are blocked. If one is truly needed, tell the user the exact command to run.

## Definition of Done (verify every item before saying a ticket is finished)

1. Every acceptance criterion in the ticket is met, and its **Verification Command** exits 0. Paste the command and its final output lines.
2. `make check` passes: build, `go vet`, `golangci-lint` (zero findings) and `go test -race -count=1 ./...`.
3. New behaviour has tests, including the failure paths (injected write/sync errors where the code touches disk).
4. No invariant above is violated and no prohibited pattern was introduced.
5. No changes outside the ticket's **Files/Packages Touched** list, except trivial fixes you call out explicitly.
6. Exported identifiers have doc comments; no TODOs left without a ticket ID.
7. The ticket's checkbox in `ROADMAP.md` is ticked.
8. Your final message states plainly what was done, what was **not** done, and anything you could not verify.

If any item fails, the ticket is not done: say so and show the failing output.

## Subagents (`.claude/agents/`)

Closing a ticket that touches disk: `fault-test-writer` (missing failure-path tests) → `invariant-reviewer` (fix every blocking finding) → `dod-verifier` (its verdict goes in your final message). Smaller tickets skip the first step.

- `invariant-reviewer`: read-only review of the diff against the invariants, prohibited list and ARCHITECTURE.
- `fault-test-writer`: writes `TestFault_`/`TestCrash_` tests on SimFS; edits only `_test.go` and `testdata/`; reports bugs instead of fixing them.
- `dod-verifier`: runs the Verification Command and `make check`, checks every DoD item and acceptance criterion; never fixes.
- `code-explainer`: on request, explains code line by line for the defense (Go concepts, the "why", likely examiner questions).

## Conventions worth knowing up front

- The user commits. Close each ticket by proposing a message in the form `T<week>.<n>: <imperative summary>`.
- Sizes are `int64` bytes; file IDs are `uint32`; offsets are `int64`.
- Time comes from an injectable clock on the engine options, so tests are deterministic.
- Randomised tests take a seed, log it, and can be re-run with `-seed`.
- Tests shrink limits (for example 4 KiB segments) instead of writing megabytes.
