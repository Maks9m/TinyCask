---
paths:
  - "**/*_test.go"
---

# Testing rules

## Basics
- `testing` plus `testify`: `require` for preconditions and anything the test cannot continue without, `assert` for the rest.
- Every test must pass under `go test -race -count=1`. A test that needs `-count=1` to hide state is a bug.
- Table-driven tests with `t.Run` and descriptive case names. Call `t.Parallel()` only when the test owns all of its state.
- Helpers call `t.Helper()` and take `testing.TB`. Clean up with `t.Cleanup`, files with `t.TempDir()`.
- Name tests `Test<Unit>_<Scenario>`; fault-injection tests start with `TestCrash_` or `TestFault_`.

## Determinism
- No `time.Sleep`, no wall-clock assertions, no dependence on goroutine scheduling. Synchronise with channels, `sync.WaitGroup`, or the injectable clock.
- Randomised tests take a seed (`-seed` flag, default fixed), log it with `t.Logf`, and derive everything from one `rand.New(rand.NewPCG(...))`.
- Use small limits to hit edge cases quickly: 4 KiB segments, 64-byte values, a handful of keys.

## What to cover
- Failure paths are first-class: for code that writes, inject a failed write, a short write and a failed sync, and assert both the returned error and the state afterwards.
- After any reopen of an engine, call the shared invariant checker: KeyDir size equals live keys in the log, and every entry reads back with a valid CRC.
- Engine tests run on `SimFS` by default; keep at least one end-to-end test per feature on `OSFS` to catch simulator drift.
- Concurrency tests assert an outcome (final state equals a model, restart state equals pre-restart state), not merely "the race detector was quiet".
- Crash tests follow one shape: arm fault → run until `ErrCrashed` → `Restart` → reopen → compare with the model → check invariants.

## Fuzzing and golden files
- Fuzz targets assert only "no panic, no huge allocation, and decode(encode(x)) == x". Commit every crasher the fuzzer finds under `testdata/fuzz/`.
- Golden files in `testdata/` pin the on-disk format. Do not add an `-update` flag for the record golden file.

## Benchmarks
- Benchmarks report, they never assert on timings. Call `b.ReportAllocs()`, and report extra metrics with `b.ReportMetric` (for example `p99-ns/op`).
- Build fixtures before `b.ResetTimer()`.

## Never
- Do not delete, skip or loosen a failing test to finish a ticket. Fix the code, or stop and report.
- Do not test unexported helpers through reflection; test through the package's API or put the test in the same package.
- Do not hit the real network beyond `127.0.0.1:0` listeners.
