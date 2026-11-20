// Package e2e drives the built binaries the way the compose stack does: two
// upstream databases, a proxy in front of each, and the API collecting what
// they emit.
//
// It runs the real `proxy` and `api` executables as subprocesses rather than
// wiring their packages together in-process. The difference matters — the
// endpoint serving the port mapping returned an empty list for the whole of
// the first version because nothing supplied its config file, and no in-process test
// could have noticed, since the gap was in main's environment handling rather
// than in any package it calls.
//
// Required environment, defaulting to deploy/docker-compose.yml:
//
//	E2E_DB_A_ADDR=127.0.0.1:3306  E2E_DB_A_USER=root  E2E_DB_A_PASS=rootpass
//	E2E_DB_B_ADDR=127.0.0.1:3316  E2E_DB_B_USER=root  E2E_DB_B_PASS=rootpass-b
//
// Both upstreams must be reachable. The tests skip when they are not, so the
// suite still passes on a machine with nothing running — but a skip covers
// nothing, so set E2E_REQUIRE=1 (as CI does) to turn an unreachable upstream
// into a failure instead.
package e2e

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
)

// upstream describes one of the two databases under test.
type upstream struct {
	name string // the audit log's `upstream` value
	addr string
	user string
	pass string
	// directParams is extra DSN parameters for the root connections the tests
	// open themselves (seeding, creating accounts), e.g. "tls=skip-verify".
	directParams string

	// ownTable is the qualified table this upstream has and the other does not.
	// It is what makes a misrouted query fail rather than return plausible rows.
	ownTable string
	// seed creates ownTable. Idempotent, because it runs against both an
	// already-seeded compose stack and a bare CI service container.
	seed []string
}

// extraParams is directParams ready to append to a DSN's query string.
func (u upstream) extraParams() string {
	if u.directParams == "" {
		return ""
	}
	return "&" + u.directParams
}

// startupTimeout bounds waiting for a subprocess to begin listening.
const startupTimeout = 30 * time.Second

// auditTimeout bounds waiting for an event to travel proxy -> sink -> API ->
// store. The sink flushes a partial batch every second by default, so this is
// mostly slack for a loaded CI runner.
const auditTimeout = 30 * time.Second

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// upstreams returns the two databases under test, skipping (or failing, under
// E2E_REQUIRE) when either is unreachable.
func upstreams(t *testing.T) (a, b upstream) {
	t.Helper()

	// The schemas mirror deploy/initdb. They are restated here rather than read
	// from those files because CI attaches bare MySQL service containers, with
	// nowhere to mount an init script — the test has to be able to establish
	// what it asserts on.
	a = upstream{
		name:     "db-a",
		addr:     getEnv("E2E_DB_A_ADDR", "127.0.0.1:3306"),
		user:     getEnv("E2E_DB_A_USER", "root"),
		pass:     getEnv("E2E_DB_A_PASS", "rootpass"),
		ownTable: "sales.customers",
		seed: []string{
			"CREATE DATABASE IF NOT EXISTS sales",
			"CREATE TABLE IF NOT EXISTS sales.customers (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(100) NOT NULL)",
		},
	}
	b = upstream{
		name:     "db-b",
		addr:     getEnv("E2E_DB_B_ADDR", "127.0.0.1:3316"),
		user:     getEnv("E2E_DB_B_USER", "root"),
		pass:     getEnv("E2E_DB_B_PASS", "rootpass-b"),
		ownTable: "hr.employees",
		seed: []string{
			"CREATE DATABASE IF NOT EXISTS hr",
			"CREATE TABLE IF NOT EXISTS hr.employees (id INT AUTO_INCREMENT PRIMARY KEY, full_name VARCHAR(100) NOT NULL)",
		},
	}

	for _, u := range []upstream{a, b} {
		conn, err := net.DialTimeout("tcp", u.addr, 3*time.Second)
		if err != nil {
			msg := fmt.Sprintf("upstream %s not reachable at %s (run `make test-env`): %v", u.name, u.addr, err)
			if os.Getenv("E2E_REQUIRE") != "" {
				// A silent skip in CI looks identical to a passing run.
				t.Fatalf("E2E_REQUIRE is set: %s", msg)
			}
			t.Skip(msg)
		}
		conn.Close()
	}

	// Pass-through authentication: the proxy logs in as the bootstrap account
	// and every client is a real database user, so those accounts have to exist
	// before any test connects through a proxy.
	for _, u := range []upstream{a, b} {
		ensureAccounts(t, u, accountsFor(t, u))
	}
	return a, b
}

var (
	portMu   sync.Mutex
	portUsed = map[int]bool{}
)

// freePort asks the OS for an unused port and releases it. It never returns
// a port it has already handed out in this process, because released ports
// are not bound until the stack starts and could otherwise be reissued.
func freePort(t *testing.T) int {
	t.Helper()
	portMu.Lock()
	defer portMu.Unlock()
	for i := 0; i < 100; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserving a port: %v", err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if !portUsed[p] {
			portUsed[p] = true
			return p
		}
	}
	t.Fatalf("no unused port after 100 attempts")
	return 0
}

// buildOnce, builtProxyBin, builtAPIBin and buildErr back buildBinaries: the
// binaries are compiled exactly once for the whole `go test` process, not
// once per test. Every stack-starting test (startStack, startStackWith,
// startLiveStack) used to call buildBinaries itself, each triggering its own
// `go build` — harmless on a fast machine, but on a slow or contended CI
// runner those repeated builds compound and can blow through go test's
// default 10-minute timeout before the first test even finishes. The built
// binaries deliberately live outside any single test's t.TempDir(), which is
// removed as soon as that test ends — reusing them across later tests would
// otherwise fail once the original test's cleanup deleted the directory.
var (
	buildOnce     sync.Once
	builtProxyBin string
	builtAPIBin   string
	buildErr      error
)

// buildBinaries compiles the proxy and API once for the whole test run (see
// buildOnce above), and hands back the same paths to every caller.
func buildBinaries(t *testing.T) (proxyBin, apiBin string) {
	t.Helper()

	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "auditproxy-e2e-bin-*")
		if err != nil {
			buildErr = fmt.Errorf("creating build dir: %w", err)
			return
		}
		builtProxyBin = filepath.Join(dir, "proxy")
		builtAPIBin = filepath.Join(dir, "api")

		for _, b := range []struct{ out, pkg string }{
			{builtProxyBin, "./cmd/proxy"},
			{builtAPIBin, "./cmd/api"},
		} {
			cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
			cmd.Dir = ".." // the module root
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("building %s: %w\n%s", b.pkg, err, out)
				return
			}
		}
	})

	if buildErr != nil {
		t.Fatalf("building e2e binaries: %v", buildErr)
	}
	return builtProxyBin, builtAPIBin
}

// procLog collects a subprocess's output so a test can look for something in it
// while the process is still running, not only read it back when the test ends.
type procLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *procLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String returns everything the process has written so far.
func (l *procLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// start runs bin with env, streams its output into the test log, and kills it
// when the test ends. The returned log holds the output so far at any moment.
func start(t *testing.T, name, bin string, env []string) *procLog {
	t.Helper()

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), env...)

	out, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("%s: stderr pipe: %v", name, err)
	}
	cmd.Stdout = cmd.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("%s: start: %v", name, err)
	}

	// Both binaries log to stderr. Surfacing it under -v turns "the test timed
	// out" into "the proxy could not reach its upstream".
	captured := &procLog{}
	go func() {
		_, _ = io.Copy(captured, out)
		if b := captured.String(); len(b) > 0 {
			t.Logf("--- %s ---\n%s", name, b)
		}
	}()

	t.Cleanup(func() {
		// The proxy has no graceful shutdown path, so there is nothing gentler
		// to ask for. The API flushes on SIGTERM, but every assertion here has
		// already read back through its HTTP API by this point.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return captured
}

// waitTCP blocks until addr accepts a connection.
func waitTCP(t *testing.T, what, addr string) {
	t.Helper()
	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never listened on %s within %s", what, addr, startupTimeout)
}

// waitHealthz blocks until the API reports healthy.
func waitHealthz(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(startupTimeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("API never became healthy at %s within %s", baseURL, startupTimeout)
}

// stack is a running system: one API and a single proxy process opening one
// listener per upstream. The harness provisions the servers through the API
// (POST /api/servers) and the proxy loads them from GET /api/proxy/config at
// startup — the target architecture, not the one-process-per-upstream
// scaffolding it replaced. PROXY_CONFIG_FILE is not used here.
type stack struct {
	apiURL     string
	token      string
	proxyAddrs map[string]string // upstream name -> proxy listen address
	proxyPorts map[string]int
	proxyLog   *procLog // the proxy's output so far
	adminURL   string   // the proxy's admin server (healthz, metrics)
}

// stackOpts tunes the stack the tests bring up.
//
// It exists for the audit sink's batching. The functional tests drive it down
// to one event per flush so an assertion does not wait a second for a batch to
// close; a load test must not inherit that, because one HTTP POST per event
// measures the harness rather than the system. Zero leaves the proxy's own
// default in place (batch 50, flush 1s — see internal/sink.New).
type stackOpts struct {
	sinkBatchSize     int
	sinkFlushInterval time.Duration
	// bootstrapPass overrides the password the proxy is configured with for
	// its own bootstrap login, so a test can bring up a proxy whose bootstrap
	// credential is wrong. Empty means the real one.
	bootstrapPass string
	// sinkOnFull sets SINK_ON_FULL ("block" or "drop"). Empty means the
	// proxy's default.
	sinkOnFull string
}

// startStack brings up the API and the proxy with the settings the functional
// tests want: a sink that flushes almost immediately, so an assertion that
// reads the audit log back does not have to wait for a batch.
func startStack(t *testing.T, ups ...upstream) *stack {
	t.Helper()
	return startStackWith(t, stackOpts{
		sinkBatchSize:     1,
		sinkFlushInterval: 200 * time.Millisecond,
	}, ups...)
}

// startStackWith brings up the API and the proxy, and waits for all of
// them to be serving.
func startStackWith(t *testing.T, opts stackOpts, ups ...upstream) *stack {
	t.Helper()
	proxyBin, apiBin := buildBinaries(t)
	dir := t.TempDir()

	s := &stack{
		proxyAddrs: map[string]string{},
		proxyPorts: map[string]int{},
	}

	// The API's own view of the mapping, matching ServerConfigReq.
	type server struct {
		TLS          string `json:"tls"`
		TLSCAFile    string `json:"tls_ca_file"`
		ID           int    `json:"id"`
		Name         string `json:"name"`
		ListenPort   int    `json:"listen_port"`
		UpstreamHost string `json:"upstream_host"`
		UpstreamPort int    `json:"upstream_port"`
		UpstreamUser string `json:"upstream_user"`
		UpstreamPass string `json:"upstream_pass"`
		Enabled      bool   `json:"enabled"`
	}
	var cfg struct {
		Servers []server `json:"servers"`
	}

	for i, u := range ups {
		port := freePort(t)
		s.proxyPorts[u.name] = port
		s.proxyAddrs[u.name] = fmt.Sprintf("127.0.0.1:%d", port)

		host, upPort, err := net.SplitHostPort(u.addr)
		if err != nil {
			t.Fatalf("upstream %s address %q: %v", u.name, u.addr, err)
		}
		var upPortNum int
		fmt.Sscanf(upPort, "%d", &upPortNum)

		bootPass := opts.bootstrapPass
		if bootPass == "" {
			bootPass = bootstrapPass(t, u)
		}
		cfg.Servers = append(cfg.Servers, server{
			TLS:       getEnv("E2E_TLS", "skip-verify"),
			TLSCAFile: os.Getenv("E2E_TLS_CA_FILE"),
			ID:        i + 1, Name: u.name, ListenPort: port,
			UpstreamHost: host, UpstreamPort: upPortNum,
			UpstreamUser: bootstrapUser, UpstreamPass: bootPass, Enabled: true,
		})
	}

	apiPort := freePort(t)
	s.apiURL = fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	start(t, "api", apiBin, []string{
		fmt.Sprintf("API_LISTEN_ADDR=127.0.0.1:%d", apiPort),
		"API_DB_PATH=" + filepath.Join(dir, "auditproxy.db"),
		"API_ADMIN_PASS_HASH=$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW",
		"API_CONFIG_KEY=0123456789abcdef0123456789abcdef",
		"API_PROXY_TOKEN=e2e-secret-token-123",
	})
	waitHealthz(t, s.apiURL)

	loginBody, _ := json.Marshal(audit.LoginRequest{Username: "admin", Password: "admin123"})
	loginResp, err := http.Post(s.apiURL+"/api/login", "application/json", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("e2e login request failed: %v", err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(loginResp.Body)
		t.Fatalf("e2e login failed with status %d: %s", loginResp.StatusCode, body)
	}

	// Extract the session cookie from the login response
	for _, c := range loginResp.Cookies() {
		if c.Name == "session" {
			s.token = c.Value
		}
	}
	if s.token == "" {
		t.Fatalf("e2e login failed to set session cookie")
	}

	// Provision servers dynamically via the API using cookies and CSRF headers
	for _, srv := range cfg.Servers {
		body, _ := json.Marshal(srv)
		req, _ := http.NewRequest(http.MethodPost, s.apiURL+"/api/servers", bytes.NewReader(body))
		req.AddCookie(&http.Cookie{Name: "session", Value: s.token})
		req.Header.Set("X-Requested-With", "XMLHttpRequest")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("provisioning server %s: %v", srv.Name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("provisioning server %s returned status %d", srv.Name, resp.StatusCode)
		}
	}

	// Start a single proxy process that manages all enabled servers
	adminPort := freePort(t)
	s.adminURL = fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	proxyEnv := []string{
		fmt.Sprintf("PROXY_ADMIN_ADDR=127.0.0.1:%d", adminPort),
		"API_URL=" + s.apiURL,
		"API_ADMIN_USER=admin",
		"API_ADMIN_PASS=admin123",
		"API_PROXY_TOKEN=e2e-secret-token-123",
		"SINK_INGEST_URL=" + s.apiURL + "/api/events",
	}
	// Unset means "use the proxy's default", which is what a load test wants.
	if opts.sinkBatchSize > 0 {
		proxyEnv = append(proxyEnv, fmt.Sprintf("SINK_BATCH_SIZE=%d", opts.sinkBatchSize))
	}
	if opts.sinkFlushInterval > 0 {
		proxyEnv = append(proxyEnv, "SINK_FLUSH_INTERVAL="+opts.sinkFlushInterval.String())
	}
	if opts.sinkOnFull != "" {
		proxyEnv = append(proxyEnv, "SINK_ON_FULL="+opts.sinkOnFull)
	}
	s.proxyLog = start(t, "proxy-manager", proxyBin, proxyEnv)

	for _, u := range ups {
		waitTCP(t, "proxy-"+u.name, s.proxyAddrs[u.name])
	}

	return s
}

// openThroughProxy connects to the named upstream's proxy as user.
func (s *stack) openThroughProxy(t *testing.T, upstreamName, user, pass, database string) *sql.DB {
	t.Helper()
	// The client leg must be TLS (the proxy relays the database's own
	// authentication, which may need the password in the clear); the proxy's
	// certificate is self-signed, hence skip-verify.
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?timeout=10s&tls=skip-verify", user, pass, s.proxyAddrs[upstreamName], database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("opening %s through its proxy: %v", upstreamName, err)
	}
	// One connection: sql.DB is a pool, and a second one would appear in the
	// audit log as a second session.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// getJSON fetches path from the API using cookies and decodes it into out.
func (s *stack) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	req, err := http.NewRequest("GET", s.apiURL+path, nil)
	if err != nil {
		t.Fatalf("creating GET %s: %v", path, err)
	}
	req.AddCookie(&http.Cookie{Name: "session", Value: s.token})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %d: %s", path, resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

// markerNames returns the routing markers an upstream carries, or nil if it has
// never been seeded. A missing database or table means "not seeded", not an
// error; anything else is reported.
func markerNames(db *sql.DB) ([]string, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'auditproxy_test' AND table_name = 'upstream_marker'`).Scan(&n)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}

	rows, err := db.Query("SELECT name FROM auditproxy_test.upstream_marker ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// seedUpstream makes sure the upstream carries the routing marker naming it and
// the schema that distinguishes it from the other one.
//
// deploy/initdb does this for the compose stack; this is the same thing for an
// environment where nothing can be mounted. Idempotent, so it is a no-op
// against an already-seeded stack.
//
// It identifies the server BEFORE writing anything. Seeding first and checking
// afterwards leaves a marker naming the wrong server behind on the way to the
// failure — which corrupts the environment every other test reads, and turns
// the routing check into a coin flip over which row comes back first.
func seedUpstream(t *testing.T, u upstream) {
	t.Helper()
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=10s&multiStatements=true%s", u.user, u.pass, u.addr, u.extraParams())
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("connecting to %s: %v", u.name, err)
	}
	defer db.Close()

	existing, err := markerNames(db)
	if err != nil {
		t.Fatalf("reading %s marker: %v", u.name, err)
	}
	switch {
	case len(existing) == 0:
		// Never seeded — safe to write.
	case len(existing) == 1 && existing[0] == u.name:
		// Already correct.
	default:
		t.Fatalf("%s at %s identifies itself as %v — refusing to seed it. "+
			"Either the two upstream addresses are crossed, or a previous run "+
			"seeded this server under another name",
			u.name, u.addr, existing)
	}

	stmts := append([]string{
		"CREATE DATABASE IF NOT EXISTS auditproxy_test",
		"CREATE TABLE IF NOT EXISTS auditproxy_test.upstream_marker (name VARCHAR(32) NOT NULL PRIMARY KEY)",
		fmt.Sprintf("INSERT IGNORE INTO auditproxy_test.upstream_marker (name) VALUES (%q)", u.name),
	}, u.seed...)
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seeding %s (%s): %v", u.name, q, err)
		}
	}

	// Exactly one row: the marker only identifies a server if it names one.
	after, err := markerNames(db)
	if err != nil {
		t.Fatalf("reading %s marker: %v", u.name, err)
	}
	if len(after) != 1 || after[0] != u.name {
		t.Fatalf("%s at %s carries markers %v, want exactly [%s]", u.name, u.addr, after, u.name)
	}
}

// waitForStatement polls until a statement whose SQL contains substr appears in
// the audit log.
//
// Polling rather than sleeping: the path from proxy to a readable row runs
// through a batching sink and an HTTP round trip, so the latency is real but
// variable, and a fixed sleep would be either flaky or slow.
func (s *stack) waitForStatement(t *testing.T, substr string) audit.Statement {
	t.Helper()
	deadline := time.Now().Add(auditTimeout)

	for time.Now().Before(deadline) {
		var resp struct {
			Statements []audit.Statement `json:"statements"`
		}
		s.getJSON(t, "/api/statements", &resp)

		for _, st := range resp.Statements {
			if strings.Contains(st.SQL, substr) {
				return st
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("no statement containing %q reached the audit log within %s", substr, auditTimeout)
	return audit.Statement{}
}

// pingAs logs in to the named upstream's proxy as user and returns the login's
// error, without failing the test: the caller is usually asserting on a refusal.
// It uses its own pool, so a refused login leaves nothing behind.
func (s *stack) pingAs(t *testing.T, upstreamName, user, pass string) error {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/?timeout=10s&tls=skip-verify", user, pass, s.proxyAddrs[upstreamName]))
	if err != nil {
		t.Fatalf("opening %s through its proxy: %v", upstreamName, err)
	}
	defer db.Close()
	return db.Ping()
}

// createTempUser creates a database account directly on u with the given grants
// (each a GRANT statement with a %s where the user goes), and drops it when the
// test ends. The shared accounts (alice, bob) are never touched by a test that
// changes grants: other tests are logging in as them.
func createTempUser(t *testing.T, u upstream, prefix string, grants ...string) (user, pass string) {
	t.Helper()
	user = fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()%1_000_000_000)
	pass = fmt.Sprintf("pw-%d", time.Now().UnixNano())

	root := openDirect(t, u, u.user, u.pass)
	defer root.Close()
	if _, err := root.Exec(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", user, pass)); err != nil {
		t.Fatalf("creating %s on %s: %v", user, u.name, err)
	}
	t.Cleanup(func() {
		r := openDirect(t, u, u.user, u.pass)
		defer r.Close()
		_, _ = r.Exec(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", user))
	})
	for _, g := range grants {
		if _, err := root.Exec(fmt.Sprintf(g, "'"+user+"'@'%'")); err != nil {
			t.Fatalf("granting to %s: %v", user, err)
		}
	}
	return user, pass
}

// waitForConnection polls until a connection for user appears in the audit log.
func (s *stack) waitForConnection(t *testing.T, user string) audit.Connection {
	t.Helper()
	deadline := time.Now().Add(auditTimeout)

	for time.Now().Before(deadline) {
		var resp struct {
			Connections []audit.Connection `json:"connections"`
		}
		s.getJSON(t, "/api/connections", &resp)

		for _, c := range resp.Connections {
			if c.DBUser == user {
				return c
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("no connection for user %q reached the audit log within %s", user, auditTimeout)
	return audit.Connection{}
}
