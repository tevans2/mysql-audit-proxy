# ADR 0001: Proxy language — Go

## Status

Accepted — 2026-07-28. Locked; not contingent on any further investigation.

## Context

The proxy core is the hard part of this project: a long-lived network service that must
accept MySQL client connections, complete the protocol handshake, relay traffic in both
directions without corrupting the stream, handle many concurrent connections per port, and
parse queries out of the MySQL wire protocol.

The client left the stack open, and stated that the choice and the reasoning behind it are
part of what is assessed. No team member currently knows Go (or has a strong alternative
preference), so both the technical fit *and* the learning curve matter.

## Decision

We will build the proxy in **Go**, using the `go-mysql-org/go-mysql` library for the MySQL
wire protocol rather than implementing it from scratch.

## Rationale

- **The library fits the exact use case.** `go-mysql` is a pure-Go MySQL toolset whose
  server package is explicitly intended for building a MySQL proxy, and is compatible with
  MySQL 5.5–8.0, so standard clients connect unmodified.
- **This is a proven pattern in Go, not a science experiment.** Production Go MySQL proxies
  exist — e.g. **kingshard** (~80% of direct-connect performance, prepared-statement support,
  dynamic backend online/offline ≈ our hot-reload requirement) and **Vitess** (YouTube-scale,
  mostly Go, production MySQL parser).
- **Go's concurrency model matches the requirement.** Goroutine-per-connection costs only a
  few KB each and comfortably handles thousands of concurrent connections — a direct fit for
  "multiple concurrent client connections per port."
- **Simple deployment.** Compiles to a single static binary → trivial to containerise.
- **Client-endorsed**, which makes the reasoning easy to defend.

## Consequences

**Positive:** best technical fit for a concurrent network proxy; strong library support; clean
deployment; a defensible, well-evidenced choice.

**Negative / risks:** nobody on the team knows Go yet → a learning curve during Sprint 1.

**Mitigations:**
- Joe's first proxy task is getting `go-mysql` to complete a handshake and read query text —
  this also serves as the team's hands-on Go onboarding.
- Mieke pairs on the proxy so the crux is not a single-person silo and two people climb the
  curve together.
- Manage goroutine lifecycles with `context`-based cancellation to avoid leaks (a known,
  documented pattern).

## Alternatives considered

- **Rust** — best performance/safety, but the steepest learning curve; too risky with no
  prior experience on the team.
- **Java (Netty + Spring)** — team-adjacent and client-adjacent, but no dominant MySQL-proxy
  library means implementing more of the protocol ourselves; heavier deployment.
- **Node/TypeScript** — familiar, but the weakest concurrency + byte-handling model for a
  line-rate proxy.
- **Python** — easiest learning curve and `mysql-mimic` exists, but the weakest concurrency
  story for a line-rate proxy; considered and not chosen.
