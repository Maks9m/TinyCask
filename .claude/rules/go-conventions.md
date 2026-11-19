---
paths:
  - "**/*.go"
---

# Go conventions

## Errors
- Return errors; wrap with `fmt.Errorf("doing x: %w", err)` so the chain keeps the sentinel.
- Declare sentinels as `var ErrX = errors.New("pkg: x")` at the top of the package's `errors.go`.
- A function either handles an error or returns it, never both (no log-and-return).
- Check the error from `Close` and `Sync` on any file that was written. On read-only handles, `defer f.Close()` is fine.
- When two errors occur (for example a write error and then a failed cleanup), return both with `errors.Join`.

## API shape
- Constructors take dependencies explicitly: `New(fsys vfs.FS, dir string, opts Options)`. No globals, no singletons.
- Configuration is a plain `Options` struct with a `withDefaults()` method; no functional options.
- Accept interfaces, return concrete types. Define an interface in the package that consumes it, and only when there are two implementations or a test needs a fake.
- `context.Context` is the first parameter of anything that blocks for long or runs in the background. Do not store a context in a struct, except the engine's own lifetime context.
- Keys and values are `[]byte`. Copy a slice before retaining it past the call; return slices the caller owns.

## Concurrency
- Comment every mutex with what it protects. Group the mutex directly above the fields it guards.
- Unlock with `defer` unless the lock must be released before I/O; in that case keep the critical section in its own small function.
- Use `sync/atomic` typed values (`atomic.Bool`, `atomic.Int64`) for flags and counters.
- Start goroutines only where their owner can cancel and wait for them.

## Style
- Standard library first. No generics unless they remove real duplication.
- Table-driven code over long `if` chains for command dispatch and error mapping.
- Named constants for every size, limit and magic number; no bare `21` or `1024` in logic.
- Doc comment on every exported identifier, starting with its name. Comments explain why, not what.
- Logging is `log/slog` with key-value attributes. Libraries receive a `*slog.Logger` in `Options`; they never call the default logger.
- Keep files under about 300 lines and functions under about 60; split by responsibility, not by type.
