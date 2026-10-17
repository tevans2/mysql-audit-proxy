# Meeting 1 — Client Kickoff (Acumen)

**Date:** Friday 31 July 2026 · **Time:** 09:00 (SAST) · **Platform:** Microsoft Teams
**Type:** Client kickoff — introductions + scope for Sprint 1 · **Status:** ✅ Held

**Team Lustre:** Tate Evans (coordinator/infra), Joe Mudge (proxy lead), Mieke Wepener (proxy), Keagan Slabbert (traffic-side/API), Maria Kotze (back-office UI)
**Acumen:** Francois Heunis (Development Manager, `francois.heunis@acumensoft.net`), Michael Botha (CTO, spec author, `michael.botha@acumensoft.net`)

## Project overview (from the client)

- Acumen builds **ForceLink**, a customizable ERP / workforce-management tool — manages field staff, assets, and distributed resources for corporate clients.
- **Current pain:** ~25 analysts/devs share DB credentials, so there's no per-user accountability. They have **ISO 27001 audit-trail** needs.
- **The proxy's purpose:** provide **per-user audit logging** and better observability, and **replace manual shared-credential handling with controlled proxy access**.

This confirms our research hypothesis: the real driver is auditing direct human SQL access, tied to a compliance (ISO 27001) obligation.

## Technical decisions & answers

- **MySQL version: 8.0** (so default auth is `caching_sha2_password` — factor this into the handshake).
- **Scale: 25–50 concurrent connections.** Multiple schemas → multiple ports.
- **Encryption / TLS:** client↔proxy and proxy↔DB **should both be TLS**; **proxy↔DB always encrypted**. Full encryption handshake can be **deferred past the MVP**.
- **Auth model direction:** proxy provides *controlled per-user access* replacing shared credentials (per-user identity is the point of the project).
- **API layer** stores captured queries and exposes the **port→DB mapping config** for the UI.
- **UI** displays logs; can be crude for the MVP.

## MVP priorities (agreed)

1. Basic proxy up — **relay + query capture**.
2. **Audit-logging** of captured SQL commands.
3. Simple **API** to receive + store logs in a test DB.
4. **Minimal UI** for log viewing only (polish later).
5. **Defer** full encryption handshake and high-concurrency handling to later sprints.

## Sprint 1 plan & timeline

- Sprint length: ~**3 weeks** (2–3 discussed).
- Target: minimal working proxy + basic API by **early August**.
- **Demo: end of August, ~20th.**
- Weekend goal: start proxy code, share design docs, assign concrete tasks.
- After the demo: iterate on encryption, concurrency, and UI polish.

## Open questions / decisions still to resolve

- Define the **real-time config-update flow** via the API (how port-mapping changes propagate live).
- Decide **MVP encryption scope** (handshake now vs. always-on TLS later) — confirm the cut line.
- Validate that **25–50 concurrent connections** covers future load expectations.

## Action items

| # | Action | Owner | Status |
|---|--------|-------|--------|
| 1 | Resend meeting invite to Michael and Francois | Tate | ✅ Done |
| 2 | Share repository link + initial docs with the team | Tate | Open |
| 3 | Upload proxy design diagrams + detailed specs (post-meeting) | Tate | Open |
| 4 | Send follow-up message with concrete weekend goals for proxy work | Tate | Open |
| 5 | Begin implementation: basic proxy relaying + simple API (target end of next week) | Joe / Mieke / Keagan | Open |
| 6 | Prepare minimal demo for end-August (~20th) | Team | Open |
