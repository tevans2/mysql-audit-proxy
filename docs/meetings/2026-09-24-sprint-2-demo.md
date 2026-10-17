# Sprint 2 Demo — Runsheet

**Date:** Thu 24 Sep 2026 · **Sprint:** 2 (24 Aug – 25 Sep) · **Weight:** 15%

**Client:** Acumen Software — Michael Botha (CTO), Cameron Hatch
**Marker:** Sphelelo Mhlongo

**Driving:** _____ · **Narrating:** _____ · **Machine:** _____

## Sprint 2 in one sentence

A proxy fronting **several** databases, connectable from **real client tooling**, with
configuration and access control managed through the back-office.

Everything below is evidence for that sentence.

---

## Pre-flight

Run through this **before** anyone joins. Budget 20 minutes.

| # | Check | Command | Expect |
|---|---|---|---|
| 1 | Clean start | `make down && make up` | all services up |
| 2 | Both upstreams distinguishable | `make test-env-check` | `marker: db-a`, `marker: db-b` |
| 3 | Routing correct | `make routes` | `:3307 -> db-a`, `:3308 -> db-b` |
| 4 | API alive | `curl -s localhost:8080/healthz` | `ok` |
| 5 | Viewer loads + login works | `make ui` → http://localhost:8000 | login screen, then data |
| 6 | Logs tailing in a spare pane | `make logs` | events flowing |

**Blockers to clear before the meeting starts:**

- [ ] **Admin password.** The bcrypt hash is in `deploy/docker-compose.yml`, but the plaintext
      is **not written down anywhere in the repo**. Maria set it in #29 — confirm it, test the
      login, and add it to `deploy/README.md`. If nobody can log in, steps 5–7 below are dead.
- [ ] **Port 8000 free.** The API's CORS origin is `http://localhost:8000` specifically. Any
      other port is blocked by the browser and the page loads empty. `make ui` checks this.
- [ ] Close Slack, mail, notifications. Browser zoom up; terminal font up.
- [ ] Decide the config-screens question below.

---

## Decide before the demo: what the config screens actually do

**This needs a call at today's meeting.** In the stack as it ships, the config screens do
**not** affect the running proxy — and not just "until restart."

`deploy/docker-compose.yml` sets `PROXY_CONFIG_FILE: /etc/lustre/upstreams.json` on the proxy.
While that is set, the static file wins and the proxy **never calls the API for its routing
table** (the static configuration path, now in `internal/proxy/app/startup.go`). Meanwhile the API seeds its `servers` table from that same
file and the config screens edit the **database copy**. The two are disconnected: editing a
server in the back-office changes the database, and the proxy — running or restarted — keeps
reading the file.

Two ways to play it.

### Option 1 — Safe (default)

Demo the screens as **configuration management against the back-office store**: list, add,
edit, delete, with credentials encrypted at rest. Say plainly that in this stack the running
proxy takes its routing from a static file, and that wiring the proxy to the stored
configuration is Sprint 3's first item.

Honest, and it needs no changes today.

### Option 2 — Stronger, but must be proven this afternoon

Drop `PROXY_CONFIG_FILE` from the `proxy` service. The proxy then loads configuration from the
API — `API_URL` derives from `SINK_INGEST_URL` and `API_PROXY_TOKEN` is already set, so that
one line is genuinely all that changes.

**The catch:** `upstreams.json` deliberately holds no passwords, so the seeded rows have empty
upstream passwords and both listeners fail to connect. The upstream passwords (`rootpass` for
db-a, `rootpass-b` for db-b) have to be entered **through the config screens** first.

That turns into a strong demo — *the back-office is where the proxy's configuration lives* —
but it is untested, and it is the day before. **Only take Option 2 if it is proven working
end to end before this afternoon's meeting ends.** Otherwise Option 1.

> Either way, do not say "configuration changes take effect live." They don't.

---

## Runsheet

Target ~35 minutes, leaving 15+ for questions.

### 1 · Frame it — 2 min

Recap Sprint 1 in a sentence (one database, end-to-end pipeline, live viewer requested), then
state the Sprint 2 goal above. Say what you'll show, in order. No slides.

### 2 · The stack — 2 min

```bash
make routes
```

```
  :3307 -> db-a
  :3308 -> db-b
each proxy port reaches its own upstream
```

**Say:** one proxy process, one listener per configured port, each with its own upstream and
credentials. The two databases are deliberately unalike so a misrouted query fails visibly
instead of returning believable rows.

### 3 · Multiple upstreams, routed by port — 4 min

Two shells, side by side:

```bash
make mysql-a    # 3307
```
```sql
SELECT name FROM lustre_test.upstream_marker;   -- db-a
SELECT * FROM sales.customers;
```

```bash
make mysql-b    # 3308
```
```sql
SELECT name FROM lustre_test.upstream_marker;   -- db-b
SELECT * FROM hr.employees;
```

**Say:** the port picks the database. Same client, same credentials, different upstream. And
the schemas share no table names — `sales.customers` against db-b errors rather than
succeeding, which is how we know routing is real.

**This is the headline Sprint 2 feature.** Don't rush it.

### 4 · The live-updating viewer — 5 min

*The one change the client asked for at the Sprint 1 demo. Call that out.*

Open http://localhost:8000, log in, and leave it visible. Run a query in the MySQL shell from
step 3 and let the row **appear without a reload**. Click a connection to filter the statements
below it.

**Say:** every connection and every statement — who ran what, when, against which server.

### 5 · Failed logins are audited — 3 min

```bash
docker compose -f deploy/docker-compose.yml exec client \
  mysql -h proxy-a -P 3307 -u alice -pWRONGPASSWORD \
  -e "SELECT 1;"
```

Rejected, and an `auth_fail` event appears in the viewer.

**Say:** a rejected login is a security event, not a non-event. Sprint 1 rejected it correctly
but recorded nothing.

### 6 · Real client tooling — 5 min

Two things shipped here: **prepared statements** and **MySQL 8 default authentication**.

Connect with **DBeaver** (or any JDBC client) to `127.0.0.1:3307`, user `alice`, password
`alicepw`. Browse `sales`, run a query, and show it landing in the viewer with the **prepared
SQL** — not "executed statement 3".

**Say:** in Sprint 1 this client could not connect at all. Two reasons, both fixed. Prepared
statements weren't supported, which excludes most real tooling. And we only offered
`mysql_native_password`, which MySQL 9 removed outright — a current client failed before a
packet reached us.

> **Rehearse this step specifically.** It is the one with someone else's software in it.
> Have the connection profile saved and tested. Fallback: a scripted client using the binary
> protocol, prepared before the meeting.

**If asked about TLS:** we terminate TLS on the client connection with a self-signed
certificate generated in memory, because `caching_sha2_password` requires a secure channel.
The proxy-to-database leg is **not yet encrypted** — that's Sprint 3, and we have a question
for you about certificates.

### 7 · The back-office — 5 min

- **Admin login.** Log out, show the gate, log back in. *The audit log is every query every
  user has run — in Sprint 1 anyone who could reach the page could read it.*
- **Configuration screens.** Per the decision above — Option 1 or Option 2.
- **Credentials at rest.** Upstream passwords are stored AES-256-GCM encrypted, and the
  back-office endpoints never serve them back.

### 8 · Performance — 5 min

*Acumen specified 25–50 concurrent connections. Until this sprint that was unmeasured.*

Show `docs/perf/sprint-2-baseline.md` — the tables, not a live run (`make perf` takes far too
long for a demo).

Four numbers, said from memory:

- **~1–4 ms** added per query at 25–50 connections
- **Zero** audit records lost at the specified load (2,505 statements/second, three runs)
- First loss at **~7,500/s** — roughly **2× headroom** before the guarantee changes
- **~340 µs** added per connection, paid once per session

**Then the caveat, unprompted:** measured on one laptop against Docker. The *difference*
between direct and proxied is the transferable number; the absolute milliseconds are not
theirs.

**And the honest one:** above ~7,500 statements/second the audit sink drops records rather
than blocking queries. That is a deliberate design trade — the query path is protected first —
and it leads into question 2 below.

### 9 · How we work — 3 min

*Aimed at Sphelelo; both questions last time were about process.*

- Three CI stages on every push: lint, unit/integration against a real MySQL 8.0, and a
  **two-database end-to-end job** driving the built binaries.
- Every bug fix ships with a regression test verified failing first.
- GitLab issues and board; ADRs recording key decisions; known limitations written down.

### 10 · What's next — 2 min

Sprint 3 is the last one. Name the candidates — live configuration reload, proxy-to-database
TLS, retention, tamper-evidence, exposing the audit drop counter — and go straight into the
questions.

---

## Questions to put to Acumen

**Sprint 3 is our final sprint. Anything not decided here gets built on a guess, or not at
all.** Say that.

| # | Question | Owner |
|---|---|---|
| 1 | **Per-user authorisation**, or is the audit trail sufficient? Every session reaches MySQL as one service account today — identity is recorded, not enforced. *(Asked 20 Aug, still open.)* | |
| 2 | **Drop or block?** Above ~7,500 stmt/s we protect queries and drop audit records. For a compliance tool, is that the right trade — or would you rather we slow queries than lose a record? | |
| 3 | **SQLite or Postgres?** At your specified load SQLite is lossless with 2× headroom. Migrating in the final sprint is expensive, so we need the call now. | |
| 4 | **Raw SQL with literal values** is stored, which makes the audit log POPIA-relevant. Fingerprint-and-separate, or leave as is? | |
| 5 | **Retention** — how long, and delete or archive at expiry? | |
| 6 | Is **25–50 concurrent connections** still the real figure? What's your actual peak? | |
| 7 | For proxy-to-database TLS, **what certificates would you use** — your own CA, or self-signed in your environment? | |
| 8 | Of what remains, **what are your top three**? | |

---

## Q&A prep

**"Is it encrypted end to end?"**
No, and it structurally can't be — an auditing proxy has to see the SQL to audit it. What we
have is two independently encrypted legs with the proxy in the middle. The client leg is
encrypted today; the database leg is Sprint 3.

**"Can someone bypass the proxy?"**
Yes, today — anyone with the database credentials and network access can connect directly and
leave no audit record. Closing that means mutual TLS with `REQUIRE X509` on the database user,
or network isolation. It's on the Sprint 3 list and it's question 7.

**"Can you prove no audit records were lost?"**
Not yet — the sink counts drops but nothing exposes the counter, so the only evidence is a log
line. Exposing it is the top item on our Sprint 3 list. Don't oversell this one.

**"What happens if the back-office goes down?"**
Queries keep flowing. The sink buffers and retries for about fifteen seconds, then drops the
batch and logs it. Deliberate — auditing never blocks a query — but it is best-effort, not
durable. Don't describe it as durable.

**"Is the audit log tamper-proof?"**
No. It's a database file; someone with access could edit rows undetectably. Hash-chaining is
designed, not built.

**"How many people can use it?"**
Straight to the four numbers in step 8.

---

## If something breaks

| Symptom | Do |
|---|---|
| A container is unhealthy | `make down && make up`, then `make routes`. Narrate while it comes up. |
| Viewer empty / CORS error | Port 8000 free? `make ui` refuses anything else and says why. |
| Viewer won't load at all | Flip `USE_LIVE_API = false` in `web/index.html` for mock mode — **say** it's mock data. |
| DBeaver won't connect | Fall back to the scripted binary-protocol client. Don't debug live. |
| A step fails twice | Move on. Say you'll follow up, and do. Never debug in front of the client. |

Nothing broke at the Sprint 1 demo because the plan held. Same discipline.

---

## Immediately after

- Write the outcome into this file, as we did for Sprint 1: what was asked, what was
  requested, what was decided.
- **Capture every answer to the eight questions** — those are Sprint 3's inputs.
- Note any new request the way the live viewer was noted last time; it becomes a Sprint 3
  issue.

---

# Outcome — held 25 Sep 2026

Moved from Thursday 24 (public holiday) to Friday 25 Sep. Ran to plan; reception was very positive.

## Client

Michael Botha (CTO) was the main participant, as at Sprint 1. Positive on progress and direction.

## Questions from the markers

None recorded.

## Answers to our questions

**1. Per-user authorisation — yes.** Michael wants the proxy to connect to the database **as each user**, with separate upstream connections per user, rather than every session reaching MySQL through one shared account. Identity must be enforced at the database, not only recorded in the audit log.

**2. Drop or block — block.** This is an auditing tool in front of non-critical databases, so the audit log takes priority. **The default behaviour should be to block queries rather than drop audit records.** "Default" implies it stays configurable, with block as the default.

**Not covered:** top three priorities, SQLite vs Postgres, raw SQL / POPIA masking, retention, whether 25–50 connections is still the real figure, certificates for proxy-to-database TLS.

## Feedback / action

- **Per-user upstream identity → Sprint 3 P1.** This was the item flagged as changing the sprint's shape if required. It is required.
- **Audit sink defaults to block → Sprint 3 P1.** This reverses the trade recorded in ADR 0003 and the "auditing never blocks a query" rule from Sprint 1 — needs a new ADR that supersedes it.
- Follow up the uncovered questions by email early in Sprint 3. Retention and POPIA masking change the schema, so they need answers in week one.

## Notes for next time

The two questions asked first got decisive answers; the rest ran out of time. Next demo, ask the schema-affecting questions ahead of time by email rather than relying on the last five minutes.
