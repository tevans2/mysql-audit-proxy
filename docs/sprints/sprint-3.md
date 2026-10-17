# Sprint 3 — Outcomes and handover

Sprint 3 ran from the 24 Sep 2026 demo to project close on 16 Oct 2026. It was the last sprint,
so this page records what it delivered, what the finished system is, and what is left for whoever
carries it on. It was written at close from the merge history and the code, not planned in
advance; there is no separate Sprint 3 plan.

## What drove the sprint

Two answers from the client at the Sprint 2 demo, and one commitment carried over from Sprint 2:

- **Each user logs in to the database as themselves**, so the database enforces its own
  privileges. → pass-through authentication ([ADR 0004](../design/decisions/0004-pass-through-authentication.md)).
- **Audit completeness over query latency** when the audit buffer fills. → the sink blocks by
  default ([ADR 0005](../design/decisions/0005-audit-sink-blocks-by-default.md)).
- **Configuration made in the back-office has to reach the running proxy** (deferred from Sprint 2).
  → live configuration polling and listener reconciliation.

Retention, the audit store (SQLite or Postgres), raw SQL with literal values (POPIA) and per-user
authorisation beyond the database's own grants were **not answered**; the defaults we chose are in
the ADRs and [known limitations](../known-limitations.md).

## What shipped

Merge requests that landed on `main` after the Sprint 2 demo, by theme:

| Theme | What | MR |
|---|---|---|
| Proxy | Pass-through authentication: the proxy opens the session as a bootstrap user and has the database switch it to the connecting user, relaying the database's own challenge | #75 |
| Proxy | Live proxy configuration: poll the API, reconcile listeners without a restart, report the applied revision | #40 |
| Proxy | Audit sink blocks on a full buffer by default, `SINK_ON_FULL=drop` to opt out | #56 |
| Proxy | Health and audit-queue metrics on an unpublished admin port (`/healthz`, `/metrics`) | #49 |
| Proxy | `cmd/proxy/main.go` split into focused files | #81 |
| API | `/api/events` requires the service token (it accepted unauthenticated writes) | #44 |
| API | Retention for the audit log: off by default, delete or archive, dry run ([ADR 0006](../design/decisions/0006-audit-log-retention.md)) | #60 |
| API | Named admin accounts, attribution log, login rate limiting | #80 |
| API | HttpOnly cookie sessions, `/api/logout`, CSRF header on every state-changing route | #78 |
| API | Statement filters (`has_error`, `kind`, `upstream`), `open=1`, `/api/summary`, paginated `/api/actions`, `last_login_at` | #83 |
| API | Server-side search, filtering and pagination; handshake filtering moved into the query | #52, #57, #59 |
| Edge | Strict CSP and security headers; the UI is baked into the edge image | #79 |
| UI | Shows the configuration revision the running proxy has applied | #58 |
| UI | Statement list shows the statement, not its comment prefix | #82 |
| UI | Back office rebuilt to the redesign: Activity (statements and sessions), session drawer, Servers, Admins, sign-in states | #84 |
| Tests | End-to-end tests for per-user identity | #74 |
| Perf | Re-run with blocking and per-user login ([results](../perf/sprint-3-rerun.md)) | `perf-rerun-block-mode` |
| Store | Hide DBeaver and Connector/J connect-time statements | `hide-dbeaver-handshake-noise` |

Decisions recorded as ADRs this sprint: [0004](../design/decisions/0004-pass-through-authentication.md)
(accepted), [0005](../design/decisions/0005-audit-sink-blocks-by-default.md),
[0006](../design/decisions/0006-audit-log-retention.md) and
[0007](../design/decisions/0007-sqlite-remains-the-audit-store.md).

## The finished system in one paragraph

A Go proxy sits in front of one or more MySQL servers, one listener per configured server. A
client connects over TLS as a real database user; the database checks the password and applies the
user's grants. Every connection, failed login and statement becomes an audit event, posted in
batches to a Go API that stores them in SQLite. A web back-office, served over HTTPS by Caddy,
lets named administrators search the audit log, open a session's timeline, and manage the proxy's
servers; a change there reaches the running proxy within a few seconds. Measured on one laptop, the
proxy added about 1–4 ms per query at 25–50 connections (Sprint 2 baseline) and about 2.2 ms per
connection (Sprint 3 re-run), and keeps every audit record up to about 18,500 statements/second;
the specified load is about 2,500/s.

## Handover

What is left, grouped by whether it is housekeeping or genuine work. Nothing here is scheduled.

### Housekeeping

Branches were cleared at close: everything merged, and everything superseded, was deleted. The
one thing kept is the pass-through authentication prototype, as the tag `spike/53-passthrough`
(commit `d6ce56e`), because the spike write-ups in `docs/design/spikes/` run it.

- **File the three #80 follow-ups as issues.** Write the attribution row in the same transaction
  as the change; trust `X-Forwarded-For` only from the edge; allow re-enabling a disabled admin.
- **Write the Sprint 3 demo runsheet and retrospective.** `docs/meetings/` and
  `docs/sprints/` have both for Sprint 1 and a runsheet for Sprint 2, and nothing for Sprint 3.
- **Fix the flaky test** `TestRun_CancelledWhileAcceptGateIsClosedReleasesThePort` (a port is still
  bound when `Run` returns). It fails repeatedly on a developer laptop and is unrelated to recent
  changes.
- **CI MySQL jobs fail with "No space left on device".** A runner-side problem; ask whoever runs
  the runner.

### Decisions that need the client

- How long to keep audit records, and delete or archive at expiry (the system keeps everything
  until told otherwise).
- Whether raw SQL with literal values is acceptable (POPIA).
- Whether per-user authorisation beyond the database's grants is wanted, or the audit trail is
  enough.
- SQLite or Postgres, if they have a preference ([ADR 0007](../design/decisions/0007-sqlite-remains-the-audit-store.md)).

### Engineering, most valuable first

1. **Show audit health in the back-office.** The proxy already exposes queue depth and drop
   counters; nothing displays them. Audit loss is the failure that matters most for this product.
2. **Durable delivery.** A sustained API outage still loses records after the sink's retries. A
   disk buffer or queue closes it.
3. **Overview tiles and filter controls in the UI.** `GET /api/summary` is built and unused. The
   statement filters (`has_error`, `kind`, `upstream`) and `open=1` have API support and no
   controls.
4. **Make live updates feel instant.** Poll incrementally with an `after_id` cursor and a 1 s
   interval, prepending new rows; an SSE nudge on the cookie session is the step after that.
5. **Read-path performance at scale.** Page connections before counting statements; decide FTS5
   or a documented limit for free-text search; introduce a `Store` interface
   ([ADR 0007](../design/decisions/0007-sqlite-remains-the-audit-store.md)).
6. **Streaming result sets**, so a large `SELECT` does not buffer in the proxy.
7. **Tamper evidence** for the audit log (hash chaining was designed).
8. **Browser tests** for the back-office.
9. **Production hardening:** expose only the proxy ports, replace the self-signed client
   certificate, run more than one proxy instance, and review the deploy README's secrets list.
