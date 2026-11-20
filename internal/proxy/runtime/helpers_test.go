package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/proxy/config"
)

// ---------------------------------------------------------------------------
// testSink — records emitted events so tests can inspect them.
// ---------------------------------------------------------------------------

type testSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *testSink) Emit(e audit.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *testSink) getEvents() []audit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]audit.Event, len(s.events))
	copy(cp, s.events)
	return cp
}

// waitForEvent polls the sink until an event of the given type appears or
// the timeout is reached. Returns true if the event was found.
func (s *testSink) waitForEvent(t string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range s.getEvents() {
			if e.Kind == t {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// ---------------------------------------------------------------------------
// Integration tests — require a running MySQL instance.
//
// These tests are skipped if PROXY_TEST_UPSTREAM_ADDR is not set (or the
// upstream is unreachable). When the Docker Compose stack from
// deploy/docker-compose.yml is running, set:
//
//   PROXY_TEST_UPSTREAM_ADDR=127.0.0.1:3306
//   PROXY_TEST_UPSTREAM_USER=root
//   PROXY_TEST_UPSTREAM_PASS=rootpass
//
// or rely on the defaults which match the compose file.
// ---------------------------------------------------------------------------

// testUpstreamConfig returns the upstream address, user, and password for
// integration tests. It skips the test if the upstream is not reachable, or
// fails it when PROXY_TEST_REQUIRE_UPSTREAM is set (see helpers_test.go).
func testUpstreamConfig(t *testing.T) (addr, user, pass string) {
	t.Helper()
	addr = config.GetEnv("PROXY_TEST_UPSTREAM_ADDR", "127.0.0.1:3306")
	user = config.GetEnv("PROXY_TEST_UPSTREAM_USER", "root")
	pass = config.GetEnv("PROXY_TEST_UPSTREAM_PASS", "rootpass")

	// Quick connectivity check — skip if the upstream is not running.
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		upstreamUnreachable(t, true, "upstream MySQL not reachable at %s (set PROXY_TEST_UPSTREAM_ADDR to run integration tests): %v", addr, err)
	}
	c.Close()
	return
}

// ---------------------------------------------------------------------------
// testLogger — forwards proxy logs to t.Log safely.
// ---------------------------------------------------------------------------

type testLogger struct {
	t    *testing.T
	done *atomic.Bool
}

func (tl testLogger) Write(p []byte) (n int, err error) {
	if !tl.done.Load() {
		tl.t.Logf("PROXY: %s", strings.TrimSpace(string(p)))
	}
	return len(p), nil
}

// testUpstreamBConfig is testUpstreamConfig's counterpart for the second
// upstream, matching db-b in deploy/docker-compose.yml — a different root
// password on purpose, per deploy/README.md, so a test that reaches it with
// the wrong credentials fails to connect rather than passing by accident.
// Skips (rather than failing) if db-b is not reachable, same as
// testUpstreamConfig, so the single-upstream tests above still run against
// just db-a.
func testUpstreamBConfig(t *testing.T) (addr, user, pass string) {
	t.Helper()
	addr = config.GetEnv("PROXY_TEST_UPSTREAM_B_ADDR", "127.0.0.1:3316")
	user = config.GetEnv("PROXY_TEST_UPSTREAM_B_USER", "root")
	pass = config.GetEnv("PROXY_TEST_UPSTREAM_B_PASS", "rootpass-b")

	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		upstreamUnreachable(t, false, "second upstream MySQL not reachable at %s (run `make test-env`, or set PROXY_TEST_UPSTREAM_B_ADDR): %v", addr, err)
	}
	c.Close()
	return
}

// freePort asks the OS for an unused port and releases it, for tests that
// need a real port number up front (BuildProxies bakes ListenPort into
// ListenAddr, unlike startTestProxy's "127.0.0.1:0"). There is a race between
// releasing it and the proxy binding it, but the window has not been observed
// to matter in practice — see e2e/harness_test.go's identical helper.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitTCPReachable blocks until addr accepts a connection or the timeout
// elapses, at which point it fails the test.
func waitTCPReachable(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never became reachable within %s", addr, timeout)
}

// seedRoutingMarker connects directly to addr (bypassing any proxy) and
// records name as that server's identity, the same technique
// e2e/harness_test.go uses: whichever server a query comes back from names
// itself, so a client reading the wrong one is caught by content, not just by
// which port it happened to dial. Idempotent — safe to call against an
// already-seeded upstream.
func seedRoutingMarker(t *testing.T, addr, user, pass, name string) {
	t.Helper()
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s", user, pass, addr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("connecting to %s to seed marker: %v", addr, err)
	}
	defer db.Close()

	stmts := []string{
		"CREATE DATABASE IF NOT EXISTS auditproxy_test",
		"CREATE TABLE IF NOT EXISTS auditproxy_test.upstream_marker (name VARCHAR(32) NOT NULL PRIMARY KEY)",
		fmt.Sprintf("INSERT IGNORE INTO auditproxy_test.upstream_marker (name) VALUES (%q)", name),
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seeding marker on %s (%s): %v", addr, q, err)
		}
	}
}

// startTestProxy starts a proxy on a random port and returns its address and
// sink. The proxy runs in a background goroutine.
func startTestProxy(t *testing.T, upstreamAddr, upstreamUser, upstreamPass string, opts ...func(*Proxy)) (proxyAddr string, sink *testSink) {
	t.Helper()
	sink = &testSink{}
	ready := make(chan net.Listener, 1)

	done := &atomic.Bool{}
	t.Cleanup(func() { done.Store(true) })

	proxy := &Proxy{
		TLS:          config.GetEnv("PROXY_TEST_TLS", "skip-verify"),
		TLSCAFile:    os.Getenv("PROXY_TEST_TLS_CA_FILE"),
		ListenAddr:   "127.0.0.1:0", // OS picks a free port
		UpstreamAddr: upstreamAddr,
		UpstreamName: "test-db",
		UpstreamUser: upstreamUser,
		UpstreamPass: upstreamPass,
		Sink:         sink,
		Logger:       log.New(testLogger{t, done}, "", 0),
	}

	for _, opt := range opts {
		opt(proxy)
	}

	// Cancelling ctx on cleanup closes the listener (see Proxy.Run), so each
	// test's proxy actually stops instead of leaking its accept-loop
	// goroutine for the life of the whole test binary.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		if err := proxy.Run(ctx, ready); err != nil && ctx.Err() == nil {
			t.Logf("proxy.Run returned: %v", err)
		}
	}()

	listener := <-ready
	proxyAddr = listener.Addr().String()
	return
}

// portsOf is a small formatting helper for TestMultiUpstreamRouting's
// failure messages.
func portsOf(events []audit.Event) []int {
	ports := make([]int, len(events))
	for i, e := range events {
		ports[i] = e.ProxyPort
	}
	return ports
}

// requireUpstreamEnv makes an unreachable primary upstream a failure instead
// of a skip. Unset, the integration tests skip so `go test ./...` stays usable
// without Docker; `make test` and the CI test job set it, so a MySQL that
// failed to start turns the run red rather than quietly testing nothing.
//
// Only the primary upstream is strict. The second one (db-b) is skipped when
// absent even then: the CI test job runs a single MySQL service, and the
// two-upstream routing is covered by the e2e job.
const requireUpstreamEnv = "PROXY_TEST_REQUIRE_UPSTREAM"

// skippedUpstreamTests counts integration tests skipped for want of an
// upstream, so TestMain can say so once instead of the reason being buried in
// a skip line per test. The warning goes to stderr, which `go test` shows when
// run in the package directory or with -v but hides for a passing package in
// package-list mode (`go test ./...`) — that is what PROXY_TEST_REQUIRE_UPSTREAM
// in `make test` and CI is for.
var skippedUpstreamTests atomic.Int64

// upstreamUnreachable ends t because its upstream could not be reached:
// failing under PROXY_TEST_REQUIRE_UPSTREAM when strict is true, skipping
// (and counting the skip for the end-of-run warning) otherwise.
func upstreamUnreachable(t *testing.T, strict bool, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if strict && os.Getenv(requireUpstreamEnv) != "" {
		t.Fatalf("%s is set: %s", requireUpstreamEnv, msg)
	}
	skippedUpstreamTests.Add(1)
	t.Skip(msg)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if n := skippedUpstreamTests.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "warning: upstream MySQL unreachable, %d proxy integration test(s) skipped "+
			"(run `make test-env`, or set %s=1 to fail instead)\n", n, requireUpstreamEnv)
	}
	os.Exit(code)
}

// upstreamAdmin opens a direct, non-proxied connection, used to observe real
// upstream state and to KILL the proxy's upstream connections.
func upstreamAdmin(t *testing.T, addr, user, pass string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/demo?timeout=5s", user, pass, addr))
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping admin connection: %v", err)
	}
	return db
}

// proxyDSN builds a DSN pointing at the proxy, on the demo database. It asks
// for TLS (without checking the proxy's self-signed certificate): the proxy
// relays the database's login challenge to the client and refuses to do that
// over a plaintext connection.
func proxyDSN(user, pass, proxyAddr string) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/demo?timeout=5s&tls=skip-verify", user, pass, proxyAddr)
}
