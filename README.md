# Lustre — MySQL Auditing Proxy (ACUM-1)

A MySQL **auditing proxy** built for our CS344 client, **Acumen Software**.

The proxy sits between database clients (CLI, DBeaver, etc.) and one or more upstream
MySQL servers. It relays traffic transparently and **audit-logs every connection and
query** — who ran what, when, and against which server — while a **web back-office** lets
an administrator review the logs and manage the proxy's server/port configuration.

> Internal tooling: the goal is a working, demonstrable system, built as though it could
> become production.

## Team

| Member | Role |
|--------|------|
| Joe Mudge | Proxy core (lead) + audit pipeline |
| Mieke Wepener | Proxy core / audit pipeline (pairs with Joe) |
| Keagan Slabbert | Store + API (audit store, back-end API, data model) |
| Maria Kotze | Back-office UI |
| Tate Evans | Infra / QA + planning/speccing + integration + coordination (works across all parts, glues them together) |

**Client contact:** Cameron Hatch · **Demi (marker):** Sphelelo Mhlongo

## Tech stack

- **Proxy:** Go (using the `go-mysql` library for the MySQL wire protocol) — see
  [ADR 0001](docs/design/decisions/0001-proxy-language.md).
- **Audit store:** SQLite — see [ADR 0007](docs/design/decisions/0007-sqlite-remains-the-audit-store.md)
  for why it stayed.
- **Back-office:** a Go API plus a plain JavaScript front-end (no build step, no framework),
  served over HTTPS by Caddy. Named admin accounts, HttpOnly cookie sessions, SQLite-backed.

## Getting started

Requires Docker and Go 1.26+.

```bash
make up          # build and start both databases, a proxy for each, and the API
                 # (first run creates deploy/.env with development secrets)
make routes      # check each proxy port reaches its own database
make logs        # follow proxy and API output
```

Then drive it:

```bash
# a client session through each proxy — the port picks the database. You log in
# as a real database user (alice on db-a, bob on db-b; passwords in deploy/.env),
# and the database checks the password, not the proxy.
make mysql-a   # shell on db-a as alice
make mysql-b   # shell on db-b as bob
make routes    # one query through each port, checking each reaches its own database

# the back-office UI and API, served by the edge (self-signed certificate: -k)
curl -sk https://localhost/healthz
make ui        # opens https://localhost
```

**Back-office login (local dev only):** `admin` / `admin123`. This is the development
credential from `deploy/.env.example` — it is public, so never use it anywhere but your own
machine. To replace it, generate a new bcrypt hash and set `API_ADMIN_PASS_HASH` in
`deploy/.env`:

```bash
htpasswd -nbBC 10 "" 'the-new-password' | cut -d: -f2
```

(`API_ADMIN_USER` sets the username; it defaults to `admin`. See
[`deploy/README.md`](deploy/README.md) for the full list of secrets.)

`make down` stops the stack; `make help` lists the rest.

> Each proxy posts its audit events to `POST /api/events`, so sessions appear in
> `/api/connections` tagged with the `upstream` they reached. Running a proxy
> without `SINK_INGEST_URL` logs the events to stdout instead.

### Layout

| Path | What |
|---|---|
| `cmd/proxy` | entry point for the MySQL auditing proxy (listens on 3307) |
| `internal/proxy/app`, `internal/proxy/config`, `internal/proxy/runtime` | proxy startup, configuration, and runtime implementation with colocated tests |
| `cmd/api` | the back-office API (listens on 8080) |
| `internal/audit` | the shared `Event` type and `Sink` interface — the contract both halves code against |
| `internal/sink` | implementations of `audit.Sink` |
| `internal/api`, `internal/store` | API handlers and the SQLite audit store |
| `internal/retention` | the optional audit-log retention job |
| `web` | the back-office UI (static files, served by the edge; see [`web/README.md`](web/README.md)) |
| `e2e` | end-to-end tests and the performance harness (need the compose stack) |
| `deploy` | Dockerfiles, compose stack, Caddy edge config |

For the proxy’s file-by-file layout, package dependencies, and test locations, see
the [proxy source guide](cmd/proxy/README.md#source-layout).

### Working on it

```bash
make test        # all tests; proxy integration tests need the db container, the UI helpers need node 22
make web-test    # just the UI helpers' tests
make csp-check   # fail if web/ has inline scripts or styles the CSP would block
make lint        # gofmt + go vet, same as CI
make perf        # performance baseline — measurements, not pass/fail
```

## Documentation

- **Project status:** [Sprint 3 — Outcomes and handover](docs/sprints/sprint-3.md) — what shipped, and what is left
- [Known Limitations](docs/known-limitations.md) — what it doesn't do, and the traps in the code
- [Sprint 2 — Plan](docs/sprints/sprint-2.md) · [Delegation](docs/sprints/sprint-2-delegation.md)
- [Sprint 1 — Plan](docs/sprints/sprint-1.md) · [Retrospective](docs/sprints/sprint-1-retro.md)
- Performance: [Sprint 3 re-run](docs/perf/sprint-3-rerun.md) (current) · [Sprint 2 baseline](docs/perf/sprint-2-baseline.md)
- [Data model and API contract](docs/design/data-model.md)
- [Deployment guide](deploy/README.md) · [Proxy guide](cmd/proxy/README.md) · [Back-office UI](web/README.md)
- [Architecture](docs/diagrams/architecture.md) — starting design diagram + how it fits together
- [Ways of Working](docs/ways-of-working.md) — the build breakdown + our Scrum process
- [Testing Strategy](docs/testing.md) — how we test + how to build for testability (TDD)
- [Contributing](CONTRIBUTING.md) — Git/GitHub workflow (branches, pull requests, CI)
- [Architecture Decisions](docs/design/decisions/) — ADRs recording key choices
- `docs/spec/` — the client specification + our distilled requirements
