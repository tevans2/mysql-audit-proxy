# ADR 0005: The audit sink blocks on buffer-full by default, superseding part of ADR 0003

## Status

Accepted - 2026-09-24, client direction at a demo. Supersedes the
"audit must never block or break a query" guarantee from ADR 0003.

## Context

ADR 0003 established that the proxy's audit sink is strictly non-blocking:
`Emit` sends onto a buffered channel and a separate worker posts batches to the
API, so a slow or unreachable audit backend can never add latency to a query.
That was treated as a structural property, not an implementation detail.

The initial performance baseline (see
[docs/perf/baseline.md](../../perf/baseline.md)) found the
limit of that design: above roughly 7,500 statements/second, the buffered
channel fills faster than the worker can drain it. At that point the sink has
exactly two choices, drop new audit records to keep letting queries through,
or block the query path to keep every record.

Dropping was the implicit behaviour ADR 0003 assumed, but the team flagged
internally that this is a real trade nobody had put a number
on for the client, and that choosing to block would directly reverse ADR
0003's guarantee. The question was put to the client at a demo on 24 Sept. Their answer: block.

## Decision

When the audit sink's buffer is full, the proxy now **blocks** on emitting a
new audit event rather than dropping it. Audit completeness takes priority
over query latency under sustained overload (per the initial baseline).

This explicitly reverses the "auditing must never block or break a query"
property from ADR 0003's Decision and Consequences sections. ADR 0003's
broader decision , the proxy posts to the API, not to the store directly,
is unaffected and still stands.

Blocking is the default, not the only option. `SINK_ON_FULL=drop` restores
the previous drop-on-full behaviour for an operator who would rather protect
query latency; unset, or `SINK_ON_FULL=block`, gives the behaviour above.
Anything else is a fatal startup error rather than a silent default, since
this knob decides whether a full buffer stalls queries or discards audit
records. See `cmd/proxy/README.md`.

## Consequences

**Positive**

- No silent loss of audit records *to buffer overflow*. For an auditing tool,
  this was named directly in the 23 Sept meeting as the worst failure mode to
  leave unaddressed.

**Negative**

- Query latency is no longer structurally decoupled from audit sink
  capacity. Above the ~7,500 stmt/s threshold, clients can now experience
  increased latency or stalls while the proxy waits for buffer space,
  something ADR 0003 specifically designed against.
- **Blocking does not make auditing lossless.** It closes the buffer-overflow
  path and nothing else. A batch whose POSTs keep failing is still given up on
  after `MaxRetries` attempts with backoff (roughly 15s with the current
  defaults, counted as `Stats.Failed`), so a sustained API outage loses
  records regardless of this decision. Durable buffering is what would close
  that gap, and ADR 0003 rejected a message queue as over-engineering for the
  expected scale, worth revisiting if completeness during an outage becomes a
  requirement rather than a preference.
- An event blocked in `Emit` when `Close()` fires is dropped rather than
  delivered: the send selects on shutdown as well as on buffer space, since a
  plain send would deadlock once the worker stops draining. Shutdown drains
  what is already buffered, not what is still waiting to enter.
- Makes visibility into sink pressure more important than it was under
  drop-and-continue, since a filling buffer now has a user-facing symptom.
  `sink.Stats()` existed but nothing called it when this was written, an
  open commitment the perf baseline ranked first. This ADR
  doesn't implement that monitoring; it only changes the buffer-full
  behaviour. *Resolved later (#49): the proxy now serves
  `auditproxy_audit_queue_depth`, `auditproxy_audit_dropped_total` and the other sink
  counters on its admin port; see `cmd/proxy/README.md`. Nothing displays or
  alerts on them.*

## Alternatives considered

- **Keep dropping on buffer-full** (the ADR 0003 status quo). Protects query
  latency unconditionally, but risks silent, unbounded audit gaps under
  load with no record that anything was lost. Rejected as the default by
  client direction, but kept available as `SINK_ON_FULL=drop`.
- **Bounded wait, then drop** (block up to some timeout, then fall back to
  dropping). Could cap worst-case latency impact while still favouring
  completeness. Not discussed with the client as a distinct option, worth
  raising if the plain-block behaviour proves too costly in
  practice, but not adopted now since it wasn't what was decided.
- **A durable queue between proxy and API**, giving more buffer headroom
  before either blocking or dropping is forced. Already rejected in ADR
  0003 as infrastructure the project's expected scale doesn't justify; that
  reasoning still holds here.