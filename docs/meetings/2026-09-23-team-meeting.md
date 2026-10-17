# Team Meeting — 2026-09-23

**Type:** Internal team meeting (pre-demo) · **Sprint:** 2 (closes Fri 25 Sep)

**Present:** Tate, Joe, Maria, Mieke, Keagan

## Purpose

Sprint 2's demo is **tomorrow**. Leave this meeting with two things:

1. A **frozen `main`** — everyone agreed on what lands today and what waits.
2. A **written demo runsheet** — who shows what, in what order, on whose machine.

Sprint 1's demo went to plan because it had a runsheet. Sprint 2 doesn't have one yet. That
is the main output of this meeting; everything else on the agenda exists to make writing it
possible.

This is not a status round. Everyone can read `git log`.

## Before the meeting

- Know the state of your own branch: pushed, reviewed, and whether it merges clean on `main`.
- Skim [Performance Baseline](../perf/sprint-2-baseline.md) — it will come up at the demo.
- Skim [Known Limitations](../known-limitations.md) and note anything about *your* area that
  is now out of date.

---

## Agenda (~75 min)

### 1. Confirm the demo — 5 min

There is a **discrepancy in our own docs** and it has to be settled first, because everything
below depends on it:

- `docs/sprints/sprint-2-delegation.md` says **Demo: Thu 24 Sep**
- `docs/sprints/sprint-2.md` says **"to be scheduled with Acumen"**

Settle: is it confirmed, at what time, who attends from Acumen, and is Sphelelo joining.
Whoever has the thread with Cameron says so out loud. Then fix whichever doc is wrong.

### 2. Merge freeze — 20 min

Four branches are unmerged. Decide each one, now, in the room.

| Branch | Owner | State | Proposed |
|---|---|---|---|
| `31-configuration-management-screens` | Maria | review addressed 22 Sep, 2 commits | **Merge today** — demo item |
| `35-performance-baseline` | Tate | 7 commits | **Merge today** — answers the client's obvious question |
| `40-...listener-reconciliation-2` | Mieke | 1 commit, up to date with main | **Merge today**, with the fix below |
| `36-ui-show-disconnect-time` | Maria | 1 commit, stale since 18 Sep | **Merge or close** — don't leave it hanging |

**On #40 specifically.** The *issue* is large and is Sprint 3 scope. The *branch* is only the
"stoppable listeners" part: `Run` takes a `ctx`, the call site passes `context.Background()`,
so nothing cancels it and runtime behaviour is unchanged. It also fixes the accept-loop spin
we have recorded in known-limitations. Merging the inert primitive now is lower risk than
letting the reconciliation manager stack on an unmerged base — that is exactly the branch
divergence the Sprint 1 retro called our biggest cost.

One review point to fix before it goes in. With `context.Background()`, `ctx.Err()` is `nil`,
so a listener closed unexpectedly makes `Run` return nil, `runAll` return nil, and `main`
skip `log.Fatal` — **the process exits 0 silently** while other proxies are still listening.
Until something actually cancels that context, that case is fatal, not clean:

```go
if errors.Is(err, net.ErrClosed) {
    if ctxErr := ctx.Err(); ctxErr != nil {
        return ctxErr // intentional stop
    }
    return fmt.Errorf("listener closed unexpectedly: %w", err)
}
```

**The line to agree:** the `Run` refactor merges today. The reconciliation manager, the
polling client, draining and the E2E matrix are **Sprint 3** and stay on a branch until
Monday.

### 3. What we claim vs. what is true — 15 min

Three places where we could mislead the client by accident.

**a. Live config reload — we ship a seam, so name it.** The proxy fetches config **once** at
startup (the startup fetch, now in `internal/proxy/config/config.go`) and never re-reads it. So an admin adds a server in the
back-office, it saves, it appears in the list — and **nothing routes to it** until the proxy
restarts. The UI says nothing about this.

This is not a failure: `sprint-2.md` deferred runtime reconfiguration to Sprint 3, and we
promised config that is *persisted, served and editable*, which is delivered. But unexplained
it reads as a bug. Two small things fix it — a line on the config screen saying changes take
effect on proxy restart, and making the restart a deliberate step in the demo.

**b. TLS.** We terminate TLS on the client leg with a self-signed, in-memory certificate,
because `caching_sha2_password` requires a secure channel. The **proxy→database leg is still
plaintext**, and that is the direction Acumen asked to be *always* encrypted. Agree the exact
wording now so nobody says "TLS is done" tomorrow.

**c. `known-limitations.md` is stale.** It is marked "current as of end of Sprint 1" and still
claims failed logins aren't recorded, prepared statements are unsupported, there is no
back-office auth, and only `mysql_native_password` is offered. **All four shipped this
sprint.** It is the doc we answer client questions from, and right now it understates our own
work. Needs updating today, plus the new restart limitation from (a).

### 4. Write the runsheet — 20 min

Do this in the room, not afterwards. Fill in:

| # | What | Who | Notes |
|---|------|-----|-------|
| 1 | Stack up, proxies routing to two databases | | |
| 2 | Client session through each port → rows come back | | |
| 3 | Live-updating viewer (the one thing they asked for at Sprint 1) | | |
| 4 | Failed login → `auth_fail` appears | | |
| 5 | Prepared statements / real client tooling connecting | | |
| 6 | Admin login + config screens → **restart** → routing changed | | |
| 7 | Performance baseline | | |

Also agree: **whose machine**, what the fallback is if a container dies mid-demo, and who
speaks when someone else is driving.

**On the perf numbers** — whoever presents them should be able to say them without the doc:
~1–4 ms added per query at 25–50 connections; **zero** audit loss at the specified load
(2,505 stmt/s); first loss at ~7,500/s, so roughly 2× headroom; ~340 µs added per connection.
And the caveat that keeps it honest: measured on one laptop against Docker, so the
*difference* between direct and proxied transfers — the absolute milliseconds do not.

### 5. Questions to get answered at the demo — 10 min

**Sprint 3 is our last sprint.** Anything Acumen doesn't decide tomorrow either doesn't get
built or gets built on a guess. Assign an owner to each — an unowned question doesn't get
asked.

| Question | Why it can't wait | Owner |
|---|---|---|
| Per-user **authorisation**, or is the audit trail sufficient? | Pending since 20 Aug. If required, Sprint 3 changes shape entirely | |
| Audit **drop vs. block** — above ~7,500 stmt/s we protect queries and drop records. Right trade? | ADR 0003 reversal if they say block. They've never seen the choice with a number on it | |
| **SQLite or Postgres** | "Decision pending" since Sprint 1 and there is **no ADR**. Migrating in the final sprint is expensive | |
| **Raw SQL with literal values** (POPIA) | Changes the schema; cannot be retrofitted in week three of three | |
| **Retention** — how long, and delete or archive at expiry | Policy is their input, not our engineering choice | |
| Is **25–50 concurrent connections** still the real figure? | The whole baseline rests on it | |
| What **certificates** would they use for proxy→DB TLS? | Cert management is environment-specific; we cannot guess it | |
| Of what remains, what are their **top three**? | We will not get all of it. Better they choose | |

### 6. Admin — 5 min

- **Book the retro** (sprint closes Friday) and Sprint 3 planning.
- Decide on the record: does a **metrics endpoint** get squeezed in, or move to Sprint 3?
  `sink.Stats()` exists and nothing calls it. It is the one Sprint 2 infra commitment nobody
  has touched, the sprint plan justified it with "silent data loss is the worst failure mode
  for an audit tool," and the perf baseline independently ranked it as Sprint 3's #1 item.

---

## Decisions to leave with

1. Demo date, time and attendees — confirmed, docs corrected.
2. Each of the four branches: merged today, or held.
3. The #40 line: `Run` refactor in, reconciliation manager out.
4. Agreed wording for TLS and for live config reload.
5. Runsheet written, with owners.
6. An owner against every client question.
7. Metrics endpoint: in or deferred, on the record.
8. Retro and Sprint 3 planning in the calendar.

## Action items

| # | Action | Owner | Due | Status |
|---|--------|-------|-----|--------|
| 1 | Confirm demo time/attendees; fix the date in `sprint-2.md` | | Today | Open |
| 2 | Fix the `net.ErrClosed` nil-return, then merge #40 | Mieke | Today | Open |
| 3 | Merge #31 config screens | Maria | Today | Open |
| 4 | Add "changes take effect on proxy restart" to the config screen | Maria | Today | Open |
| 5 | Merge #35 performance baseline | Tate | Today | Open |
| 6 | Merge or close #36 | Maria | Today | Open |
| 7 | Update `known-limitations.md` for everything Sprint 2 shipped + the restart seam | | Today | Open |
| 8 | Write the demo runsheet into `docs/meetings/2026-09-24-sprint-2-demo.md` | Tate | Today | Open |
| 9 | Full `make up` + `make test` dry run on the demo machine | | Tonight | Open |
| 10 | Book retro + Sprint 3 planning | Tate | Today | Open |

## Next

- **Thu 24 Sep** — Sprint 2 demo to Acumen.
- **Fri 25 Sep** — Sprint 2 closes; retrospective.

## For the retro, not today

Sprint 2 pulled in three things the plan had deferred to Sprint 3 — client-leg TLS, the
revisioned config API, and the listener refactor — while the one Sprint 2 infra commitment
nobody touched is health checks and metrics. Building forward into the next sprint while a
current commitment sits undone is a pattern worth looking at on Friday, not defending
tomorrow.
