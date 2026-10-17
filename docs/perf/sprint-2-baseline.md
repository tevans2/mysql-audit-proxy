# Performance Baseline — Sprint 2

Acumen specified **25–50 concurrent connections**. Until now that was unmeasured, which
made "how many people can use this?" a question we could not answer at all. This is the
answer, the method behind it, and what it does not cover.

Measured 23 Sep 2026 against `main` at [`cd0959e`](../../). Reproduce with `make perf`.

> **Superseded in part (project close, 16 Oct 2026).** The overhead and connection-cost figures
> below describe Sprint 2's proxy. Since then the audit sink blocks by default
> ([ADR 0005](../design/decisions/0005-audit-sink-blocks-by-default.md)) and every session logs in
> to the upstream as its own user ([ADR 0004](../design/decisions/0004-pass-through-authentication.md)),
> so use the [Sprint 3 re-run](sprint-3-rerun.md) for current numbers. This document also
> attributes the audit ceiling "substantially" to SQLite. Measuring the store directly showed
> writes are not the limit; see [ADR 0007](../design/decisions/0007-sqlite-remains-the-audit-store.md).

## The short version

- **The proxy costs about 1–4 ms per query at 25–50 connections.** At the modelled load it
  roughly doubles a sub-millisecond query and adds a low single-digit number of
  milliseconds at p95. Nothing a person notices; something a tight application loop would.
- **The audit trail is complete at the specified load.** 50 concurrent connections issuing
  2,505 statements/second lost **zero** audit records across three runs.
- **The audit trail starts losing records at roughly 7,500 statements/second**, and at
  saturation (~20,000/s) loses around 70%. That is the sink's documented trade — it drops
  rather than blocking the query path — and it is the number to quote, because it is where
  the product's guarantee changes.
- **A connection costs ~300 µs more through the proxy.** The TLS handshake and
  `caching_sha2_password` added in Sprint 2 are not expensive.

## What was measured

Two workloads, both against `db-a`:

| Workload | Statement | Why |
|---|---|---|
| `select1` | `SELECT 1 /* … */` | No rows, no table — as close to pure proxy overhead as a real statement gets |
| `scan100` | `SELECT id, name FROM sales.perf_rows /* … */`, 100 rows | Exercises the result path, which the proxy buffers whole in memory rather than streaming ([known limitations](../known-limitations.md)) |

Each connection issues 50 queries/second — brisk for a person, ordinary for an
application. Unpaced, 50 connections against a sub-millisecond query produce hundreds of
thousands of audit events in seconds, which measures how fast SQLite can be filled rather
than what the proxy costs. `PERF_RATE=0` removes the pacing deliberately.

Every run drives the identical workload **direct to MySQL first, then through the proxy**,
in one process, so the two share machine conditions. The difference between them is the
number that answers the client's question; neither figure means much alone.

## Results

### Proxy overhead

Median of three runs. p95 added is proxied minus direct.

| Workload | Conns | p50 direct | p50 proxied | p95 direct | p95 proxied | p95 added |
|---|---|---|---|---|---|---|
| `select1` | 1 | 1.09 ms | 1.13 ms | 1.58 ms | 1.71 ms | +0.13 ms |
| `select1` | 25 | 1.53 ms | 2.33 ms | 2.52 ms | 4.20 ms | +1.68 ms |
| `select1` | 50 | 2.94 ms | 2.71 ms | 4.83 ms | 5.20 ms | +0.37 ms |
| `scan100` | 1 | 1.30 ms | 2.16 ms | 1.63 ms | 3.05 ms | +1.42 ms |
| `scan100` | 25 | 2.50 ms | 5.19 ms | 4.00 ms | 7.17 ms | +3.17 ms |
| `scan100` | 50 | 3.79 ms | 7.65 ms | 6.29 ms | 11.86 ms | +5.57 ms |

Reading it honestly: the `select1` row at 50 connections shows the proxy *faster* than
direct at p50. That is noise, not a result — the two paths are within a few hundred
microseconds of each other there, and run-to-run variance on this machine is of the same
order. The reliable signal is `scan100`, where the cost grows with both connection count
and result size, which is what the in-memory result buffering predicts.

Throughput matched the offered rate on both paths at every connection count, so none of
these measurements is of a saturated system.

### Audit completeness at the specified load

| Conns | Statements run | In audit log | Lost |
|---|---|---|---|
| 1 | 651 | 651 | 0 (0.00%) |
| 25 | 16,275 | 16,275 | 0 (0.00%) |
| 50 | 32,550 | 32,550 | 0 (0.00%) |

Zero loss at the specification, reproduced across three runs.

This is only meaningful because the same test **is able to detect loss** — see the ceiling
below, where it reports 70%. A completeness test that has never seen a failure is not
evidence of anything.

### Where the audit trail gives out

At 50 concurrent connections, varying the offered rate:

| Offered q/s | Achieved q/s | Statements run | In audit log | Loss |
|---|---|---|---|---|
| 2,500 | 2,510 | 20,050 | 20,050 | 0.00% |
| 5,000 | 5,010 | 40,050 | 40,050 | 0.00% |
| 7,500 | 7,510 | 60,050 | 58,324 | 2.87% |
| 10,000 | 10,010 | 80,050 | 78,138 | 2.39% |
| 12,500 | 12,514 | 99,957 | 66,227 | 33.74% |
| 50,000 | 22,218 | 179,204 | 52,246 | 70.85% |
| unpaced | 20,061 | 164,746 | 46,054 | 72.05% |

Lossless to **5,000 statements/second**; first loss at 7,500. The specified load produces
2,505/s, so there is roughly **2× headroom** before the audit guarantee changes.

The loss is the audit sink dropping events, exactly as designed: its buffer holds 1,024
events and `Emit` drops rather than blocks when it fills, so the query path is never
stalled by auditing. The proxy logs every drop, and those lines corroborate these numbers:

```
sink dropped 10089 events since the last flush
sink dropped 19161 events since the last flush
```

This is the trade [ADR 0003](../design/decisions/0003-audit-write-path.md) records, now
with a number attached to it.

### Connection cost

200 handshakes per path, first 10 discarded.

| Path | p50 | p95 | p99 | max |
|---|---|---|---|---|
| direct | 624 µs | 841 µs | 1.05 ms | 1.84 ms |
| proxied | 963 µs | 1.64 ms | 2.65 ms | 3.39 ms |

Measured separately because Sprint 2 made it more expensive: the proxy now terminates TLS
and offers `caching_sha2_password` where Sprint 1 offered `mysql_native_password` in the
clear. The answer is that it is cheap — about 340 µs — and steady-state query latency
hides it entirely, since it is paid once per session. It matters only to something that
connects per request.

## How to reproduce

```bash
make perf        # starts the upstreams, runs everything, prints the tables
```

Knobs, all optional:

| Variable | Default | What |
|---|---|---|
| `PERF` | unset | Must be set, or every perf test skips |
| `PERF_CONNS` | `1,25,50` | Connection counts to measure at |
| `PERF_RATE` | `50` | Queries per second per connection; `0` = unpaced |
| `PERF_WARMUP` | `3s` | Discarded before measuring |
| `PERF_DURATION` | `10s` | Measured window |
| `PERF_CEILING_RATES` | `50,250,1000,0` | Per-connection rates for the ceiling sweep |
| `PERF_CEILING_CONNS` | `50` | Connection count for the ceiling sweep |

The tests live in `e2e/perf_test.go` and reuse the end-to-end harness, so they drive the
real `proxy` and `api` binaries rather than wiring packages together in-process.

They are **measurements, not assertions**, and are gated on `PERF=1` so they never run in
`make test` or CI. A threshold baked in here would either fail on a loaded laptop or pass
straight through a real regression on a fast one. The one thing they do fail on is an
audit log that receives *nothing*, which is a broken pipeline rather than saturation.

## Caveats — read before quoting any of this

- **The absolute latencies are not Acumen's.** Upstream MySQL runs in Docker Desktop's VM
  with virtualised networking, and the proxy runs natively on the host. Their production
  would have neither arrangement. The **difference** between the direct and proxied paths
  is the transferable number; the raw milliseconds are not.
- **One machine, one shape.** Apple M4, 10 cores, 16 GB, Docker Desktop 29.4.0 limited to
  4 CPUs and 3.9 GB, Go 1.26.4, macOS 15. Nothing here has been run on Linux or on server
  hardware.
- **SQLite is the audit store.** The ceiling above is substantially a property of SQLite
  plus a single-process API, not of the proxy. Postgres is the pending decision, and it
  would move that number.
- **Short windows.** Ten seconds of measurement after three of warm-up. Long enough for a
  stable p99, not long enough to show anything that emerges over hours — memory growth,
  connection leaks, or the audit database getting large.
- **No writes in the load loop.** Writes would grow the table mid-run and change what was
  being measured. The write path's cost is unmeasured.
- **Result-set sizes are small.** 100 rows. The in-memory buffering limitation will matter
  far more at ten thousand, and that is not measured here.

## What this says to do next

Nothing urgent. The system meets the specification with headroom, and the one number that
could be called a weakness — audit loss above ~7,500 statements/second — is a documented
design trade rather than a defect, and sits well above anything the specified load
produces.

Worth filing for Sprint 3, not fixing now:

1. **Expose the sink's drop counter.** `Stats()` exists and nothing calls it, so the only
   evidence of loss today is a log line. An audit tool should be able to prove it did not
   lose anything, without someone reading stdout.
2. **Re-measure against Postgres** when that decision lands. The ceiling is mostly the
   store's.
3. **Measure a large result set.** `ExecuteSelectStreaming` is the known fix for whole-set
   buffering; `scan100` hints at the cost but does not size it.
