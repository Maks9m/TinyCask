---
paths:
  - "internal/server/**"
  - "cmd/**"
---

# Server and protocol rules

The contract is ARCHITECTURE §9. `redis-cli` and `redis-benchmark` must keep working unmodified.

## Parsing
- Two request forms only: RESP2 array of bulk strings, and one inline line split on single spaces. No RESP3, no nested arrays, no other RESP types in requests.
- Enforce limits **before** reading or allocating: inline line ≤ 64 KiB, ≤ 8 array elements, bulk length ≤ max value size, non-negative lengths, exact `\r\n` terminators.
- Read bulk payloads with `io.ReadFull` into a buffer sized from the already-validated length.
- A framing violation is answered with `-ERR protocol <detail>` and the connection is closed. A well-formed but unknown or wrong-arity command is answered with an error and the connection stays open.
- The parser must never panic and never allocate proportionally to an unvalidated number. `FuzzParseFrame` guards this.
- Parser and writer are pure functions over `*bufio.Reader` / `*bufio.Writer`; they know nothing about the engine.

## Dispatch
- Commands live in one table: name → arity → handler. Names are matched case-insensitively. `PUT`/`DELETE` are aliases of `SET`/`DEL`.
- Engine errors map to reply codes in one function using `errors.Is`. Unknown errors become `-ERR internal` and are logged; internal details never reach the client.
- The server talks to the store through a small interface it defines itself, so tests can use a fake.

## Connections
- One goroutine per connection, tracked in a `sync.WaitGroup`.
- Set a read deadline before each command (idle timeout) and a write deadline before each flush.
- Flush the reply buffer when the read buffer has no more complete input, so pipelines are answered in one write.
- Over `MaxConns`: close the new connection immediately; never block the accept loop.
- `net.ErrClosed` from `Accept` during shutdown is a normal exit, not an error to log.

## Shutdown
- `Shutdown(ctx)`: close the listener, expire idle read deadlines, wait for in-flight commands until `ctx` ends, then force-close what remains.
- `main` owns the order: stop the server first, then `engine.Close()`. Exit code 0 on a clean signal shutdown, non-zero if `Open` or `Close` failed.

## cmd/tinycask
- `main.go` only parses flags, builds the logger, wires `OSFS` → engine → server, and handles signals with `signal.NotifyContext`. No business logic.
- Validate flags and print a usage error before touching the data directory.
