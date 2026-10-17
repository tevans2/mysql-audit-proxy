# Ways of Working

How the build is split, and how we run the project as a Scrum team.

## The build — 5 workstreams

The system has three planes: the **data path** (every byte of every query), the
**storage layer**, and the **control plane** (humans configuring + reviewing). The golden
rule: the control plane must never slow the data path.

| # | Workstream | What it does |
|---|-----------|--------------|
| **A** | **Proxy core** | Accepts MySQL client connections, relays them to the upstream server, and reads the queries as they pass through. The hard/interesting part — Go + networking + the MySQL wire protocol. |
| **B** | **Audit pipeline + data model** | Takes captured connections/queries and writes them to the audit store efficiently, **without blocking the live query path**. Includes the log schema. |
| **C** | **Store + API** | The audit database itself, plus the back-end API the UI talks to (config management, log search, admin auth). |
| **D** | **Back-office UI** | The web front-end: a log viewer (search/filter) and screens to manage proxy config. |
| **E** | **Infra / QA + Coordination (the glue)** | Cross-cutting — touches all of A–D. See the breakdown below. |

**How it connects:** client → proxy accepts the connection → relays bytes, reading each
query → hands records to the audit pipeline → pipeline batches them into the audit store;
meanwhile an admin uses the UI → API → edits config → proxy reloads, and the same API
reads the store to show the logs.

### What workstream E involves (the glue role)

A, B, C and D each build one *part* of the product. **E is the part that is not a single
feature — it's the foundation everything else runs on, plus the work of holding the pieces
together.** If you're not sure what "infra" means: it's the shared plumbing that isn't a
feature itself, but is what lets features get built, tested, run and demoed.

Concretely, E covers:

- **Infrastructure (the shared plumbing):**
  - `docker-compose` that spins up the whole system — proxy + audit DB + back-office + a
    couple of demo MySQL servers — so anyone can run and demo it with one command.
  - The **GitLab CI pipeline** (`.gitlab-ci.yml`) that builds, lints and tests every push.
  - Repo structure, dev tooling, and the local dev environment so everyone's set up the same.
- **QA (making sure it actually works):**
  - The testing framework + the **integration tests** that prove the proxy relays traffic
    correctly (large results, prepared statements) without corrupting the stream — our
    evidence for the demos.
  - Keeping `main` green and demoable.
- **Planning & speccing:**
  - Breaking the project into **issues, sprints and milestones**; keeping the board current.
  - Keeping the **requirements + design docs** up to date as scope firms up (turning the
    client's brief into concrete, buildable tasks).
- **Integration (gluing it together):**
  - Making sure A–D fit together — e.g. the data-model/API contract everyone codes against,
    and that the proxy, store, API and UI actually connect end-to-end.
  - Unblocking people and spotting where two workstreams need to agree an interface.
- **Coordination:**
  - Running stand-ups/retros, and being the point of contact for the **client and demi**.

In short: E is hands across **every** part of the project — some of its own build work
(infra + tests, which are real, committable code), plus the planning, speccing, integration
and coordination that keep the other four workstreams moving and fitting together.

### Current assignments

- **Joe Mudge** — Proxy core (A) + pipeline (B), leading the trickiest part
- **Mieke Wepener** — pairs with Joe on A/B so the crux isn't a one-person job
- **Keagan Slabbert** — Store + API (C): audit store, back-end API, and the data model
- **Maria Kotze** — Back-office UI (D)
- **Tate Evans** — Infra/QA + planning/speccing + integration + coordination (E) — across all
  workstreams, gluing the pieces together (see the breakdown above)

## Scrum

The project runs as a Scrum team over **three sprints**, each ending in a **demo of working
software to the client**.

| Sprint | Window |
|--------|--------|
| 1 | 27 Jul – 21 Aug |
| 2 | 24 Aug – 25 Sep |
| 3 | 28 Sep – 16 Oct |

**Roles**
- **Product Owner** — the client (Cameron / Acumen): owns priorities.
- **Scrum coordinator** — Tate: facilitates, removes blockers, handles client/demi comms.
- **Developers** — all of us.

**Ceremonies**
- **Sprint planning** — pick + break down the sprint backlog at the start of each sprint.
- **Stand-ups** — a couple of short syncs a week + quick async updates in the team chat.
  Format: what I did / what I'll do / what's blocking me.
- **Sprint review** — demo the working increment to the client + demi at sprint end.
- **Retrospective** — after each sprint: what went well, what to improve (pick 1–2 changes).

**Artifacts (all in GitLab)**
- **Product backlog** — all work, as GitLab **issues**.
- **Sprint backlog** — issues assigned to the current **milestone**, tracked on the **board**
  (To Do → In Progress → In Review → Done).
- **Increment** — the working product demoed each sprint.

## Principles

- **Working software every sprint.** Scope so *something* runs and demos — small-but-working
  beats ambitious-but-broken.
- **Build in vertical slices** — thin end-to-end features, deepened each sprint.
- **Welcome change.** The brief is deliberately underspecified; when the client clarifies
  scope, we re-plan the backlog rather than cling to the old plan.
- **Visible, balanced contribution.** Marks are moderated by peer review — everyone owns
  real, git-visible work.
