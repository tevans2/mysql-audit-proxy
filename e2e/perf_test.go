// Performance baseline for the proxy: what does putting Lustre in front of a
// database cost, at the 25-50 concurrent connections the client specified?
//
// These are measurements, not assertions, so they are gated on PERF=1 and
// never run as part of `go test ./...` or CI. A benchmark that fails a build
// on a loaded shared runner teaches nobody anything; `make perf` runs them
// deliberately, on a machine whose state is known and recorded.
//
// The shape of every run is the same: the identical workload is driven first
// straight at the upstream and then through the proxy, in one process, so the
// two share machine conditions. The number that answers the client's question
// is the difference between them, not either one on its own -- absolute
// latency here includes a Docker Desktop VM hop that their production would
// not have.
package e2e

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// requirePerf skips unless the caller explicitly asked for a perf run.
func requirePerf(t *testing.T) {
	t.Helper()
	if os.Getenv("PERF") == "" {
		t.Skip("performance baseline: set PERF=1 (or run `make perf`)")
	}
}

// perfConfig is one run's shape. Every field is env-overridable so a result
// can be reproduced, or the run shortened while iterating on it.
type perfConfig struct {
	// warmup is discarded before measuring: the first queries on a fresh
	// connection pay for MySQL's buffer pool and Go's allocator, and folding
	// that into a p99 reports a startup cost as a steady-state one.
	warmup time.Duration
	// duration is the measured window, after warmup.
	duration time.Duration
	// interval paces each connection. Unpaced, fifty connections against a
	// sub-millisecond query produce hundreds of thousands of audit events in
	// seconds, which measures how fast SQLite can be filled rather than what
	// the proxy costs. 20ms models each session issuing 50 queries a second --
	// brisk for a person, ordinary for an application. Set PERF_RATE=0 to
	// remove the pacing and drive the system to saturation on purpose.
	interval time.Duration
	// conns is the set of concurrent connection counts to measure at.
	conns []int
}

func perfConfigFromEnv(t *testing.T) perfConfig {
	t.Helper()
	cfg := perfConfig{
		warmup:   perfDuration(t, "PERF_WARMUP", 3*time.Second),
		duration: perfDuration(t, "PERF_DURATION", 10*time.Second),
		conns:    perfConns(t, "PERF_CONNS", []int{1, 25, 50}),
	}
	if rate := perfInt(t, "PERF_RATE", 50); rate > 0 {
		cfg.interval = time.Second / time.Duration(rate)
	}
	return cfg
}

func perfInt(t *testing.T, key string, fallback int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q: not an integer: %v", key, raw, err)
	}
	return n
}

func perfDuration(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s=%q: not a duration: %v", key, raw, err)
	}
	return d
}

func perfConns(t *testing.T, key string, fallback []int) []int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	var out []int
	for _, f := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 {
			t.Fatalf("%s=%q: want a comma-separated list of positive integers", key, raw)
		}
		out = append(out, n)
	}
	return out
}

// ---------------------------------------------------------------------------
// Measuring
// ---------------------------------------------------------------------------

// loadResult is one (path, connection count) measurement.
type loadResult struct {
	path    string // "direct" or "proxied"
	conns   int
	queries int // completed in the measured window
	// issued is every statement put on the wire, warm-up and measured window
	// alike, successful or not. The audit log should contain all of them --
	// a statement that returned an error is still a statement that was run --
	// so this, not queries, is what audit completeness is measured against.
	issued int
	failed int
	// behind counts iterations that were already past their scheduled start
	// when they began -- the system is not keeping up with the offered rate.
	// Latency alone hides this: a saturated system that serves each query
	// quickly but cannot start them on time still looks fast.
	behind  int
	elapsed time.Duration
	p50     time.Duration
	p95     time.Duration
	p99     time.Duration
	max     time.Duration
}

func (r loadResult) qps() float64 {
	if r.elapsed <= 0 {
		return 0
	}
	return float64(r.queries) / r.elapsed.Seconds()
}

// summarise sorts samples and fills in the percentiles.
//
// Percentiles rather than a mean throughout: a mean hides exactly the tail an
// operator notices, and with two paths to compare it can hide a regression in
// one behind an improvement in the other.
func summarise(r *loadResult, samples []time.Duration) {
	if len(samples) == 0 {
		return
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	q := func(p float64) time.Duration {
		i := int(math.Round(p * float64(len(samples)-1)))
		return samples[i]
	}
	r.p50, r.p95, r.p99 = q(0.50), q(0.95), q(0.99)
	r.max = samples[len(samples)-1]
}

// runLoad drives conns concurrent sessions against dsn, running workload w,
// and reports the latency of the statements it produces.
//
// Each session gets its own *sql.DB pinned to a single connection. sql.DB is a
// pool, not a session (ADR 0002): without the pin, "50 concurrent connections"
// would be however many the pool decided to open, and on the proxied path each
// extra one is a separate audited session, so the thing being measured would
// not be the thing being described.
func runLoad(t *testing.T, cfg perfConfig, path, dsn string, conns int, w workload, tag string) loadResult {
	t.Helper()

	dbs := make([]*sql.DB, conns)
	for i := range dbs {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatalf("%s: opening connection %d: %v", path, i, err)
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		// Connect now, so the measured window does not include handshakes and
		// so a credential or routing mistake fails here rather than as a pile
		// of per-query errors.
		if err := db.Ping(); err != nil {
			t.Fatalf("%s: connection %d could not reach %s: %v", path, i, dsn, err)
		}
		dbs[i] = db
	}
	defer func() {
		for _, db := range dbs {
			db.Close()
		}
	}()

	// One clock for every worker. Deriving each worker's phase from a shared
	// start means they warm up and measure over the same window; letting each
	// time its own would have the quick ones measuring while the slow ones
	// were still warming.
	start := time.Now()
	measureFrom := start.Add(cfg.warmup)
	end := measureFrom.Add(cfg.duration)

	// Per-worker sample slices, merged at the end. A shared slice behind a
	// mutex would serialise fifty goroutines on every query and measure the
	// lock.
	samples := make([][]time.Duration, conns)
	failed := make([]int, conns)
	behind := make([]int, conns)
	issued := make([]int, conns)

	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			local := make([]time.Duration, 0, 4096)
			next := start
			for seq := 0; ; seq++ {
				now := time.Now()
				if now.After(end) {
					break
				}
				if cfg.interval > 0 {
					if now.After(next) {
						if now.After(measureFrom) {
							behind[worker]++
						}
					} else {
						time.Sleep(next.Sub(now))
					}
					next = next.Add(cfg.interval)
				}

				began := time.Now()
				issued[worker]++
				err := perfQuery(dbs[worker], w.sql(tag, worker, seq))
				took := time.Since(began)

				// Failures are counted across the whole run, warm-up
				// included, because issued is. A warm-up failure counted as
				// issued but never reported would surface later as audit
				// loss with nothing pointing at the cause.
				if err != nil {
					failed[worker]++
					continue
				}
				if began.Before(measureFrom) {
					continue // warm-up: ran, audited, but not timed
				}
				local = append(local, took)
			}
			samples[worker] = local
		}(i)
	}
	wg.Wait()

	r := loadResult{path: path, conns: conns, elapsed: cfg.duration}
	var all []time.Duration
	for i := range samples {
		all = append(all, samples[i]...)
		r.failed += failed[i]
		r.behind += behind[i]
		r.issued += issued[i]
	}
	r.queries = len(all)
	summarise(&r, all)

	if r.failed > 0 {
		t.Errorf("%s at %d connections: %d queries failed", path, conns, r.failed)
	}
	if r.queries == 0 {
		t.Fatalf("%s at %d connections: no successful queries in the measured window", path, conns)
	}
	return r
}

// perfQuery runs one statement and drains its result set.
//
// Draining matters: the proxy buffers a whole result set in memory rather than
// streaming it (docs/known-limitations.md), so a measurement that discarded
// rows would miss the cost that limitation describes.
func perfQuery(db *sql.DB, query string) error {
	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		// Scanning into nothing still moves the row off the wire, which is the
		// part being measured.
	}
	return rows.Err()
}

// ---------------------------------------------------------------------------
// Workloads
// ---------------------------------------------------------------------------

// perfRowCount is how many rows the scan workload reads. Small on purpose: the
// point is to exercise the result path, not to benchmark MySQL.
const perfRowCount = 100

// perfTable is seeded by this file rather than reused from deploy/initdb,
// because CI attaches bare MySQL containers with nothing mounted -- the same
// reason seedUpstream restates its schema.
const perfTable = "sales.perf_rows"

// seedPerfRows creates and fills perfTable. Idempotent: it tops the table up
// to perfRowCount rather than assuming the state it finds.
func seedPerfRows(t *testing.T, u upstream) {
	t.Helper()
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=10s", u.user, u.pass, u.addr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("connecting to %s: %v", u.name, err)
	}
	defer db.Close()

	for _, q := range []string{
		"CREATE DATABASE IF NOT EXISTS sales",
		"CREATE TABLE IF NOT EXISTS " + perfTable + " (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(64) NOT NULL)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seeding %s (%s): %v", perfTable, q, err)
		}
	}

	var have int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + perfTable).Scan(&have); err != nil {
		t.Fatalf("counting %s: %v", perfTable, err)
	}
	for i := have; i < perfRowCount; i++ {
		if _, err := db.Exec("INSERT INTO "+perfTable+" (name) VALUES (?)", fmt.Sprintf("row-%03d", i)); err != nil {
			t.Fatalf("filling %s: %v", perfTable, err)
		}
	}
}

// workload is one kind of statement to measure.
type workload struct {
	name string
	// sql builds one statement. Every call must return distinct text: the
	// store's de-duplication key is (conn_id, ts, sql_text), so two identical
	// statements landing in the same microsecond on one connection would
	// collapse into a single row and make the audit accounting in
	// TestPerfAuditCompleteness under-count for a reason that is not loss.
	sql func(tag string, worker, seq int) string
}

// perfWorkloads are deliberately two, at opposite ends of the result path.
func perfWorkloads() []workload {
	return []workload{
		{
			// No rows, no table: as close to pure proxy overhead as a real
			// statement gets.
			name: "select1",
			sql: func(tag string, worker, seq int) string {
				return fmt.Sprintf("SELECT 1 /* %s w=%d n=%d */", tag, worker, seq)
			},
		},
		{
			// perfRowCount rows through the relay, exercising the buffering
			// that ExecuteSelectStreaming would remove.
			name: fmt.Sprintf("scan%d", perfRowCount),
			sql: func(tag string, worker, seq int) string {
				return fmt.Sprintf("SELECT id, name FROM %s /* %s w=%d n=%d */", perfTable, tag, worker, seq)
			},
		},
	}
}

// perfTag makes one run's statements identifiable in the audit log, so a later
// count can attribute rows to that run and nothing else.
//
// Called once per run, never per statement: a tag that changed with every
// query would still read correctly in a latency table and would silently make
// the audit accounting in TestPerfAuditCompleteness count nothing.
func perfTag(name, path string, conns int) string {
	return fmt.Sprintf("perf=%s-%s-%dc-%d", name, path, conns, time.Now().UnixNano())
}

// ---------------------------------------------------------------------------
// Phase 1 -- what the proxy costs
// ---------------------------------------------------------------------------

// TestPerfProxyOverhead measures the same workload direct and proxied, at each
// configured connection count, and reports the difference.
func TestPerfProxyOverhead(t *testing.T) {
	requirePerf(t)
	a, _ := upstreams(t)
	seedUpstream(t, a)
	seedPerfRows(t, a)

	cfg := perfConfigFromEnv(t)
	s := startStackWith(t, stackOpts{}, a)

	directDSN := fmt.Sprintf("%s:%s@tcp(%s)/sales?timeout=10s", a.user, a.pass, a.addr)
	proxiedDSN := fmt.Sprintf("alice:%s@tcp(%s)/sales?timeout=10s&tls=skip-verify", alicePass(t), s.proxyAddrs[a.name])

	var rows []string
	for _, w := range perfWorkloads() {
		for _, n := range cfg.conns {
			direct := runLoad(t, cfg, "direct", directDSN, n, w, perfTag(w.name, "direct", n))
			proxied := runLoad(t, cfg, "proxied", proxiedDSN, n, w, perfTag(w.name, "proxied", n))

			rows = append(rows, fmt.Sprintf(
				"| %s | %d | %s | %s | %s | %s | %s | %s | %+.2f | %.0f | %.0f |",
				w.name, n,
				round(direct.p50), round(proxied.p50),
				round(direct.p95), round(proxied.p95),
				round(direct.p99), round(proxied.p99),
				(proxied.p95-direct.p95).Seconds()*1000,
				direct.qps(), proxied.qps(),
			))
			logResult(t, w.name, direct)
			logResult(t, w.name, proxied)
		}
	}

	t.Log("\n" + strings.Join(append([]string{
		"",
		"| workload | conns | p50 direct | p50 proxied | p95 direct | p95 proxied | p99 direct | p99 proxied | p95 added (ms) | qps direct | qps proxied |",
		"|---|---|---|---|---|---|---|---|---|---|---|",
	}, rows...), "\n"))
}

func logResult(t *testing.T, workload string, r loadResult) {
	t.Helper()
	t.Logf("%-8s %-7s %2d conns: %6d queries, %5.0f q/s, p50=%s p95=%s p99=%s max=%s, behind=%d failed=%d",
		workload, r.path, r.conns, r.queries, r.qps(),
		round(r.p50), round(r.p95), round(r.p99), round(r.max), r.behind, r.failed)
}

// round trims a duration to microseconds, which is as much precision as any of
// this is worth reporting.
func round(d time.Duration) time.Duration { return d.Round(time.Microsecond) }

// ---------------------------------------------------------------------------
// Phase 2 -- does it still audit everything under load
// ---------------------------------------------------------------------------

// auditDrainTimeout bounds waiting for the audit log to stop growing after a
// run. Generous, because the whole point is that the sink is behind.
const auditDrainTimeout = 60 * time.Second

// TestPerfAuditCompleteness measures whether the audit trail survives load.
//
// This matters more than the latency numbers. The sink buffers 1024 events,
// batches 50, and by default blocks when the buffer fills (ADR 0005;
// SINK_ON_FULL=drop restores dropping). Either way a fast enough client can
// outrun the audit log, and a proxy that relays quickly while losing records
// is not an auditing proxy. So: count what was run, count what can be read back, and
// report the difference.
//
// It does not assert a loss threshold. Where loss begins is a property of the
// machine, and a number baked in here would either fail on a loaded laptop or
// pass through a real regression on a fast one. It does fail if nothing at all
// arrives, which is a broken pipeline rather than saturation.
func TestPerfAuditCompleteness(t *testing.T) {
	requirePerf(t)
	a, _ := upstreams(t)
	seedUpstream(t, a)
	seedPerfRows(t, a)

	cfg := perfConfigFromEnv(t)
	s := startStackWith(t, stackOpts{}, a)
	proxiedDSN := fmt.Sprintf("alice:%s@tcp(%s)/sales?timeout=10s&tls=skip-verify", alicePass(t), s.proxyAddrs[a.name])

	// One workload is enough here, and the cheap one is the right choice: it
	// produces audit events faster than the scan for the same wall clock, so
	// it reaches the sink's limits sooner and finds the knee with less noise.
	w := perfWorkloads()[0]

	var rows []string
	for _, n := range cfg.conns {
		tag := perfTag(w.name, "audit", n)
		r := runLoad(t, cfg, "proxied", proxiedDSN, n, w, tag)

		persisted := waitForAuditDrain(t, s, tag)
		lost := r.issued - persisted
		pct := float64(lost) / float64(r.issued) * 100

		if persisted == 0 {
			t.Errorf("%d connections: ran %d statements and the audit log has none of them — "+
				"the pipeline is broken, not merely behind", n, r.issued)
		}
		t.Logf("%2d conns: issued=%d persisted=%d lost=%d (%.2f%%) at %.0f q/s",
			n, r.issued, persisted, lost, pct, r.qps())
		rows = append(rows, fmt.Sprintf("| %d | %d | %d | %d | %.2f%% | %.0f |",
			n, r.issued, persisted, lost, pct, r.qps()))
	}

	t.Log("\n" + strings.Join(append([]string{
		"",
		"| conns | statements run | in audit log | lost | loss | offered q/s |",
		"|---|---|---|---|---|---|",
	}, rows...), "\n"))
}

// waitForAuditDrain polls until the count of audited statements carrying tag
// stops rising, and returns it.
//
// Polling to stability rather than sleeping a fixed time: the sink flushes on
// a batch or a timer and the API writes to SQLite, so how long the tail takes
// depends on how far behind it got. A fixed sleep would either cut the tail
// off -- reporting loss that was really just lag -- or pad every run.
func waitForAuditDrain(t *testing.T, s *stack, tag string) int {
	t.Helper()

	var last, stable int
	deadline := time.Now().Add(auditDrainTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)

		var resp struct {
			Total int `json:"total"`
		}
		// limit=1: only the pre-pagination total is wanted, and pulling the
		// rows themselves would be tens of thousands of statements of SQL.
		s.getJSON(t, "/api/statements?limit=1&q="+tag, &resp)

		if resp.Total == last {
			stable++
			// Three quiet polls: one is easily a gap between two batches.
			if stable >= 3 {
				return resp.Total
			}
			continue
		}
		last, stable = resp.Total, 0
	}
	t.Logf("audit log still growing after %s; reporting the last count seen", auditDrainTimeout)
	return last
}

// ---------------------------------------------------------------------------
// Phase 3 -- what a connection costs, and where the pipeline gives out
// ---------------------------------------------------------------------------

// perfHandshakes is how many connections each path opens and closes, and how
// many of those are discarded first.
//
// 200 rather than a handful because the two paths turned out to sit within
// tens of microseconds of each other at the median, which a small sample
// cannot tell apart from noise.
const (
	perfHandshakes    = 200
	perfHandshakeWarm = 10
)

// TestPerfConnectionCost measures establishing a session, not running queries
// on one.
//
// Worth its own measurement because a later release made it much more expensive. The
// proxy now terminates TLS and offers caching_sha2_password (MySQL 8's
// default) where the first version offered mysql_native_password in the clear, so every
// connection pays for a TLS handshake and an RSA-backed exchange that did not
// exist before. Steady-state query latency hides that entirely: it is paid
// once per session, which is invisible to a long-lived client and significant
// to anything that connects per request.
func TestPerfConnectionCost(t *testing.T) {
	requirePerf(t)
	a, _ := upstreams(t)
	seedUpstream(t, a)

	s := startStackWith(t, stackOpts{}, a)
	paths := []struct{ name, dsn string }{
		{"direct", fmt.Sprintf("%s:%s@tcp(%s)/sales?timeout=10s", a.user, a.pass, a.addr)},
		{"proxied", fmt.Sprintf("alice:%s@tcp(%s)/sales?timeout=10s&tls=skip-verify", alicePass(t), s.proxyAddrs[a.name])},
	}

	var rows []string
	for _, p := range paths {
		samples := make([]time.Duration, 0, perfHandshakes)
		for i := 0; i < perfHandshakes+perfHandshakeWarm; i++ {
			began := time.Now()
			db, err := sql.Open("mysql", p.dsn)
			if err != nil {
				t.Fatalf("%s: open: %v", p.name, err)
			}
			db.SetMaxOpenConns(1)
			// sql.Open is lazy, so Ping is what actually pays for the TCP
			// connect, the TLS handshake and authentication.
			if err := db.Ping(); err != nil {
				db.Close()
				t.Fatalf("%s: connecting: %v", p.name, err)
			}
			took := time.Since(began)
			db.Close()
			// The first few connections on each path pay one-off costs -- the
			// upstream's auth caches are cold, and on the direct path the Go
			// driver fetches the server's RSA public key before it can
			// complete caching_sha2_password.
			if i >= perfHandshakeWarm {
				samples = append(samples, took)
			}
		}

		var r loadResult
		summarise(&r, samples)
		t.Logf("%-7s connect: p50=%s p95=%s p99=%s max=%s over %d handshakes",
			p.name, round(r.p50), round(r.p95), round(r.p99), round(r.max), len(samples))
		rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s | %s |",
			p.name, round(r.p50), round(r.p95), round(r.p99), round(r.max)))
	}

	t.Log("\n" + strings.Join(append([]string{
		"",
		"| path | p50 | p95 | p99 | max |",
		"|---|---|---|---|---|",
	}, rows...), "\n"))
}

// TestPerfAuditCeiling finds the offered rate at which the audit trail starts
// losing records.
//
// TestPerfAuditCompleteness answers "is it lossless at the load the client
// described"; this answers "how much headroom is there before it isn't". The
// gap between the two is the number worth quoting, because it says how far the
// system can be pushed past the specification before its guarantee changes.
//
// Rates are per connection, so the offered total is rate x connections. 0
// means unpaced: drive as hard as the client can, which is well past anything
// a person would do and is here to establish the far end of the curve.
func TestPerfAuditCeiling(t *testing.T) {
	requirePerf(t)
	a, _ := upstreams(t)
	seedUpstream(t, a)
	seedPerfRows(t, a)

	cfg := perfConfigFromEnv(t)
	// A shorter window than the other tests on purpose: unpaced, fifty
	// connections put tens of thousands of statements a second into SQLite,
	// and the run has to stay somewhere near the length of a coffee break.
	cfg.duration = perfDuration(t, "PERF_CEILING_DURATION", 5*time.Second)

	conns := perfInt(t, "PERF_CEILING_CONNS", 50)
	rates := perfRates(t, "PERF_CEILING_RATES", []int{50, 250, 1000, 0})
	modes := perfModes(t, "PERF_SINK_MODES", []string{"block", "drop"})
	w := perfWorkloads()[0]

	// The unproxied rate at the same offered load: what the database itself
	// can do, so "the proxy slowed it down" is not confused with "the load
	// was never achievable".
	directDSN := fmt.Sprintf("%s:%s@tcp(%s)/sales?timeout=10s", a.user, a.pass, a.addr)
	var direct []string
	for _, rate := range rates {
		run := cfg
		run.interval = 0
		if rate > 0 {
			run.interval = time.Second / time.Duration(rate)
		}
		r := runLoad(t, run, "direct", directDSN, conns, w, perfTag(w.name, "ceiling-direct", rate))
		direct = append(direct, fmt.Sprintf("| %s | %.0f | %s | %s | %s | %s |",
			offeredLabel(rate, conns), r.qps(), round(r.p50), round(r.p95), round(r.p99), round(r.max)))
	}

	var rows []string
	for _, mode := range modes {
		// A fresh stack per mode: the mode is a startup setting, and a fresh
		// API database keeps one mode's backlog out of the next one's counts.
		s := startStackWith(t, stackOpts{sinkOnFull: mode}, a)
		proxiedDSN := fmt.Sprintf("alice:%s@tcp(%s)/sales?timeout=10s&tls=skip-verify", alicePass(t), s.proxyAddrs[a.name])

		for _, rate := range rates {
			run := cfg
			run.interval = 0
			if rate > 0 {
				run.interval = time.Second / time.Duration(rate)
			}

			tag := perfTag(w.name, "ceiling-"+mode, rate)
			r := runLoad(t, run, "proxied", proxiedDSN, conns, w, tag)
			persisted := waitForAuditDrain(t, s, tag)
			lost := r.issued - persisted
			pct := float64(lost) / float64(r.issued) * 100

			t.Logf("mode=%s rate=%-7s achieved=%.0f q/s p50=%s p95=%s p99=%s max=%s issued=%d persisted=%d lost=%d (%.2f%%) behind=%d",
				mode, offeredLabel(rate, conns), r.qps(), round(r.p50), round(r.p95), round(r.p99), round(r.max),
				r.issued, persisted, lost, pct, r.behind)
			rows = append(rows, fmt.Sprintf("| %s | %s | %.0f | %s | %s | %s | %s | %d | %d | %.2f%% |",
				mode, offeredLabel(rate, conns), r.qps(),
				round(r.p50), round(r.p95), round(r.p99), round(r.max),
				r.issued, persisted, pct))
		}
	}

	t.Log("\n" + strings.Join(append([]string{
		"",
		fmt.Sprintf("At %d concurrent connections, proxied:", conns),
		"",
		"| sink on full | offered q/s | achieved q/s | p50 | p95 | p99 | max | statements run | in audit log | loss |",
		"|---|---|---|---|---|---|---|---|---|---|",
	}, rows...), "\n"))
	t.Log("\n" + strings.Join(append([]string{
		"",
		fmt.Sprintf("Same load, direct to the upstream (%d connections):", conns),
		"",
		"| offered q/s | achieved q/s | p50 | p95 | p99 | max |",
		"|---|---|---|---|---|---|",
	}, direct...), "\n"))
}

// offeredLabel is the total offered rate for a per-connection rate, or
// "unpaced".
func offeredLabel(rate, conns int) string {
	if rate <= 0 {
		return "unpaced"
	}
	return strconv.Itoa(rate * conns)
}

// perfModes parses a comma-separated list of SINK_ON_FULL values.
func perfModes(t *testing.T, key string, fallback []string) []string {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	var out []string
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f != "block" && f != "drop" {
			t.Fatalf("%s=%q: want a comma-separated list of block and drop", key, raw)
		}
		out = append(out, f)
	}
	return out
}

// perfRates parses a comma-separated list of per-connection query rates.
//
// Separate from perfConns because 0 is meaningful here -- it means "unpaced",
// drive as hard as the client can -- whereas a connection count of 0 is a
// mistake worth rejecting.
func perfRates(t *testing.T, key string, fallback []int) []int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	var out []int
	for _, f := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 0 {
			t.Fatalf("%s=%q: want a comma-separated list of non-negative integers (0 = unpaced)", key, raw)
		}
		out = append(out, n)
	}
	return out
}
