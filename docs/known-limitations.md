# Known Limitations

What the system does *not* do, and the traps in the code. Written down so nobody
rediscovers them the hard way, and so we can answer the client honestly.

Current as of project close (16 Oct 2026, end of Sprint 3). Earlier versions of this file listed
prepared statements, `caching_sha2_password`, failed-login recording and back-office
authentication as missing; all four have shipped and are no longer listed.

## Client compatibility

- **Clients must use TLS to the proxy.** Authentication is passed through to the database
  ([ADR 0004](design/decisions/0004-pass-through-authentication.md)) and `caching_sha2_password`
  needs a secure channel. The proxy's certificate is self-signed and generated in memory at start,
  so it changes on every restart and nothing can pin it. `mysql --ssl-mode=PREFERRED` (the default)
  works; DBeaver and JDBC need `sslMode=REQUIRED` and must not verify the certificate. A client
  with TLS off is refused.
- **A user that does not exist on the database cannot connect.** There is no proxy-side user
  list; add real users to the database.
- **A failed prepared statement loses its real error code.** A `COM_STMT_EXECUTE` that fails
  upstream (duplicate key, deadlock, lock timeout) reaches the client as `1105 ER_UNKNOWN_ERROR`
  instead of 1062, 1213 or 1205. This is a go-mysql defect, not the proxy's; plain queries keep
  their codes. Detail in [`cmd/proxy/README.md`](../cmd/proxy/README.md).
- **Whole result sets are buffered in memory.** A very large `SELECT` costs proxy memory and
  latency before the client sees its first row. Streaming is the fix and is not built. Only
  100-row results were measured ([perf baseline](perf/sprint-2-baseline.md)).

## Availability

- **The proxy is a single point of failure.** If it stops, clients cannot reach the database
  through it. Sessions hold no shared state, so running several instances is possible, but that is
  not built and failover is untested.
- **The API and its SQLite file are single-instance.** The audit store cannot be shared between
  API processes ([ADR 0007](design/decisions/0007-sqlite-remains-the-audit-store.md)). The
  admin-login rate limiter lives in that process's memory, so it resets when the API restarts.
- **A config change is applied within one poll interval, not instantly.** The proxy polls the API
  (default 5 s), then reconciles its listeners. The Servers screen and the header pill show
  whether the running proxy has caught up with what was saved.

## Auditing guarantees

- **A full buffer blocks queries by default.** Above about 18,500 statements/second the sink
  stalls the query path rather than lose records
  ([ADR 0005](design/decisions/0005-audit-sink-blocks-by-default.md),
  [measurements](perf/sprint-3-rerun.md)). `SINK_ON_FULL=drop` chooses the opposite trade. The
  specified load (about 2,500/s) is far below either threshold.
- **Delivery is still not durable.** Blocking closes the buffer-overflow path only. A batch the
  API keeps refusing is given up on after the sink's retries (about 15 s), counted in
  `lustre_audit_failed_total`, and not replayed. A real API outage loses records. There is no disk
  buffer.
- **An event waiting to enter a full buffer at shutdown is dropped.** Shutdown drains what is
  already buffered, not what is still waiting.
- **The audit log is not tamper-evident.** It is a SQLite file; anyone with access to it could
  edit or delete rows undetectably. Hash-chaining was designed and not built.
- **Raw SQL is stored, including literal values.** The audit log therefore holds whatever personal
  or sensitive data appears in queries, which makes it POPIA-relevant. The usual mitigation is a
  normalised fingerprint for searching, with the raw text under tighter control. Acumen did not
  decide this.
- **Retention is off by default.** Nothing is ever deleted unless `API_RETENTION_DAYS` is set,
  so the database grows without bound (about 362 bytes per statement). Acumen did not give a
  period; 365 days with deletion is recommended. See
  [ADR 0006](design/decisions/0006-audit-log-retention.md).
- **The statement type is only the first keyword.** `SET`, `BEGIN`, `COMMIT`, `USE`, `WITH` and
  `CALL` belong to no read/write/schema group, and a `WITH ... INSERT` is typed `WITH`. The
  filters and the UI badges use the same fixed lists.
- **The "handshake" filter is a list of known strings.** It hides the statements common clients
  (the mysql CLI, JDBC, DBeaver) send on connect, and nothing else. A client that sends something
  new shows it until the list is extended. Nothing is deleted; the filter only hides rows.
- **Bypassing the proxy is not recorded.** The development compose file publishes the databases'
  own ports (3306 and 3316) next to the proxy's (3307 and 3308). A client that connects to 3306
  skips the proxy and the audit entirely; this has caught people out. A production deployment
  must expose only the proxy ports.

## Observability

- **Proxy metrics exist; nothing consumes them.** `GET /metrics` and `/healthz` are on the proxy's
  admin port (9100, not published). There is no dashboard, no alerting, and the API has no
  metrics of its own. Audit loss is visible only to someone who reads the counters.
- **The UI has no view of those metrics.** An overview of the audit log is available (the
  summary endpoint), but queue depth and drop counters are not shown anywhere in the back-office.

## Security and privacy

- **Back-office accounts are basic.** Named admins with bcrypt passwords, an HttpOnly session
  cookie, a CSRF header and a login rate limit. There is no multi-factor authentication, no
  password policy beyond length (8 to 72 bytes), sessions last a fixed 24 hours, and a disabled
  admin cannot be re-enabled except by editing the database.
- **Admin attribution is written after the change, not with it.** If the API dies between the
  change and the attribution row, the change has no log entry. Doing both in one transaction is
  the intended fix.
- **The login rate limiter trusts `X-Forwarded-For` from any private address.** It should trust
  only the edge. On the compose network this is harmless; anywhere else check it.
- **There is one role.** Every admin can do everything: read the whole audit log, manage servers
  and manage other admins.
- **Authorisation is the database's.** Each session reaches MySQL as the connecting user, so the
  database's grants apply. The proxy adds no rules of its own, and there is no per-user policy on
  what may be run or read in the audit log.

## Attribution

- **Pooled application connections give coarse attribution.** If an application authenticates
  once as a service account and pools those connections, every query on them is attributed to
  that account. This system is aimed at people connecting as themselves, not at application
  traffic.

## Back-office

- **The UI polls.** The audit tables and the Servers screen refresh every 5 seconds and the
  header status pill every 10, so a new statement can take about 5 seconds to appear. There is
  no push channel.
- **Free-text search scans the table.** `LIKE '%q%'` over about a million statements took about
  7.5 seconds in testing; an FTS5 index, or a documented limit, was never done
  ([ADR 0007](design/decisions/0007-sqlite-remains-the-audit-store.md)). The connections list
  counts statements per connection on every poll and is slow at that size for the same reason.
- **Only the pure UI helpers are tested.** `make web-test` covers URL state, config state,
  statement kinds and the action log. There are no browser tests; the screens were checked by
  hand.

## Traps in the code

- **Never wrap the error returned from go-mysql's `Execute`.** `server.writeError` uses a direct
  type assertion on `*mysql.MyError`, not `errors.As`, so any wrapping collapses every upstream
  error code to 1105. Real codes (1062, 1046, …) are lost.
- **`sql.DB` is a connection pool, not a session.** This is why the upstream half uses go-mysql's
  client directly — see [ADR 0002](design/decisions/0002-go-mysql-both-halves.md). Reintroducing
  `database/sql` on the upstream side silently breaks transactions.
- **Timestamps are compared as text.** They must be stored fixed-width with microsecond
  precision; `RFC3339Nano` trims trailing zeros, which breaks both ordering and the statement
  de-duplication key.
- **`file::memory:?cache=shared` is process-wide**, so every "in-memory" test database is the
  same one. Name them uniquely per call or tests only pass in their existing order.
- **The statement kind lists exist twice.** `stmtKinds` in `internal/store/store.go` and
  `web/stmtkind.js` must match; `web/stmtkind.test.mjs` reads the Go source and fails if they
  differ.
- **A time range preset has no end.** "Last hour" is a start that moves with the clock and is
  resolved on every request. Pinning an end at the moment it was chosen freezes the live view.
- **The session token is not a Bearer token.** Back-office routes read the `session` cookie;
  the proxy's service routes read a Bearer token. They are not interchangeable, and the second
  needs no CSRF header.
- **Config changes are not instant.** See the polling note above before debugging a "new server
  does not route" report.

## Where these are tracked

The project has closed, so these are not scheduled. The ones that matter most if it were
continued are listed in [Sprint 3 — Outcomes and handover](sprints/sprint-3.md#handover).
