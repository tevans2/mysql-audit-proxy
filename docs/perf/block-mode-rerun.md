# Performance re-run: block mode and per-user connections

Re-measures what changed since the [initial baseline](baseline.md): the audit sink
now **blocks** when its buffer is full ([ADR 0005](../design/decisions/0005-audit-sink-blocks-by-default.md)),
and every client session now logs in to the upstream **as that user** (pass-through
authentication, [ADR 0004](../design/decisions/0004-pass-through-authentication.md)).

Measured 15 Oct 2026.
Reproduce with `make perf`; `PERF_SINK_MODES=block,drop` selects the sink modes.

## The short version

- **Block mode keeps every audit record under overload and does not fail queries.** At 50
  connections offered 50,000 q/s, drop mode lost 72.9% of the audit records; block mode lost
  **none** (147,569 run, 147,569 audited), with zero failed queries.
- **The price is throughput, not errors.** Block mode tops out at about **18,500 statements/s**,
  the rate the API can ingest. Offered more than that, clients are slowed to it: 18,201 q/s
  achieved at 50,000 offered, against 48,863 in drop mode. Tail latency rises with it
  (p95 9.5 ms against 2.0 ms).
- **At the specified load there is no cost.** At 2,500 q/s block and drop are the same, and
  lossless. The headroom before blocking starts to matter is about 7× the specification.
- **A connection now costs about 2.2 ms more through the proxy**, up from about 0.3 ms in the
  initial baseline. This is the per-user upstream login. It is paid once per session.

## Block mode under overload

50 concurrent connections, `select1`, 5 s measured window, a fresh stack per mode.

| sink on full | offered q/s | achieved q/s | p50 | p95 | p99 | max | run | audited | loss |
|---|---|---|---|---|---|---|---|---|---|
| block | 2,500 | 2,510 | 2.40 ms | 3.94 ms | 5.33 ms | 8.5 ms | 20,050 | 20,050 | 0.00% |
| block | 12,500 | 12,510 | 1.19 ms | 2.23 ms | 3.50 ms | 202 ms | 100,050 | 100,050 | 0.00% |
| block | 50,000 | 18,201 | 1.46 ms | 9.54 ms | 15.2 ms | 207 ms | 147,569 | 147,569 | 0.00% |
| block | unpaced | 18,692 | 1.41 ms | 9.76 ms | 13.5 ms | 204 ms | 148,636 | 148,636 | 0.00% |
| drop | 2,500 | 2,510 | 2.14 ms | 4.33 ms | 7.37 ms | 12.8 ms | 20,050 | 20,050 | 0.00% |
| drop | 12,500 | 12,510 | 1.08 ms | 2.00 ms | 3.16 ms | 204 ms | 100,050 | 99,722 | 0.33% |
| drop | 50,000 | 48,863 | 0.77 ms | 1.98 ms | 4.05 ms | 202 ms | 384,609 | 104,355 | 72.87% |
| drop | unpaced | 46,958 | 0.76 ms | 1.87 ms | 3.56 ms | 206 ms | 372,648 | 107,141 | 71.25% |

The same load straight at the upstream, for reference:

| offered q/s | achieved q/s | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| 2,500 | 2,510 | 1.94 ms | 3.25 ms | 4.34 ms | 22 ms |
| 12,500 | 12,510 | 1.18 ms | 2.62 ms | 3.86 ms | 204 ms |
| 50,000 | 49,367 | 0.44 ms | 0.71 ms | 1.14 ms | 202 ms |
| unpaced | 78,889 | 0.31 ms | 0.81 ms | 1.78 ms | 204 ms |

Reading it:

- **Block mode is a throttle at the audit pipeline's ingest rate.** Once the offered rate passes
  ~18,500/s the buffer is always full, each query waits for room, and the clients are paced
  down to what the API can store. No query errors, no record lost.
- **Drop mode runs the queries at up to ~49,000/s but audits only about 105,000 of 385,000.**
  Block mode audited 147,569 in the same window, so it also stored more records per second
  (~18,500/s against ~13,000/s). Drop mode is probably slower to store because the proxy and
  the load generator compete with the API for CPU at that rate. I did not isolate that.
- **The first loss in drop mode is now at about 12,500 q/s (0.33%)**, against 7,500 (2.9%) in the
  initial baseline. The sink and API are not the same code or machine state, so treat the
  shift as "somewhat higher", not as a precise gain.
- **The ~200 ms maximum appears in every row, including direct to MySQL**, at the same
  magnitude. It is a client-side or loopback artifact (delayed-ACK scale), not the proxy and not
  block mode. Ignore the max column except for the 2,500 q/s rows, where it does not appear.
- Not measured: how long a client waits when the **API is down**. ADR 0005 notes the sink gives
  up on a batch after its retries; whether block mode stalls queries for that whole time in
  practice is untested.

## Per-user upstream connections

200 handshakes per path, first 10 discarded.

| path | p50 | p95 | p99 | max |
|---|---|---|---|---|
| direct | 270 µs | 565 µs | 647 µs | 673 µs |
| proxied | 2.46 ms | 5.50 ms | 6.04 ms | 6.14 ms |

A proxied connection now costs about **2.2 ms more at p50 and 5 ms at p95**. The initial
baseline was ~0.34 ms more (0.62 ms direct, 0.96 ms proxied). The direct path is faster this
time (270 µs against 624 µs) and the proxied one slower, so the gap has grown from
~0.3 ms to ~2.2 ms. The cause matches what pass-through does: the proxy terminates the client's TLS, then
opens a **new upstream connection and logs in as that user** for each session. The proxy log
shows `caching_sha2_password FAST auth (cache hit)` on each, so this is the cheap path; a cold
upstream cache would cost more.

What it means:

- Nobody notices 2 ms on a session that lives for minutes. A client that opens a connection
  per request pays it every time, and 2 ms is the floor.
- Each client session holds one upstream connection, so the proxy needs MySQL's
  `max_connections` (151 by default) to cover the total of concurrent clients across all users.
  Not measured here; at the specified 25–50 sessions it is not a concern.

## Proxy overhead and completeness (unchanged)

| workload | conns | p50 direct | p50 proxied | p95 direct | p95 proxied | p95 added |
|---|---|---|---|---|---|---|
| `select1` | 1 | 664 µs | 872 µs | 1.12 ms | 1.12 ms | 0.00 ms |
| `select1` | 25 | 1.28 ms | 2.16 ms | 2.03 ms | 3.23 ms | +1.20 ms |
| `select1` | 50 | 1.98 ms | 2.17 ms | 3.40 ms | 3.64 ms | +0.24 ms |
| `scan100` | 1 | 804 µs | 2.07 ms | 1.04 ms | 2.46 ms | +1.42 ms |
| `scan100` | 25 | 1.54 ms | 4.76 ms | 2.81 ms | 7.35 ms | +4.54 ms |
| `scan100` | 50 | 2.78 ms | 5.74 ms | 5.18 ms | 9.72 ms | +4.54 ms |

Audit completeness at the specified load: 651, 16,275 and 32,550 statements run, and the
same number audited, at 1, 25 and 50 connections. This agrees with the initial baseline.

## Caveats

- **One run per row.** The initial baseline took the median of three. A single run cannot
  separate small differences from noise; the large ones above (block versus drop loss and
  throughput, the connection cost) are well outside it.
- **A noisy machine.** The load average was about 3.1 at the start, with other applications open.
  The baseline asks for a quiet one. The absolute latencies are therefore pessimistic.
- **Same machine and setup as the baseline**: Apple M4, 10 cores, MySQL in Docker Desktop, proxy
  and API native, SQLite audit store. The ~18,500/s ingest ceiling is mostly SQLite plus a
  single API process; Postgres would move it.
- **Short windows** (5 s for the ceiling sweep, 10 s elsewhere), no writes in the load loop,
  100-row result sets at most, as before.
- The 50,000 q/s row's `achieved` figure counts completed queries in the measured window only.
