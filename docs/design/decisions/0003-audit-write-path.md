# ADR 0003: The proxy posts audit events to the API, not to the store

## Status

Accepted — 2026-08-01, implemented in the first version.

## Context

Audit events originate in the proxy and end up in the audit store. Two options:

1. The proxy writes to the audit database directly.
2. The proxy sends events to the back-office API over HTTP, and only the API touches the store.

The client's specification says the API stores the captured queries and exposes them to the
back-office, which points at (2), but the decision was worth making explicitly because it costs
a network hop on a path we care about.

## Decision

The proxy's audit sink **POSTs batches to `POST /api/events`**. The back-office API is the only
component that touches the audit store.

## Consequences

**Positive**

- **One owner of the schema.** The proxy has no knowledge of tables, and no database driver for
  the audit store at all. Schema changes are the API's business alone.
- **The services are genuinely decoupled.** The audit backend can be restarted, redeployed or
  fail without the proxy caring — which is what makes the fail-safe behaviour a structural
  property rather than error handling bolted on.
- **Swapping the store is contained.** SQLite to Postgres is a change behind the `Store`
  interface in one service.
- Two writers to one database would have needed coordination we now don't need.

**Negative**

- A network hop and a serialisation step on the audit path. Acceptable because that path is
  already asynchronous and off the query path — see below.
- The API becomes a dependency of complete auditing. If it's down, records can be lost (see
  [known limitations](../../known-limitations.md)).

**What this required**

Auditing must never block or break a query. So `Emit` is a non-blocking send onto a buffered
channel, and a separate worker batches and posts. The proxy never waits on HTTP.

An important corollary surfaced in review: because the sink decides whether to discard its buffer
based on the response status, **the ingest endpoint must report failure honestly.** A `202` that
can mean "lost it" makes the sink's retry logic meaningless.

## Alternatives considered

- **Proxy writes directly to the store.** Simpler and one hop faster, but couples the proxy to
  the schema, creates two writers, and makes swapping the store a two-service change. Rejected.
- **A message queue between them.** Better durability, but another piece of infrastructure to run
  for a system whose expected load is 25–50 concurrent connections. Rejected as
  over-engineering for now; durable buffering is a later concern.
