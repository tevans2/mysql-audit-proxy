# Testing Strategy

How we test the system, how each person tests their part, and — most importantly — how to **build our code so tests are easy to write**. Testing isn't a phase at the end; it's how we know each piece works, and it's the evidence our marks depend on ("every claim backed by executable tests").

## 1. Why we test
- **Correctness we can't eyeball:** a proxy can subtly corrupt a byte stream in a way that only shows under load or on a big result. Tests catch what a manual click-through won't.
- **Confidence to change:** with tests, anyone can refactor without fear of silently breaking someone else's part.
- **Evidence for marking:** the course rewards claims backed by reproducible tests. A green test suite *is* the proof.

## 2. The shape of our tests (the pyramid)
We want **many fast unit tests, some integration tests, few end-to-end tests.**

```
        ┌───────────────┐
        │  end-to-end    │  few — whole system via docker-compose (the demo path)
        ├───────────────┤
        │  integration   │  some — real MySQL, real SQLite, real HTTP between parts
        ├───────────────┤
        │     unit       │  many — pure logic, fast, no network/DB
        └───────────────┘
```

- **Unit** — one function/type in isolation, no I/O. Milliseconds. The bulk of our tests.
- **Integration** — a part against a real dependency (proxy ↔ real MySQL; API ↔ real SQLite; sink ↔ real HTTP server).
- **End-to-end** — the whole slice via `docker-compose`: connect a client, run a query, see it in the UI. This is the demo, run as a test.

## 3. Design for testability (this is what makes TDD possible)
You can only test-drive code that's built to be substitutable. Five rules — follow these and tests become easy; ignore them and testing becomes painful.

> **As built (16 Oct 2026).** This strategy was written up front as intent. Two
> things differ in the finished system. The API holds a concrete `*store.Store`, not a `Store`
> interface, and tests use an in-memory SQLite database instead of a fake
> ([ADR 0007](design/decisions/0007-sqlite-remains-the-audit-store.md)). And the audit sink now
> blocks when its buffer is full unless `SINK_ON_FULL=drop`
> ([ADR 0005](design/decisions/0005-audit-sink-blocks-by-default.md)), so the "`Emit` never
> blocks" test below applies to drop mode only. The UI's pure helpers have `make web-test`; there
> are no browser tests.

1. **Depend on interfaces, not concrete things.** The proxy depends on `Sink`, not on HTTP or the DB. The API depends on `Store`, not on SQLite. In a test you pass a fake; in production you pass the real one. (These interfaces already live in `/internal/audit`.)
2. **Separate pure logic from I/O.** Parsing a MySQL packet, classifying a SQL statement, building a batch — these are **pure functions** (input → output, no side effects) and are trivial to test. Keep the network/DB calls at the edges, thin.
3. **Inject dependencies through constructors.** `NewAPI(store Store)`, `NewSink(client HTTPDoer)`. Never reach for a global or construct a DB connection deep inside a function — pass it in, so a test can pass a fake.
4. **Inject time and randomness.** Don't call `time.Now()` directly in logic you want to test — take a `Clock` interface, so tests are deterministic. Same for anything random.
5. **Keep functions small and single-purpose.** Small functions are easy to test exhaustively; giant functions are not.

If a piece of code is hard to test, that's a **design smell** — usually it's doing I/O and logic together, or constructing its own dependencies. Fix the design, don't skip the test.

## 4. The TDD loop
Red → Green → Refactor:
1. **Red:** write a failing test that describes the behaviour you want.
2. **Green:** write the simplest code that makes it pass.
3. **Refactor:** clean it up with the test as your safety net.

You don't have to TDD everything, but it shines for **pure logic** (parsing, classification, batching, filtering) — write the test first, and the function almost designs itself. Example, test first:

```go
func TestClassify(t *testing.T) {
    cases := []struct{ name, in, want string }{
        {"select", "SELECT 1", "SELECT"},
        {"insert lower", "insert into t values (1)", "INSERT"},
        {"leading space", "  UPDATE t SET x=1", "UPDATE"},
        {"unknown", "BEGIN", "OTHER"},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            if got := classify(c.in); got != c.want {
                t.Fatalf("classify(%q) = %q, want %q", c.in, got, c.want)
            }
        })
    }
}
```
Now write `classify` until it's green. **Table-driven tests** like this are the idiomatic Go pattern — add a row per case; use them everywhere.

## 5. Tooling
- **`go test`** + the standard `testing` package — our base. `go test ./...` runs everything.
- **Table-driven tests** with subtests (`t.Run`) — the default style.
- **`net/http/httptest`** — test API handlers without a real socket.
- **In-memory SQLite** (`:memory:`) — fast, real SQL, no files.
- **`testcontainers-go`** — spin up a real MySQL 8.0 in a container for proxy/integration tests.
- **CI** runs unit + build + lint on every push and pull request; integration tests run in CI too (they need Docker).
- Optional: `testify` for terser asserts — fine to use, not required.

## 6. How each part is tested

### Proxy (`/proxy`)
- **Unit (most of it):** the packet/command detection and SQL extraction are pure functions — table-driven tests over byte slices. `classify`, "is this a `COM_QUERY`?", multi-packet framing math. Test-drive these.
- **Relay logic:** test with a **fake `Sink`** (records events in a slice) and in-memory pipes (`net.Pipe`) so no real MySQL is needed — assert the right events are emitted for a scripted exchange.
- **Integration:** `testcontainers` real MySQL 8.0 — connect through the proxy, run a `SELECT`, assert the result is uncorrupted and a statement event was emitted. This is where you prove pass-through correctness.

```go
type fakeSink struct{ events []audit.Event }
func (f *fakeSink) Emit(e audit.Event) { f.events = append(f.events, e) }
// inject fakeSink into the session, drive a scripted client, assert f.events
```

### Sink / pipeline (`/proxy` / `/internal/audit`)
- **Unit only, no real network:** inject a **fake HTTP client** (`HTTPDoer` interface). Test: events get batched; a batch is POSTed at the size/time threshold; **fail-safe** — when the fake client returns an error, `Emit` still never blocks or panics (the query path is protected). This is the most important behaviour to prove.
```go
type HTTPDoer interface { Do(*http.Request) (*http.Response, error) }
// pass a fake that returns errors -> assert Emit still returns immediately
```

### API + Store (`/backoffice`)
- **Handlers:** `httptest` + an **in-memory `Store`**. POST a batch to `/api/events`, assert `202` and that the store received it; GET `/api/connections`, assert the JSON shape.
- **Store:** run the SQLite implementation against **in-memory SQLite** — save events, query them back, assert. Same tests can later run against Postgres to prove the interface holds.
```go
store := NewMemStore()
srv := NewAPI(store)
req := httptest.NewRequest("POST", "/api/events", strings.NewReader(`[{"kind":"connect","user":"bob"}]`))
rec := httptest.NewRecorder()
srv.ServeHTTP(rec, req)
// assert rec.Code == 202 and store has 1 event
```

### End-to-end (`/e2e`)

Runs the built `proxy` and `api` executables as subprocesses against two real
upstream databases, connects a MySQL client through each proxy port, and reads
the audit trail back over HTTP. It covers what no package test can: that the
pieces are wired to each other correctly.

The distinction earns its keep. `GET /api/servers` returned an empty list for the
whole of the first version — `cmd/api` read its config from a path nothing supplied. Every
package test passed, because the gap was in `main`'s environment handling rather
than in any package it calls. Running the real binary is what catches that class
of bug.

What it asserts:

| Test | What it proves |
|------|----------------|
| `TestEachProxyPortReachesItsOwnUpstream` | Each port reaches its own database — the acceptance test for the multi-upstream environment |
| `TestUpstreamsAreNotInterchangeable` | The databases really do differ, so a routing bug cannot hide. Asserts each serves its own schema *and* rejects the other's — checking only the rejection would pass against two empty databases |
| `TestAuditTrailRecordsWhichUpstream` | The full path — client, proxy, sink, API, store — tags each statement with the database it reached |
| `TestServersEndpointDescribesRunningStack` | The back-office is told the mapping the proxies actually route by |
| `TestConnectionRecordsPortAndUpstream` | A session is attributed to both the port it arrived on and the database it reached |

The tests skip when an upstream is unreachable, so the suite still passes on a
machine with nothing running. A skip covers nothing, though, so CI sets
`E2E_REQUIRE=1`, which turns an unreachable upstream into a failure instead of a
green tick that means nothing.

They seed what they assert on (the routing marker and each upstream's own
schema), because CI attaches bare MySQL service containers with nowhere to mount
an init script. The seeding identifies a server before writing to it — seeding
first and checking afterwards leaves a marker naming the wrong server behind on
the way to the failure, corrupting the environment every other test reads.

### UI (`/web`)
- Keep it light (crude UI is fine for MVP). **Manual testing** against the stub API + the real API is the baseline. If time allows, a couple of component tests, but don't over-invest — the client said don't polish the UI, and that applies to its tests too.

## 7. Test doubles we provide (in the scaffolding)
So nobody is blocked and everyone can test in isolation:
- **In-memory `Store`** — for API tests and for running the API before the DB schema is final.
- **Fake `Sink`** — for proxy tests without HTTP.
- **Stub API server** — returns canned JSON, for UI development + sink tests.

Use these; don't stand up real infrastructure for a unit test.

## 8. Running tests
```
make test          # go test ./...  (unit + integration)
make test-e2e      # end-to-end: brings up both upstreams first
make perf          # performance baseline (PERF-gated; never runs in CI)
go test ./internal/proxy/...   # just one part
go test -run TestClassify ./proxy   # one test
go test -cover ./...  # with coverage
```

The integration and end-to-end tests need upstream databases — `make test-env`
starts them. Without them they skip, which is not the same as passing.
CI runs the same on every push and pull request; a red suite blocks merge.

The performance tests are deliberately outside that loop. They measure rather
than assert, so they are gated on `PERF=1` and CI never runs them: a threshold
baked into a test would either fail on a loaded shared runner or pass straight
through a real regression on a quiet one. Results and method live in
[docs/perf](perf/baseline.md).

## 9. Definition of Done (ties back to CONTRIBUTING)
A task isn't done until it's **tested**: pure logic has unit tests, a part talking to a real dependency has at least one integration test, and CI is green. "It works on my machine" without a test doesn't count — an untested feature is invisible evidence at marking time.

## 10. What NOT to test
- **Don't test the standard library or `go-mysql`** — assume they work; test *our* use of them.
- **Don't write brittle tests** tied to exact log strings or internal wiring — test behaviour (inputs → outputs / effects), not implementation detail.
- **Don't chase 100% coverage** — cover the logic that matters (parsing, batching, fail-safe, store queries); skip trivial getters.

## 11. Quick reference — where each kind of test lives
| Part | Unit (fake deps) | Integration (real dep) |
|------|------------------|------------------------|
| Proxy | packet/SQL parsing, relay w/ fake Sink | real MySQL via testcontainers |
| Sink | batching + fail-safe w/ fake HTTP | (covered by end-to-end) |
| API | handlers w/ in-memory Store | — |
| Store | — | SQLite in-memory |
| Whole system | — | `/e2e` — the real binaries against two upstreams |
| Performance | — | `e2e/perf_test.go` — measurements, not assertions; `PERF=1` |
