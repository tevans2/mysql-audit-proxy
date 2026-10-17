# ADR 0007: SQLite remains the audit store

## Status

Accepted — 2026-10-16, by default. This closes the "SQLite or Postgres" question that had been
pending since the first version. The client did not answer it at a demo or afterwards, so
this is our default and it stands unless they tell us otherwise. The measurements below were taken
on 13 Oct 2026; the follow-up status at the end was checked at project close.

## Context

The audit store is SQLite (`mattn/go-sqlite3`, WAL mode, one API process, file on the `api-data`
volume). The spec allows MySQL, PostgreSQL, or SQLite for an MVP. The initial baseline lost audit
records above about 7,500 statements/second and attributed that "substantially" to SQLite, so we
measured the store directly (one developer laptop, local disk):

- **Writes are not the limit.** `WriteBatch` sustains 44,000–58,000 events/s, and holds about
  50,000/s steadily up to 1M rows. The specified load is about 2,500/s. The loss point therefore
  appeared to come from the sink (one worker, batches of 50, one HTTP POST at a time), not from
  SQLite. The block-mode re-run later measured the sink and API together and found the audit
  pipeline ingests about 18,500 statements/s end to end
  ([perf re-run](../../perf/block-mode-rerun.md)); that figure is the pipeline's, not the
  store's alone.
- **Reads were slow at 1M rows, for fixable reasons.** `GetStatements` had no index on `ts`, so
  an unfiltered page sorted the whole table; adding one took it from seconds to about 18 ms.
  `GetConnections`, which the UI polls, counts every statement before limiting (150–250 ms);
  selecting the page first took 5.5 ms. Free-text search (`LIKE '%q%'`) stays at about 7.5 s per
  query and needs an FTS5 trigram index or a documented limit.
- **Size:** about 362 bytes per statement, or 362 MB per million. Retention, which the client has not
  answered, will decide storage needs more than the choice of database.
- **Switching is expensive.** `store.go` is SQLite-specific (`AUTOINCREMENT`, `INSERT OR IGNORE`,
  duplicate-column migrations, `?` placeholders), and `api.go` holds a concrete `*store.Store`.
  Postgres would also be a new service for the client to run, secure and back up.

Timings were noisy (one unindexed query varied from 0.9 s to 6.9 s between runs), so trust the
orders of magnitude, not the digits. Postgres was not measured.

## Decision

Keep SQLite as the audit store. This is the default if the client does not specify otherwise.

## Consequences

- No new service, credentials or migration for the client.
- The API remains a single process; SQLite cannot be shared across instances.
- The read path needed work, which is query and index work and not a database change. Where each
  item stands at project close:
  - index on `statements(ts)`: **done** (`idx_statements_ts`, added with retention);
  - page connections before counting statements: **not done**; `GetConnections` still joins and
    counts on every poll;
  - decide between FTS5 and a documented limit for free-text search: **not done**; the limit is
    recorded in [known limitations](../../known-limitations.md);
  - retention needs a deletion job: **done** ([ADR 0006](0006-audit-log-retention.md)), with the
    period still unset by default.
- A `Store` interface so a later port stays bounded was recommended and **not done**: the API
  still holds a concrete `*store.Store`. [`docs/testing.md`](../../testing.md) describes the
  interface as intent; the as-built note there says so.
- [`docs/perf/baseline.md`](../../perf/baseline.md) attributes the ceiling to
  SQLite. That note carries a correction pointing here.

## Revisit if

- More than one API instance, or high availability, is needed.
- Retention times volume implies roughly 100 GB or more, or time-partitioned deletes.
- Free-text search over history is required and FTS5 is not acceptable.
- Postgres is mandated.

## Alternatives considered

- **PostgreSQL now.** Better for concurrency, partitioned retention and text search (`pg_trgm`).
  Rejected: nothing measured needs it, it adds an operated service, and migrating the store, API
  wiring and tests (plus the sessions and admin tables) would have landed late in the project.
- **MySQL.** Allowed by the spec; same cost as Postgres with no advantage here.
