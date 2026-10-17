# Sprint 1 — Retrospective

**Sprint:** 27 Jul – 21 Aug 2026 · **Demo:** 20 Aug, held and well received

## Did we deliver the sprint goal?

Yes. The goal was a working end-to-end slice: a proxy relaying a client to MySQL, capturing
connections and queries, shipping them through an API into a store, and showing them in a
viewer — running via `docker-compose`. All of that works, and it was demonstrated live.

The client's CTO was the main participant and was happy with both the state and the direction.
The only change requested was making the log viewer update live rather than needing a reload.

## What worked

**Contract first.** Agreeing the data model and API before building anything was the single
most valuable decision of the sprint. Each of the four components was written against that
contract with stand-ins for the others, which is why five people could work in parallel and
have the pieces fit when they met. When a field was later added (`upstream_addr`), the change
was mechanical because there was one authoritative definition to change.

**Reviewing before merging caught real defects.** Not style issues — writes silently reporting
zero affected rows, truncated result sets appearing successful, transactions that reported
success while rolling back nothing, audit records dropped with no signal, and stored XSS in
the viewer. Several of these would have surfaced in front of the client rather than in a merge
request.

**Proving defects before fixing them.** Writing a failing test first, then fixing, then
confirming the test fails against the original commit. This changed review conversations —
"upstream actually holds 1 row" ended an argument that prose hadn't.

**Testing against the real thing.** The proxy's integration tests run against a real MySQL 8.0
rather than a mock, because the wire protocol can't be meaningfully faked. Everything runs
under Go's race detector.

## What didn't

**Branch divergence — the biggest cost of the sprint.** Three branches drifted far enough from
main to make merging hazardous, and two of them broke the build:

| Branch | Cause |
|--------|-------|
| Ingest API | Rebased locally while the original was still being pushed to; merge base fell back 12 commits |
| Audit sink | Developed in a separate scratch module with a temporary copy of the shared contract, which came along in the merge |
| Query capture | Sat 22 commits behind main, built on the pre-restructure layout |

This cost more time than several of the features did.

**A review fix introduced a new bug.** The drop-logging fix put its state in a package-level
global, which silently disabled the logging for every sink but the noisiest — the exact failure
the change was meant to fix, relocated. Caught in review, but it shows a fix without a test
proves nothing.

**Some tests didn't test what they claimed.** `TestProxyFailingQuery` failed on "no database
selected" rather than the missing table it was named for, and passed anyway because the
assertion only checked that *some* error occurred.

**Contributions were uneven early on.** Work landed under one person's name on occasion rather
than the author's, and the commit distribution was lopsided for the first half of the sprint.

## Actions for Sprint 2

1. **Pull main into your branch daily.** Never rebase a branch someone else has pushed to.
2. **Develop in the repo**, not in a scratch module alongside it.
3. **Every bug fix ships with a regression test**, verified failing against the unfixed code.
4. **Name the case a test actually covers** — if it's called `FailingQuery`, make it fail for
   the reason in the name.
5. **Everyone pushes their own work**, in several commits rather than one, under their own name.

## Carried into Sprint 2

See [Sprint 2 plan](sprint-2.md). The client-requested live-updating viewer is now on it, and
the known gaps are tracked in [known limitations](../known-limitations.md).
