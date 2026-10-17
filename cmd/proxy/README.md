# Proxy

This directory contains the command entry point for the MySQL auditing proxy.
The implementation and its tests live under [`internal/proxy`](../../internal/proxy).

## How it works

The proxy acts as a transparent, specialized MySQL intermediary. It intercepts connections from MySQL clients, acts as a server toward those clients, and forwards their queries to an upstream MySQL database. 

It accomplishes this via the [`go-mysql`](https://github.com/go-mysql-org/go-mysql) library:

1. **Listening and Accepting**: The proxy listens on a designated TCP port (default `:3307`). When a client connects, `go-mysql`'s `server.Handler` interface securely handles the initial MySQL handshake, capability negotiation, and authentication.
2. **Client Identification**: During the handshake, the proxy captures the client's username via `conn.GetUser()`. The proxy natively understands credentials and authenticates the user directly, making it the primary access gate.
3. **Session Pinning**: Every successfully authenticated client session is granted exactly *one* dedicated, persistent upstream connection to the backend database. This strict 1:1 pinning ensures that `SESSION` variables, transactions, and temporary tables function seamlessly exactly as if the client connected directly to the database.
4. **Relaying Queries**: When the client issues a plain query (via `COM_QUERY`), the proxy's `HandleQuery` intercepts it and forwards the exact SQL to the backend over its pinned connection.
5. **Relaying Prepared Statements**: `COM_STMT_PREPARE` / `COM_STMT_EXECUTE` / `COM_STMT_CLOSE` are also forwarded — `HandleStmtPrepare` prepares the same statement against the upstream and hands back the resulting `*client.Stmt` as the statement's context, `HandleStmtExecute` binds the client's parameters and executes it there, and `HandleStmtClose` frees it upstream too when the client closes it. go-mysql keeps its own per-session statement-ID table (including the original prepared SQL text) and passes it straight back on execute, so the audit event below can record the real SQL, not a placeholder like "executed statement 3".
6. **Transparent Results**: The proxy collects the entire result from the upstream database in memory, and then returns it to the client completely unchanged. Affected-row counts, error codes, result sets, and column metadata are passed through unmodified for plain queries (`COM_QUERY`).

   **Known exception — prepared statement error codes**: a `COM_STMT_EXECUTE` that fails upstream (duplicate key, deadlock, lock timeout, …) reaches the client as `ER_UNKNOWN_ERROR` (1105) instead of the real code (1062, 1213, 1205, …), even though the same failure via a plain query keeps its real code. This is a go-mysql v1.16.0 library defect, not something this proxy does: its internal COM_STMT_EXECUTE dispatch (`server/stmt.go`) wraps whatever error `HandleStmtExecute` returns with `errors.Trace` before its own `writeError` (`server/resp.go`) inspects it — and `writeError` checks the error's type with a plain type assertion rather than `errors.As`, so the wrap defeats it. The original message text (including the real code) still reaches the client inside the error message, just not as the numeric code a client library would compare against. There is no hook in the `Handler` interface to intervene before go-mysql wraps the error, so it cannot be fixed from this proxy without patching or forking go-mysql itself. See the comment on `HandleStmtExecute` in `internal/proxy/runtime/handler_stmt.go` for the full trace through the library source.
7. **Auditing**: Throughout the lifecycle of a session, the proxy observes state changes and emits structured `audit.Event` records to an `audit.Sink`. It currently records:
   - **Connects**: When a client successfully authenticates.
   - **Disconnects**: When a client leaves, or if the upstream drops the connection.
   - **Statements**: The raw SQL queries executed by the client, including the prepared SQL text for statements run via `COM_STMT_EXECUTE` (not the bound parameter values). A `COM_STMT_PREPARE` that upstream rejects (unknown table/column, syntax error, missing privilege) is audited too, from `HandleStmtPrepare`'s own error path — a rejected prepare never reaches execute, so without this a client could probe for table or column names via prepare alone and leave no trail.
8. **Resilience and Health Checking**: 
   - A background watcher continuously monitors the upstream connection health using a periodic `Ping()`.
   - If the upstream server dies, restarts, or hangs completely, the proxy detects it immediately (even while the client is idle) and cleanly terminates the client session, preventing leaked resources.

## Configuration

The proxy can front either one upstream or several, depending on which environment variables are
set. Multi-listener mode itself comes in two flavours — a static file read once at startup, or
live configuration polled from the API and reconciled without a restart — and exactly one of them
must be chosen explicitly: setting both `PROXY_CONFIG_FILE` and `API_PROXY_TOKEN` is a fatal
startup error (`ChooseConfigSource` in `internal/proxy/config/config.go`), not a "file wins" fallback, so a config drifting
into that state fails loudly instead of silently ignoring the API.

### Live configuration, routed by port (`API_PROXY_TOKEN`)

Set `API_PROXY_TOKEN` (and `API_BASE_URL`, default `http://localhost:8080`) to have the proxy load
its routing table from `GET /api/proxy/config` at startup and keep it live from then on: it polls
the same endpoint (ETag-conditional, so an unchanged config is a cheap `304`), and reconciles
whatever changed — added servers get a new listener, removed ones are drained and torn down,
a `listen_port` change binds the new port and only then retires the old one, and any other field
change (upstream host/port/user/password, enabled/disabled) updates the existing listener in
place so already-open sessions keep running on their original upstream until they end or are
drained. Once a snapshot is fully applied the proxy reports that revision to `POST /api/proxy/status`,
authenticated with the same service token, so the back-office can show whether a saved change
is actually live. The report is best effort: a failure is logged and the reconciliation stands. A poll that only partially succeeds (say, one server's new port is already taken) applies
whatever it can and retries the rest on the next poll — one bad entry never blocks the others, and
the config revision only advances once every item in a poll has applied cleanly.

| Variable | Default | Description |
|---|---|---|
| `API_PROXY_TOKEN` | *(unset)* | Service token for `GET /api/proxy/config` and `POST /api/proxy/status`, and the audit sink's token unless `SINK_TOKEN` is set. Setting this switches the proxy into live multi-listener mode |
| `API_BASE_URL` | `http://localhost:8080` | Base URL of the back-office API |
| `PROXY_CONFIG_POLL_INTERVAL` | `5s` | How often the proxy polls for a new config revision |
| `PROXY_CONFIG_REQUEST_TIMEOUT` | `5s` | Per-request timeout against the API |
| `PROXY_CONFIG_RETRY_MAX` | `30s` | Ceiling for the poll loop's exponential backoff after a failed request |
| `PROXY_CONFIG_STARTUP_DEADLINE` | `30s` | How long the proxy retries a failing initial fetch before giving up and exiting — an auth failure (bad token) skips the retry and fails immediately instead of waiting out the deadline |
| `PROXY_DRAIN_TIMEOUT` | `30s` | How long a removed/disabled server's listener waits for its existing sessions to finish on their own before forcibly closing them |
| `PROXY_UPSTREAM_READ_TIMEOUT` | `60s` | Same meaning as in single-upstream mode, applied to every listener |
| `PROXY_COMMAND_TIMEOUT` | `0` | Same meaning as in single-upstream mode, applied to every listener |

Once running, the log line on each successful reconciliation summarizes what changed:

```
2026/09/23 16:00:47 reconciled config revision 1 -> 2: 1 added, 0 updated, 0 moved, 0 removed (1 total)
```

If the API is unreachable mid-run, the proxy keeps serving its last known-good configuration and
logs the poll failures; it catches up automatically once the API comes back, with no listener ever
dropped just because a poll failed.

### Multiple upstreams, routed by port (`PROXY_CONFIG_FILE`)

Set `PROXY_CONFIG_FILE` to a JSON file shaped like `deploy/upstreams.json` — `{"servers": [...]}`,
each entry an `id`, `name`, `listen_port`, `upstream_host`, `upstream_port`, `upstream_user` and
`enabled` (the same shape `GET /api/servers` returns, so the API can read the same file). The
proxy opens one listener per **enabled** entry, each with its own upstream and, via
`PROXY_UPSTREAM_PASSWORDS`, its own password — the file itself carries no passwords, since it is
also served to the back-office API.

Every enabled entry is validated before any listener is opened: `name`, `upstream_host` and
`upstream_user` must be set, `listen_port` and `upstream_port` must each be in `1–65535` (`0` is
rejected rather than silently bound to a random port), and both `name` and `listen_port` must be
unique across the enabled entries. A bad config fails the proxy at startup with a clear error
instead of only failing once a client tries to connect through the broken entry.

| Variable | Default | Description |
|---|---|---|
| `PROXY_CONFIG_FILE` | *(unset)* | Path to the upstreams JSON file. Setting this switches the proxy into multi-listener mode and the single-upstream variables below are ignored |
| `PROXY_UPSTREAM_PASSWORDS` | *(none)* | Per-upstream passwords, keyed by each entry's `name`: `db-a:<bootstrap password>,db-b:<bootstrap password>` — the passwords of each entry's `upstream_user`, the bootstrap account. A configured, enabled upstream with no matching entry here is a fatal startup error, not a fallback to some other upstream's password |
| `SINK_INGEST_URL` | *(unset)* | Where audit events are posted, e.g. `http://api:8080/api/events`. Unset logs them instead, so the proxy runs without the API |
| `SINK_BATCH_SIZE` | `50` | Events per POST |
| `SINK_FLUSH_INTERVAL` | `1s` | How often a partial batch is sent anyway |
| `SINK_ON_FULL` | `block` | What happens when the audit buffer is full: `block` holds the query until there is room, `drop` discards the event and lets the query through. Anything else is a fatal startup error |
| `SINK_TOKEN` | `API_PROXY_TOKEN` | Bearer token the sink sends on `POST /api/events`, which the API requires. Set it on its own for a file-mode proxy, where `API_PROXY_TOKEN` cannot be set. With neither set the proxy warns at startup and every batch is rejected with `401` |

```bash
PROXY_CONFIG_FILE=deploy/upstreams.json \
PROXY_UPSTREAM_PASSWORDS="db-a:rootpass,db-b:rootpass-b" \
go run ./cmd/proxy
```

```
PROXY_CONFIG_FILE=deploy/upstreams.json: routing db-a (0.0.0.0:3307), db-b (0.0.0.0:3308)
```

### Single upstream (`PROXY_CONFIG_FILE` unset)

Without `PROXY_CONFIG_FILE`, the proxy falls back to one upstream from these variables — the
original Sprint 1 shape, still what `go run ./cmd/proxy` uses with no setup:

| Variable | Default | Description |
|---|---|---|
| `PROXY_LISTEN_ADDR` | `0.0.0.0:3307` | Address and port to listen on |
| `PROXY_UPSTREAM_ADDR` | `127.0.0.1:3306` | Upstream MySQL address, recorded on events as `upstream_addr` |
| `PROXY_UPSTREAM_NAME` | `db-a` | Configured server name, recorded on events as `upstream` — what the UI groups and filters by |
| `PROXY_UPSTREAM_USER` | `root` | The proxy's own bootstrap login on the upstream. In production this is an account with `GRANT USAGE` only (`proxy_bootstrap` in `deploy/`); the default `root` is for local development only. Clients never use it: each logs in as themselves |
| `PROXY_UPSTREAM_PASS` | `rootpass` | Upstream MySQL password |
| `PROXY_UPSTREAM_READ_TIMEOUT`| `60s` | Maximum time allowed for any single read operation against the upstream. Set to `0` to disable. |
| `PROXY_COMMAND_TIMEOUT` | `0` | Explicit maximum duration a single query can run before the proxy intervenes and kills the connection. `0` disables it (unbounded). |
| `SINK_INGEST_URL` | *(unset)* | Where audit events are posted, e.g. `http://api:8080/api/events`. Unset logs them to stdout instead. |
| `SINK_BATCH_SIZE` | `50` | Events per POST |
| `SINK_FLUSH_INTERVAL` | `1s` | How often a partial batch is sent anyway |
| `SINK_ON_FULL` | `block` | What happens when the audit buffer is full: `block` holds the query until there is room, `drop` discards the event and lets the query through. Anything else is a fatal startup error |
| `SINK_TOKEN` | `API_PROXY_TOKEN` | Bearer token the sink sends on `POST /api/events`, which the API requires. Set it on its own for a file-mode proxy, where `API_PROXY_TOKEN` cannot be set. With neither set the proxy warns at startup and every batch is rejected with `401` |

### Users and privileges

The proxy does not keep a user list. Each client logs in as itself and **the database decides**
(ADR 0004, `docs/design/decisions/0004-pass-through-authentication.md`):

1. The proxy opens a TLS connection to the upstream as the *bootstrap account* — the entry's
   `upstream_user`, which should have `GRANT USAGE ON *.*` and nothing else.
2. It sends `COM_CHANGE_USER` for the user that is connecting, so the database names that
   account's real authentication plugin and challenge.
3. It relays that challenge to the client and the client's answer back, as opaque bytes. The
   proxy never sees a plaintext password it could compare, and never logs or stores one.
4. If the database accepts, the session continues as that user. `SELECT USER()` returns the
   connecting user and the user's own grants apply: a table without a grant is refused by the
   database with error 1142, and that refusal is audited like any other statement.

```
sink event: conn=19c56469-... kind=connect user="alice" ... upstream=db-a upstream_addr=127.0.0.1:3306 ...
```

Things to know:

- **Clients must use TLS to the proxy**, because the database's authentication may need the
  password in the clear (`caching_sha2_password` full authentication, `sha256_password`). A
  plaintext client gets error 3159. The proxy's certificate is self-signed, so clients use
  `--ssl-mode=REQUIRED`/`PREFERRED` or `tls=skip-verify`.
- **A user must exist in the database** (for the upstream they are routed to). Adding a user
  there is all it takes; the proxy needs no change.
- **A failure to log in as the bootstrap account** (wrong password, upstream down, TLS not
  verifiable) gives the client a generic error and an operator-level log line. It is not an
  `auth_fail` event and never names the bootstrap account to the client.
- **The 1045 a client sees comes from the database**, so it names the address the database saw
  — the proxy's — not the client's.
- **`PROXY_USERS` is gone.** Setting it is a fatal startup error, so a stale deployment cannot
  silently run with different rules from the ones its operator wrote down.

A login the database refuses — a wrong password or unknown user (error 1045), a locked account (3118), a host the account may not connect from (1130), a user over its connection limit (1226) — emits an
`auth_fail` event carrying the attempted username, `auth_result=fail` and the failure reason
(go-mysql's `OnAuthFailure` hook, wired up in `auditingAuthHandler`), not a `connect` event:

```
sink event: conn=2e9a5b13-... kind=auth_fail user="alice" client=127.0.0.1:54321 proxy=3307 upstream=db-a upstream_addr=127.0.0.1:3306 at=...
```

(the reason is on the event's `err` field, which the fake sink's log line above doesn't print —
the batched HTTP sink forwards it to the API as documented in `docs/design/data-model.md`, which
stores it as `connections.auth_fail_reason` and returns it from `GET /api/connections`)

A rejected login is only ever audited when it really is one. `readHandshakeResponse` (inside
go-mysql) calls this proxy's `UseDB` — for a client's default database, named as part of the
handshake — before it ever compares credentials, so if `UseDB` touched the upstream at that point,
a bad database name would fail the handshake on the upstream's "unknown database" error before the
password was even checked: a rejected login with a bogus database would go unaudited (or audited
under the wrong reason), and an unauthenticated client could use the database name to probe which
databases exist upstream. So `UseDB` defers during the handshake — it only records the requested
name — and `auditingAuthHandler.OnAuthSuccess` applies it for real once credentials have actually
been checked, immediately before the client would otherwise receive the OK packet. See the
comments on `proxyHandler.UseDB` and `proxyHandler.applyPendingDB` in `internal/proxy/runtime/handler.go`,
and `auditingAuthHandler.OnAuthSuccess` in `internal/proxy/runtime/auth.go` for the full mechanism.
The defaults match the development credentials in `deploy/.env.example` (which `make up` copies to
`deploy/.env`), so no env vars are needed for local development.

## Health and metrics

Set `PROXY_ADMIN_ADDR` (for example `:9100`) and the proxy serves a small admin HTTP server on that
address, separate from the client-facing listeners. Unset, there is no admin server. It starts
before the configuration is loaded, so a probe sees "not ready" while the proxy waits for the API,
not a refused connection. A bind failure is fatal.

| Endpoint | What it says |
|---|---|
| `GET /healthz` | `200 ok` once the configuration is applied and every listener is bound; `503 not ready: <reason>` until then. It does not look at the audit sink: a sink that is dropping or failing is not fixed by a restart, so it is reported in the metrics instead. |
| `GET /metrics` | Prometheus text format, below. |

| Metric | Type | Meaning |
|---|---|---|
| `lustre_audit_queue_depth` | gauge | Audit events buffered and not yet picked up for delivery. Settles at the buffer size when the API stalls. |
| `lustre_audit_emitted_total` | counter | Events handed to the sink. |
| `lustre_audit_sent_total` | counter | Events delivered to the API. |
| `lustre_audit_dropped_total` | counter | Events discarded: buffer full with `SINK_ON_FULL=drop`, or the sink shutting down. |
| `lustre_audit_failed_total` | counter | Events lost because delivery failed after retries. |
| `lustre_config_applied_revision` | gauge | Highest configuration revision fully applied. Only in live mode (`API_PROXY_TOKEN`). |

The audit metrics are absent when `SINK_INGEST_URL` is unset, because the logging sink has no queue
to report. **Do not publish this port or route it through the edge**: nothing on it is for clients,
and the drop counters describe how much audit data is being lost. `deploy/docker-compose.yml` sets
it to `:9100` on the compose network and does not publish it.

## Quick start

```bash
# 1. Install Go dependencies (first time only)
go mod tidy

# 2. Start the stack (both upstreams + one proxy listening on both ports + API)
make up

# ...or run just the upstreams and the proxy on the host, single-upstream mode:
make test-env
go run ./cmd/proxy

# 4. Connect through the proxy (in another terminal)
mysql -h 127.0.0.1 -P 3307 -u root -prootpass

# 5. Run a query
SELECT 1;
```

From inside the Docker client container:
```bash
docker compose -f deploy/docker-compose.yml exec client mysql -h proxy -P 3307 -u root -prootpass
```

## Source layout

`cmd/proxy` is the executable entry point. The implementation lives under
`internal/proxy`, split into `app`, `config`, and `runtime`. Build and run commands
still target `./cmd/proxy`; proxy tests target `./internal/proxy/...`.

### Entry point and startup

| File | Responsibility |
|---|---|
| [`cmd/proxy/main.go`](main.go) | Calls `app.Run()`; contains no proxy implementation |
| [`app/app.go`](../../internal/proxy/app/app.go) | Reads process settings, creates the audit sink, selects the configuration source, and starts the chosen mode |
| [`app/startup.go`](../../internal/proxy/app/startup.go) | Wires static-file startup or the initial API fetch, listener reconciliation, and ongoing polling |

### Configuration (`internal/proxy/config`)

| File | Responsibility |
|---|---|
| [`config.go`](../../internal/proxy/config/config.go) | Environment helpers and defaults, audit sink construction, password-map parsing, file loading, server validation, source selection, and initial-fetch retries |
| [`configclient.go`](../../internal/proxy/config/configclient.go) | API client and snapshot types, bearer authentication, ETag handling, request timeouts, polling, and retry backoff |

This package obtains and validates settings. It does not open MySQL listeners or
apply routing changes; those belong to `runtime`.

### Serving and reconciliation (`internal/proxy/runtime`)

| File | Responsibility |
|---|---|
| [`proxy.go`](../../internal/proxy/runtime/proxy.go) | `Proxy` and immutable upstream configuration, the listener's `Run` loop, `BuildProxies`, and `RunAll` |
| [`session.go`](../../internal/proxy/runtime/session.go) | Per-client setup and command loop, connection tracking, upstream updates, session counting, and draining/forced closure |
| [`handler.go`](../../internal/proxy/runtime/handler.go) | Per-session command handler, plain queries, database selection, error classification, and statement audit events |
| [`handler_stmt.go`](../../internal/proxy/runtime/handler_stmt.go) | Prepared statement prepare/execute/close forwarding and auditing |
| [`handler_health.go`](../../internal/proxy/runtime/handler_health.go) | Idle upstream health checks and command-timeout detection |
| [`sql.go`](../../internal/proxy/runtime/sql.go) | Statement-type classification and parsing a `USE` statement's database name |
| [`auth.go`](../../internal/proxy/runtime/auth.go) | Authentication callbacks, failed-login auditing, and applying the requested database after successful login |
| [`tls.go`](../../internal/proxy/runtime/tls.go) | Client-facing certificate/handshake material, upstream TLS configuration, and atomic TLS-aware upstream updates |
| [`configpoll.go`](../../internal/proxy/runtime/configpoll.go) | Turns API poll results into reconciliation, retries partially applied snapshots, and commits successful revisions/ETags |
| [`manager.go`](../../internal/proxy/runtime/manager.go) | Computes configuration change plans and records applied state; does not bind or drain listeners |
| [`reconciler.go`](../../internal/proxy/runtime/reconciler.go) | Coordinates applying a plan, reports results, and tracks which server IDs have managed listeners |
| [`reconciler_apply.go`](../../internal/proxy/runtime/reconciler_apply.go) | Individual add, update, move-port, and remove operations |
| [`reconciler_listener.go`](../../internal/proxy/runtime/reconciler_listener.go) | Managed listener creation, accept gates, listener generations, retirement, and draining |
| [`reconciler_ports.go`](../../internal/proxy/runtime/reconciler_ports.go) | Orders changes that share ports and handles port swaps, bind checks, and rollback |
| [`reconciler_watch.go`](../../internal/proxy/runtime/reconciler_watch.go) | Detects unexpected listener exits and supervises restarts with backoff |

Dependencies flow from `app` to `runtime` and `config`, and from `runtime` to
`config`. Reconciliation stays with listeners and sessions because they share
lifecycle state. Keeping them in one package also lets their tests inspect that
private state without exposing it across packages.

For a client connection, follow `proxy.go` → `session.go` → `auth.go`/`tls.go` →
the `handler*.go` files. For a live configuration change, follow
`config/configclient.go` → `runtime/configpoll.go` → `manager.go` → the
`reconciler*.go` files. `app/startup.go` connects these parts at process startup.

The runtime also uses the existing shared packages:
[`internal/passthrough`](../../internal/passthrough) for the authentication exchange,
[`internal/upstream`](../../internal/upstream) for upstream TLS connections,
[`internal/audit`](../../internal/audit) for event/server types and the sink interface,
and [`internal/sink`](../../internal/sink) for sink implementations constructed by
`config.NewAuditSink`.

### Where tests belong

Tests live beside the implementation and use the same Go package. Add cases to the
relevant group rather than creating a file for each regression. Existing test
names are preserved, so `go test -run` filters still work.

| Test file | Coverage or shared support |
|---|---|
| [`config/config_test.go`](../../internal/proxy/config/config_test.go) | File loading, environment validation, server validation, configuration-source selection, and initial-fetch retries |
| [`config/configclient_test.go`](../../internal/proxy/config/configclient_test.go) | API requests, response validation, ETags, polling, timeouts, and backoff |
| [`runtime/helpers_test.go`](../../internal/proxy/runtime/helpers_test.go) | Shared audit sink, database/proxy fixtures, logging, and `TestMain` upstream-skip reporting |
| [`runtime/proxy_test.go`](../../internal/proxy/runtime/proxy_test.go) | Connection events, concurrent sessions, proxy construction, and multi-upstream routing |
| [`runtime/session_test.go`](../../internal/proxy/runtime/session_test.go) | Cancellation, login deadlines, session tracking, draining, and bootstrap credential updates |
| [`runtime/handler_test.go`](../../internal/proxy/runtime/handler_test.go) | Query forwarding, statement classification/auditing, database selection, results/errors, and session pinning |
| [`runtime/handler_stmt_test.go`](../../internal/proxy/runtime/handler_stmt_test.go) | Prepared statement execution, reuse, cleanup, failure auditing, and upstream loss |
| [`runtime/handler_health_test.go`](../../internal/proxy/runtime/handler_health_test.go) | Dropped or hung upstreams, idle pings, and command/read timeouts; includes fake upstream helpers |
| [`runtime/auth_test.go`](../../internal/proxy/runtime/auth_test.go) | Login acceptance/rejection, failed-login auditing, authentication plugins, and database-name disclosure regressions |
| [`runtime/tls_test.go`](../../internal/proxy/runtime/tls_test.go) | Client TLS negotiation, plaintext refusal, upstream TLS setup, and TLS setting validation |
| [`runtime/manager_test.go`](../../internal/proxy/runtime/manager_test.go) | Configuration diffing and applied-state bookkeeping |
| [`runtime/reconciler_test.go`](../../internal/proxy/runtime/reconciler_test.go) | Listener changes, partial failures, shared reconciliation fixtures, and retrying a pending snapshot after a `304` |
| [`runtime/reconciler_ports_test.go`](../../internal/proxy/runtime/reconciler_ports_test.go) | Port reuse/swaps, failed-swap rollback, and recovery of missing listeners |
| [`runtime/reconciler_watch_test.go`](../../internal/proxy/runtime/reconciler_watch_test.go) | Listener restart, concurrent update/removal, accept gating, session lineage, and TLS updates |

Process-level startup and live routing are also exercised by the separate
[`e2e`](../../e2e) suite, which builds and runs `cmd/proxy` as a subprocess. There
are no test files in `cmd/proxy` or `internal/proxy/app`; their startup wiring is
covered through these executable-level tests.

## Running the tests

The integration tests require a running MySQL instance (the Docker Compose `db-a` container):

```bash
# Start the upstream databases (creates deploy/.env on first run)
make test-env

# Run all proxy tests
go test -v -count=1 ./internal/proxy/...
```

### What the tests cover

Tests stay in the package they exercise and are grouped by responsibility.
Shared fixtures, database helpers, and upstream-skip reporting live in
`runtime/helpers_test.go`. Existing test names are preserved, so `go test -run`
filters still work.

**Core Behavior (`proxy_test.go` and `handler_test.go`):**

| Test | What it proves |
|------|----------------|
| `TestProxyConnectEvent` | Connecting emits a `connect` event with correct metadata. |
| `TestProxyDisconnectEvent` | Disconnecting emits a `disconnect` event. |
| `TestProxyConcurrentSessions` | Multiple concurrent clients can connect and operate independently. |
| `TestProxyQueryRelay` | Queries (`SELECT 1`) accurately execute and return responses. |
| `TestProxyStatementEvent` | Executing queries emits `statement` events. |
| `TestProxyStatementTypeClassification` | Statement events correctly classify `DDL` and `DML` types. |
| `TestProxyStatementDB` | Statement events correctly capture the active database (via `USE` commands). |
| `TestProxyFailingQuery` | Failing queries are handled gracefully and emitted as events. |
| `TestProxyIdenticalQueries` | Duplicate queries emitted rapidly are captured uniquely. |

**Regression Coverage (`auth_test.go`, `handler_test.go`, `handler_stmt_test.go`,
and `handler_health_test.go`):**

| Test | What it proves |
|------|----------------|
| `TestVerify_AcceptsOtherUsernames` | Non-default users can connect and are accurately recorded in audit events. |
| `TestVerify_WriteReportsAffectedRows` | `INSERT` reports real affected rows and insert IDs, not zeros. |
| `TestVerify_TruncatedResultSetIsAnError` | An upstream connection lost mid-`SELECT` surfaces as an explicit error. |
| `TestVerify_SessionPinnedToOneUpstreamConn`| One client stays pinned to one upstream connection (transactions/variables are safe). |
| `TestVerify_UpstreamLossClosesClientSession` | A lost upstream connection cleanly tears down the client session instead of failing. |
| `TestVerify_IdleSessionDetectsUpstreamLoss` | A client sitting idle will be disconnected gracefully if the upstream drops. |
| `TestVerify_HungUpstreamDetected` | `PROXY_COMMAND_TIMEOUT` correctly kills active queries that exceed the duration ceiling. |
| `TestVerify_PingHungUpstreamDetected` | The background watcher correctly aborts wedged health pings even when `PROXY_UPSTREAM_READ_TIMEOUT=0`. |
| `TestVerify_PingHungUpstreamDetectedWithReadTimeout` | The ping deadline still holds when `PROXY_UPSTREAM_READ_TIMEOUT` is set longer than it, which go-mysql's own read deadlines would otherwise override. |
| `TestVerify_AuthFailureEmitsEvent` | A wrong password for a known user emits an `auth_fail` event with the attempted username and a reason. |
| `TestVerify_AuthFailureUnknownUser` | Same, for a user that does not exist in the database. |
| `TestVerify_AuthFailureDoesNotEmitConnect` | A rejected login never also emits a `connect` event. |
| `TestVerify_BareTCPDisconnectDoesNotEmitAuthFail` | A bare TCP connect-and-close never parses a username, so it must not be recorded as a failed login. |
| `TestVerify_ValidUserInvalidDBDoesNotEmitAuthFail` | A valid user naming a database that doesn't exist upstream fails in `OnAuthSuccess`, not as a credential rejection, so it emits no `auth_fail`. |
| `TestVerify_WrongPasswordWithInvalidDBIsStillAudited` | A wrong password is still audited when the client also names a junk database — a brute-force attempt can't dodge the log by appending one. |
| `TestVerify_UnknownUserWithInvalidDBIsStillAudited` | Same, for a user that does not exist in the database. |
| `TestVerify_UnknownUserReasonIsNormalized` | The unknown-user reason names the attempted user instead of forwarding go-mysql's misleading "specified as a definer" text. |
| `TestVerify_RejectedLoginDoesNotRevealDatabaseExistence` | A rejected login returns the same error code whether the named database exists or not, so an unauthenticated client can't enumerate databases upstream. |
| `TestVerify_PreparedStatementExecutesAndAudits` | A server-side prepared statement (`COM_STMT_PREPARE`/`EXECUTE`) works through the proxy and the audit log records the prepared SQL, not a statement id. |
| `TestVerify_PreparedStatementReusedAcrossExecutions` | The same prepared statement executed with different arguments audits the same SQL text each time. |
| `TestVerify_PreparedStatementCloseFreesUpstream` | Closing a prepared statement (`COM_STMT_CLOSE`) frees it on the upstream without breaking the rest of the session. |
| `TestVerify_PreparedStatementFailureIsAudited` | A prepared statement that fails at execute (not prepare) still produces a statement event carrying the error. |
| `TestVerify_PreparedStatementUpstreamLossClosesSession` | A lost upstream connection during a prepared-statement execute closes the session, same as it does for a plain query. |
| `TestVerify_FailedPrepareIsAudited` | A `COM_STMT_PREPARE` upstream rejects (unknown table) still produces a statement event, so a probe that never reaches execute doesn't skip the audit log. |
| `TestVerify_PreparedStatementErrorCodeBecomesGeneric` | Pins the known go-mysql limitation above: a duplicate-key failure keeps its real code (1062) via a plain query but loses it to `ER_UNKNOWN_ERROR` (1105) via a prepared statement. |

`TestLoadUpstreamConfig_FiltersDisabled`, `TestBuildProxies_MissingPassword`,
`TestBuildProxies_PerServerFields`, `TestBuildProxies_JoinsIPv6Host`, `TestValidateServers_Valid`
and `TestValidateServers_Invalid` cover the multi-listener config loading, address construction
and startup validation directly — no MySQL needed, so they always run. `TestMultiUpstreamRouting`
is the "two databases
at once" case end to end: it builds two listeners from one config, each fronting a different real
upstream (`db-a`, `db-b`), and checks both halves of "done" — a client on each port reaches that
port's own data, and each session's audit events carry the matching `upstream`/`upstream_addr`,
not whichever proxy happened to log it first. It needs both upstreams and skips (like the tests
above) if either is unreachable.

**Live configuration (`config_test.go`, `reconciler_test.go`, `session_test.go`,
`e2e/live_reconciliation_test.go`):**
| Test | What it proves |
|------|----------------|
| `TestChooseConfigSource` | All four combinations of `PROXY_CONFIG_FILE`/`API_PROXY_TOKEN` resolve to the right source, or the right ambiguous/unconfigured error. |
| `TestFetchInitialSnapshotWithRetry_*` | Startup retries transient API failures, fails fast on a bad token, times out with a clear error, and honours context cancellation. |
| `TestReconciler_Apply*` (add/update-in-place/move-port/remove, plus `PartiallyFailsAcrossMultipleItemsInOneCall`) | Each reconciliation action does the right thing to real listeners, and one failing item in a batch never blocks the others. |
| `TestVerify_DrainForciblyClosesSessionAfterTimeout` | A session with a query outliving the drain timeout is forcibly disconnected, not left to finish. |
| `TestLiveConfig_*` (E2E, real subprocesses) | POST/PUT/DELETE against the API bring up, re-route, or drain listeners without a proxy restart; a port change brings up the new port before retiring the old one; API downtime is absorbed and caught up on recovery; a failed bind preserves the last working config; a proxy restart reconstructs the latest database state; disabling a server stops new connections while draining existing ones. |

### Test configuration

The tests use the same defaults as the proxy. Override with env vars if your upstream is elsewhere:

```bash
PROXY_TEST_UPSTREAM_ADDR=127.0.0.1:3306 \
PROXY_TEST_UPSTREAM_USER=root \
PROXY_TEST_UPSTREAM_PASS=rootpass \
go test -v -count=1 ./internal/proxy/...
```

`TestMultiUpstreamRouting` also needs a second upstream, matching `db-b`; override the same way if
yours is elsewhere:

```bash
PROXY_TEST_UPSTREAM_B_ADDR=127.0.0.1:3316 \
PROXY_TEST_UPSTREAM_B_USER=root \
PROXY_TEST_UPSTREAM_B_PASS=rootpass-b \
go test -v -count=1 -run TestMultiUpstreamRouting ./internal/proxy/runtime
```

If no upstream MySQL is reachable, tests are **automatically skipped** (not failed).

## Key types

| Type | Role |
|------|------|
| `Proxy` | Holds config + sink; `Run()` starts the listener |
| `proxyHandler` | Implements `go-mysql`'s `server.Handler`; forwards queries over one `client.Conn` |
| `audit.Sink` (interface) | Receives `audit.Event`s — the proxy's only external dependency |
| `sink.Fake` | Logs events to stdout (placeholder for the batched HTTP sink) |
| `audit.Event` | Audit record shared with the API — see `docs/design/data-model.md` §Ingest |

## What needs to be implemented for Sprint 2

*   **Robust Telemetry & Metrics:** Expose Prometheus metrics (e.g., active sessions, query latency, connection errors) to monitor the proxy's operational health.
*   **Failed Login Auditing:** Ensure failed authentication attempts generate distinct audit events for security monitoring.
*   **Client Disconnect Mitigation (DoS Prevention):** Ensure that if a client TCP connection drops mid-query, the proxy actively cancels the running upstream query rather than holding the connection open.
*   **Multi-Statement Support:** Handle `CLIENT_MULTI_STATEMENTS` to ensure the proxy properly drains all result sets to prevent protocol desynchronization.
*   **Large Query Resilience:** Validate and test proxy stability when handling and logging query packets exceeding standard `max_allowed_packet` limits.
*   **Upstream Reachability UX:** Return proper MySQL error packets to the client when upstream connections fail before the handshake, rather than abruptly dropping the TCP connection.
*   **Dynamic Configuration Updates:** Done for routing — see "Live configuration" above: listeners are added, updated, moved, and removed without a restart. Client logins are no longer configured in the proxy at all (see "Users and privileges").
*   **High Availability Support:** Handle failover gracefully if `PROXY_UPSTREAM_ADDR` routes to a load balancer and a backend node steps down.

## Notes

- This directory stays focused on the proxy logic.
- The back-office API and SQLite store are out of scope for this directory; the proxy depends only on the `Sink` interface.
## Upstream TLS

All upstream connections require TLS 1.2 or newer. Per-server configuration adds
`tls` (`verify` by default, or development-only `skip-verify`) and `tls_ca_file`
(optional PEM CA bundle; empty uses system trust roots). Verification checks both
trust and the configured upstream hostname/IP. There is no plaintext mode or retry.
`skip-verify` emits a startup warning and still refuses a server without TLS.
Invalid CA files fail startup; TLS/authentication handshakes have a bounded deadline.

The same fields work in `PROXY_CONFIG_FILE` and `/api/proxy/config`. Paths are local
to the proxy process/container. Restart after configuration or CA changes. See
[deployment instructions](../../deploy/README.md#mandatory-upstream-tls) for mounts,
certificate SANs and the required `make test-tls` acceptance suite.

TLS configuration and client handshake material live in `internal/proxy/runtime/tls.go`, backed by the shared
`internal/upstream` package. Authentication hooks live in `internal/proxy/runtime/auth.go`, and client
session setup lives in `internal/proxy/runtime/session.go`. The
`internal/passthrough` bootstrap exchange is adapted from
the `spike/53-passthrough` tag (commit `d6ce56e7ac00aa7aa82009bece36349f1ace90e3`), uses
that same connector, and never pools connections. Its TLS-only relay omits the
spike's plaintext password decoding/re-encryption path. The production proxy's
existing client authentication remains in place for the separate pass-through MR;
that MR should supply the client relay and distinguish bootstrap failures from
user authentication failures. Both legs must be encrypted before relaying through
`DialChangeUser`.
