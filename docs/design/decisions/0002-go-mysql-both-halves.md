# ADR 0002: go-mysql on both halves of the proxy

## Status

Accepted — 2026-08-12.

## Context

The proxy is a man in the middle: it acts as a MySQL *server* to the client and as a MySQL
*client* to the upstream database. The first implementation used go-mysql for the client-facing
half and `database/sql` with go-sql-driver for the upstream half.

Review found four defects, three of which shared that split as their root cause:

1. **Every write reported zero affected rows.** `db.Query()` does not error on DML in
   go-sql-driver, so the `Exec` fallback was unreachable dead code. Rows were inserted correctly;
   clients were told nothing had happened.
2. **Truncated result sets looked successful.** `rows.Err()` was never checked, so an upstream
   failure mid-`SELECT` returned a short result set with no error.
3. **Transactions silently didn't work.** `sql.DB` is a connection *pool*, not a session:
   `database/sql` retries on a fresh connection, so `BEGIN` → connection drop → `INSERT`
   autocommits → `ROLLBACK` becomes a no-op that reports success.

The underlying problem is that `database/sql` forces the caller to choose `Query` or `Exec`
*before* the reply arrives, and discards whichever half wasn't asked for. But `HandleQuery` must
return whatever the upstream actually sent.

## Decision

Use **go-mysql's own `client` package for the upstream half**, so both halves of the proxy speak
the same library.

## Consequences

**Positive**

- `client.Execute` returns `*mysql.Result` — exactly the type `HandleQuery` must return — built by
  dispatching on the reply packet *after* it arrives. Three defects disappear at once, and
  `HandleQuery` went from 55 lines to one.
- **Real error codes survive.** 1062 stays 1062 rather than collapsing to 1105.
- **Rows relay byte for byte**, because the server writes `RowDatas` verbatim — so column types
  and BLOBs stop being mangled.
- A session is pinned to one upstream connection, so transactions and session state cannot
  silently migrate.

**Negative / risks**

- Removing the pool removed the accidental reconnect-on-failure behaviour, which had been
  masking a dead upstream. A session now needs explicit teardown when the upstream dies —
  added as `classifyErr` / `upstreamDead`: a `*mysql.MyError` means the upstream is alive and
  rejected the SQL (keep going), anything else is a transport failure (close the session).

**Trap — do not undo**

Never wrap the error returned from `Execute`. `server.writeError` uses a direct type assertion on
`*mysql.MyError`, not `errors.As`, so any wrapping collapses every upstream error code to 1105.

## Verification

`ROW_COUNT()` = 1, `LAST_INSERT_ID()` = 1, duplicate key returns `ERROR 1062 (23000)`,
`SELECT HEX(b)` returns `DEADBEEF` intact, session variables and temporary tables survive, and
`ROLLBACK` actually rolls back. Regression tests were confirmed failing against the pre-fix
commit before being accepted.

## Alternatives considered

- **Keep `database/sql` and inspect the statement to choose `Query` or `Exec`.** Rejected:
  guessing from the SQL text before the reply arrives is exactly the flaw, and it can't be made
  reliable.
- **Hand-roll the upstream protocol.** Rejected: no reason to, given the library exists.
