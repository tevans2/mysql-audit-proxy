# Sprint 2 — Plan

**Window:** 24 Aug – 25 Sep 2026 · **Weight:** 15% · **Demo:** to be scheduled with Acumen

Sprint 1 delivered a working end-to-end slice against one database. Sprint 2 is about the
remaining functionality in Acumen's specification, and making the system usable with the tools
their team actually uses.

## Goal

A proxy fronting **several** databases, connectable from **real client tooling**, with
configuration and access control managed through the back-office.

## Scope

### Proxy — Joe, Mieke
- **Multiple upstream servers, routed by listening port.** The architecture in the spec; Sprint 1
  ran one database to prove the pipeline. Needs a listener per configured port, each with its own
  upstream and credentials.
- **Prepared statements.** Currently unsupported, which is why tools like DBeaver and JDBC
  applications can't connect. Note the auditing problem: `COM_STMT_EXECUTE` carries a statement
  ID and binary parameters, not SQL, so the prepared SQL must be remembered per session and
  mapped back — otherwise the audit trail records "executed statement 3".
- **MySQL 8 default authentication** (`caching_sha2_password`). We only offer
  `mysql_native_password` today, so a modern client can't connect without an override.
- **Failed-login auditing.** Rejections currently emit no event. The `auth_fail` kind already
  exists in the contract and the API already accepts it — only the proxy's emit is missing.
  Needs a custom auth handler for the `OnAuthFailure` hook.

### Store + API — Keagan
- **Persist and serve the server configuration**, so port-to-database mappings live in the
  database rather than a file we ship. Includes storing upstream credentials safely.
- **Endpoints for editing that configuration.**
- **Authentication on the API.** A login screen alone isn't access control — the endpoints
  must require a session or token, or the audit log is readable by anyone who can reach it.

### Back-office UI — Maria
- **Live-updating viewer** — *requested directly by the client at the Sprint 1 demo.* Polling on
  an interval is the cheap version; a WebSocket pushed from the API is closer to what they
  described.
- **Configuration management screens.**
- **Admin login.**
- **Surfacing failed queries** clearly rather than as an ordinary row.

### Infrastructure — Tate
- **End-to-end tests in CI**, so the whole path is verified automatically rather than by hand.
- **Performance baseline** against the 25–50 concurrent connections Acumen specified. Currently
  unmeasured, and it's the obvious client question.
- **Health checks and metrics**, in particular audit queue depth and drop count — silent data
  loss is the worst failure mode for an audit tool.
- **Configuration and secrets handling** for the services themselves.
- **Multi-upstream test environment** — a compose stack with several MySQL instances, which the
  proxy work depends on.

## Deferred to Sprint 3

TLS on both connection legs · runtime reconfiguration without dropping live sessions ·
per-user identity through to the database · audit-log retention · tamper-evident audit records ·
server-side filtering in the viewer.

## Open question for the client

**Does per-user *authorisation* at the database matter, or is the audit trail sufficient?**
Raised with Michael at the Sprint 1 demo. Today every session reaches MySQL as one service
account, so identity is recorded but not enforced and database-side per-user grants don't apply.
If authorisation is required for their ISO 27001 position, that work moves earlier and is
substantially larger. Answer pending.

## Process changes carried from the retro

Pull main in daily; never rebase a shared branch; develop in the repo; every fix ships with a
regression test verified failing first; everyone pushes their own work. See
[Sprint 1 retrospective](sprint-1-retro.md).
