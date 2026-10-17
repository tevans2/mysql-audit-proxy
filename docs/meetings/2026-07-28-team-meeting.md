# Team Meeting — 2026-07-28

**Type:** Internal team meeting (first) · **Sprint:** 1

**Present:** Tate, Joe, Maria, Mieke
**Apologies:** Keagan (absent)

## Purpose

First team get-together: meet, agree roles, settle the tech direction, and set up how we'll
work before the client kickoff.

## Decisions

- **Tech stack — Go** for the proxy (using the `go-mysql` library). **Locked in** — the choice
  is confirmed and not contingent on further investigation. See ADR
  `docs/design/decisions/0001-proxy-language.md`.
- **Roles / workstreams** agreed (provisional — to be firmed up with Keagan):
  | Member | Workstream |
  |--------|-----------|
  | Joe | Proxy core (lead) + audit pipeline |
  | Mieke | Proxy core / pipeline (pairs with Joe) |
  | Maria | Back-office UI |
  | Tate | Infra / QA + Scrum coordination; scaffolds store + API |
  | Keagan | TBD — backend/store + API, or proxy |
- **Ways of working** adopted: Scrum (3 sprints, demo each), GitLab issues + board, and the
  Git/MR workflow in `CONTRIBUTING.md`. Everyone to read both docs.
- **Client kickoff call:** internally agreed on **Thursday 30 July, 11:00** (within Cameron's
  Wed–Fri 10:00–13:00 window). **Still to be confirmed with Cameron.**
- Everyone was aligned on the overall design direction.

## Discussion / notes

- Nobody currently knows Go; we'll learn it together. Joe's first proxy task (getting
  `go-mysql` doing a handshake + reading queries) doubles as our Go onboarding, with Mieke
  pairing so the proxy isn't a one-person job.
- Keagan was absent, so his role is intentionally left open — Tate to catch up with him and
  slot him in (backend/store + API, or the proxy pair, depending on his preference/strengths).

## Action items

| # | Action | Owner | Status |
|---|--------|-------|--------|
| 1 | Confirm the Thu 30 Jul 11:00 kickoff time with Cameron (email) | Tate | Open |
| 2 | Catch up with Keagan on his role | Tate + Keagan | Open |
| 3 | First proxy task: `go-mysql` handshake + read query text (also Go onboarding) | Joe | Open |
| 4 | Set up GitLab issues + board for Sprint 1 | Tate | Open |
| 5 | Read `CONTRIBUTING.md` + `docs/ways-of-working.md` before the kickoff | Everyone | Open |

## Next

- Client kickoff call — Thu 30 Jul 11:00 (pending Cameron's confirmation).
