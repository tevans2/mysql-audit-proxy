// Tests for issue #40's live-reconciliation acceptance criteria: config
// changes actually reach a *running* proxy process — no restart — via the
// real API, the real store, and the real ConfigClient/Manager/Reconciler
// wiring landed across this ticket's earlier chunks. routing_test.go proves
// static routing is correct; this file proves it stays correct while the
// configuration underneath it changes.
//
// Like harness_test.go, this runs the real `proxy` and `api` binaries as
// subprocesses (not in-process), against the same real upstream databases —
// see harness_test.go's package comment for the required environment and
// E2E_REQUIRE.
//
// stack (harness_test.go) only ever starts its processes once and kills them
// at cleanup — good enough for routing_test.go, but two of the acceptance
// criteria here (API downtime/recovery, a proxy restart reconstructing
// state) need to stop and restart a process mid-test. liveStack and
// managedProc below exist for exactly that; they are additive, and nothing
// in harness_test.go or routing_test.go was changed to add them.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"database/sql"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
)

// managedProc is a subprocess a test can stop (and, unlike start() in
// harness_test.go, later start again) mid-test.
type managedProc struct {
	name string
	cmd  *exec.Cmd
	stop sync.Once
}

// startManaged runs bin with env, streams its output into the test log once
// it exits, and registers a cleanup kill as a safety net — but unlike
// start(), also hands back a handle a test can Stop() itself, before the
// test ends, and then start a fresh process to replace it.
func startManaged(t *testing.T, name, bin string, env []string) *managedProc {
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

	go func() {
		b, _ := io.ReadAll(out)
		if len(b) > 0 {
			t.Logf("--- %s ---\n%s", name, b)
		}
	}()

	mp := &managedProc{name: name, cmd: cmd}
	t.Cleanup(mp.Stop)
	return mp
}

// Stop kills the process and waits for it to exit. Safe to call more than
// once (only the first call does anything), so a test can Stop() a process
// itself and cleanup's own Stop() call later is a no-op.
func (m *managedProc) Stop() {
	m.stop.Do(func() {
		_ = m.cmd.Process.Kill()
		_ = m.cmd.Wait()
	})
}

// liveStackOpts tunes a liveStack's proxy for fast, deterministic tests —
// production defaults (a 3s poll interval, a 30s drain timeout) would make
// every test below either slow or racy. Zero fields fall back to defaults
// below, still far faster than production but generous enough not to be
// flaky against a loaded CI runner.
type liveStackOpts struct {
	PollInterval    time.Duration
	RequestTimeout  time.Duration
	RetryMax        time.Duration
	DrainTimeout    time.Duration
	StartupDeadline time.Duration
}

func (o liveStackOpts) withDefaults() liveStackOpts {
	if o.PollInterval <= 0 {
		o.PollInterval = 300 * time.Millisecond
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 2 * time.Second
	}
	if o.RetryMax <= 0 {
		o.RetryMax = 1 * time.Second
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = 2 * time.Second
	}
	if o.StartupDeadline <= 0 {
		o.StartupDeadline = 10 * time.Second
	}
	return o
}

// serverReq mirrors internal/api's ServerConfigReq — the flattened POST/PUT
// body for /api/servers. Duplicated here rather than imported: it is an API
// wire type, not something cmd/proxy or e2e should import internal/api for.
type serverReq struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	ListenPort   int    `json:"listen_port"`
	UpstreamHost string `json:"upstream_host"`
	UpstreamPort int    `json:"upstream_port"`
	UpstreamUser string `json:"upstream_user"`
	UpstreamPass string `json:"upstream_pass"`
	Enabled      bool   `json:"enabled"`
	TLS          string `json:"tls,omitempty"`
}

// serverInfo mirrors GET /api/servers' sanitised response shape (no
// upstream_pass — see HandleServers).
type serverInfo struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	ListenPort   int    `json:"listen_port"`
	UpstreamHost string `json:"upstream_host"`
	UpstreamPort int    `json:"upstream_port"`
	UpstreamUser string `json:"upstream_user"`
	Enabled      bool   `json:"enabled"`
}

// liveStack is startStack's (harness_test.go) sibling: an API and a
// live-reconciling proxy, but with handles that let a test stop and restart
// either process independently, and helpers to drive /api/servers directly
// (POST/PUT/DELETE) — the actions a real operator (or the UI) would take
// against a running system.
type liveStack struct {
	dir       string
	apiBin    string
	apiEnv    []string
	apiURL    string
	token     string
	apiProc   *managedProc
	proxyBin  string
	proxyEnv  []string
	proxyProc *managedProc
}

// startLiveStack brings up the API and a live-reconciling proxy in front of
// ups, provisioning each of ups as an enabled server up front (same as
// startStack), then waits for every listener to come up.
func startLiveStack(t *testing.T, opts liveStackOpts, ups ...upstream) *liveStack {
	t.Helper()
	opts = opts.withDefaults()

	proxyBin, apiBin := buildBinaries(t)
	dir := t.TempDir()

	apiPort := freePort(t)
	apiURL := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	apiEnv := []string{
		fmt.Sprintf("API_LISTEN_ADDR=127.0.0.1:%d", apiPort),
		"API_DB_PATH=" + filepath.Join(dir, "auditproxy.db"),
		"API_ADMIN_PASS_HASH=$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW",
		"API_CONFIG_KEY=0123456789abcdef0123456789abcdef",
		"API_PROXY_TOKEN=e2e-live-secret-token",
	}
	apiProc := startManaged(t, "api", apiBin, apiEnv)
	waitHealthz(t, apiURL)

	s := &liveStack{
		dir: dir, apiBin: apiBin, apiEnv: apiEnv, apiURL: apiURL, apiProc: apiProc,
	}
	s.login(t)

	for _, u := range ups {
		port := freePort(t)
		host, upPortStr, err := net.SplitHostPort(u.addr)
		if err != nil {
			t.Fatalf("upstream %s address %q: %v", u.name, u.addr, err)
		}
		var upPort int
		fmt.Sscanf(upPortStr, "%d", &upPort)

		s.postServer(t, serverReq{
			Name: u.name, ListenPort: port, UpstreamHost: host, UpstreamPort: upPort,
			UpstreamUser: bootstrapUser, UpstreamPass: bootstrapPass(t, u), Enabled: true,
		})
	}

	s.proxyBin = proxyBin
	s.proxyEnv = []string{
		"API_URL=" + apiURL,
		"API_PROXY_TOKEN=e2e-live-secret-token",
		"SINK_INGEST_URL=" + apiURL + "/api/events",
		"SINK_BATCH_SIZE=1",
		"SINK_FLUSH_INTERVAL=200ms",
		"PROXY_CONFIG_POLL_INTERVAL=" + opts.PollInterval.String(),
		"PROXY_CONFIG_REQUEST_TIMEOUT=" + opts.RequestTimeout.String(),
		"PROXY_CONFIG_RETRY_MAX=" + opts.RetryMax.String(),
		"PROXY_DRAIN_TIMEOUT=" + opts.DrainTimeout.String(),
		"PROXY_CONFIG_STARTUP_DEADLINE=" + opts.StartupDeadline.String(),
	}
	s.proxyProc = startManaged(t, "proxy-live", proxyBin, s.proxyEnv)

	for _, u := range ups {
		srv := s.serverByName(t, u.name)
		waitTCP(t, "proxy-"+u.name, fmt.Sprintf("127.0.0.1:%d", srv.ListenPort))
	}

	return s
}

// restartProxy kills the current proxy process and starts a fresh one with
// the same env. The new process has no memory of the old one's in-flight
// reconciliation state — it re-fetches its initial snapshot from the API
// exactly as any other startup would, which is the point: whatever the
// API's store currently holds is what comes back up, not whatever this
// process happened to start with originally.
func (s *liveStack) restartProxy(t *testing.T) {
	t.Helper()
	s.proxyProc.Stop()
	s.proxyProc = startManaged(t, "proxy-live-restarted", s.proxyBin, s.proxyEnv)
}

// stopAPI kills the API process without restarting it, simulating an outage
// the proxy's already-running listeners must survive untouched.
func (s *liveStack) stopAPI(t *testing.T) {
	t.Helper()
	s.apiProc.Stop()
}

// restartAPI starts a fresh API process against the same on-disk store
// (same API_DB_PATH), so whatever was persisted — server records, the
// revision counter — survives the outage. This is what lets the proxy
// "catch up" once it reaches the API again: the API is the same store, just
// briefly unreachable.
//
// It also logs back in: an operator session lives only in the API
// process's own memory, not the store, so the token from before the outage
// is worthless against the new process — every doServersReq call after
// restartAPI needs s.token to be the fresh one.
func (s *liveStack) restartAPI(t *testing.T) {
	t.Helper()
	s.apiProc = startManaged(t, "api-restarted", s.apiBin, s.apiEnv)
	waitHealthz(t, s.apiURL)
	s.login(t)
}

// login authenticates against s.apiURL and stores the resulting token in
// s.token, overwriting whatever was there before. Split out from
// startLiveStack so restartAPI can call it again after the operator
// session an earlier login established is gone.
func (s *liveStack) login(t *testing.T) {
	t.Helper()
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
	// The API issues the session as an HttpOnly cookie, not in the JSON body.
	s.token = ""
	for _, c := range loginResp.Cookies() {
		if c.Name == "session" {
			s.token = c.Value
		}
	}
	if s.token == "" {
		t.Fatalf("e2e login did not set a session cookie")
	}
}

func (s *liveStack) doServersReq(t *testing.T, method, query string, req *serverReq) {
	t.Helper()
	var body io.Reader
	if req != nil {
		if req.TLS == "" {
			req.TLS = audit.TLSSkipVerify
		}
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshalling %s /api/servers body: %v", method, err)
		}
		body = bytes.NewReader(b)
	}

	httpReq, err := http.NewRequest(method, s.apiURL+"/api/servers"+query, body)
	if err != nil {
		t.Fatalf("creating %s /api/servers%s: %v", method, query, err)
	}

	// Ensure the session cookie and CSRF headers are present on all liveStack requests
	httpReq.AddCookie(&http.Cookie{Name: "session", Value: s.token})
	httpReq.Header.Set("X-Requested-With", "XMLHttpRequest")

	if req != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("%s /api/servers%s: %v", method, query, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s /api/servers%s: status %d: %s", method, query, resp.StatusCode, b)
	}
}

func (s *liveStack) postServer(t *testing.T, req serverReq) {
	t.Helper()
	s.doServersReq(t, http.MethodPost, "", &req)
}

func (s *liveStack) putServer(t *testing.T, req serverReq) {
	t.Helper()
	s.doServersReq(t, http.MethodPut, "", &req)
}

func (s *liveStack) deleteServer(t *testing.T, id int) {
	t.Helper()
	s.doServersReq(t, http.MethodDelete, fmt.Sprintf("?id=%d", id), nil)
}

func (s *liveStack) servers(t *testing.T) []serverInfo {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.apiURL+"/api/servers", nil)
	if err != nil {
		t.Fatalf("creating GET /api/servers: %v", err)
	}

	req.AddCookie(&http.Cookie{Name: "session", Value: s.token})
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/servers: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/servers: status %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Servers []serverInfo `json:"servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding GET /api/servers: %v", err)
	}
	return out.Servers
}

func (s *liveStack) serverByName(t *testing.T, name string) serverInfo {
	t.Helper()
	for _, srv := range s.servers(t) {
		if srv.Name == name {
			return srv
		}
	}
	t.Fatalf("no server named %q in GET /api/servers", name)
	return serverInfo{}
}

// dial opens one connection to addr — SetMaxOpenConns(1), same as
// stack.openThroughProxy, so a query against the returned *sql.DB always
// reuses the same underlying session rather than silently opening a second
// one from the pool.
func (s *liveStack) dial(t *testing.T, addr, user, pass, database string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?timeout=10s&tls=skip-verify", user, pass, addr, database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("opening %s: %v", addr, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// waitForRouting polls addr with a brand-new connection every attempt (not
// one held open across the whole wait) until markerQuery answers want, or
// timeout elapses. A fresh connection each attempt is deliberate: it is
// exactly what proves "new connections get the new configuration", as
// opposed to reusing one connection and never actually exercising routing
// for a new session.
func waitForRouting(t *testing.T, addr, user, pass, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastGot string

	for time.Now().Before(deadline) {
		func() {
			dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=3s&tls=skip-verify", user, pass, addr)
			db, err := sql.Open("mysql", dsn)
			if err != nil {
				lastErr = err
				return
			}
			defer db.Close()
			db.SetMaxOpenConns(1)

			var got string
			if err := db.QueryRow(markerQuery).Scan(&got); err != nil {
				lastErr = err
				return
			}
			lastErr = nil
			lastGot = got
		}()

		if lastErr == nil && lastGot == want {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}

	t.Fatalf("routing at %s never reached %q within %s (last saw %q, last error: %v)",
		addr, want, timeout, lastGot, lastErr)
}

// waitPortClosed polls until addr refuses new connections, or timeout
// elapses — the direct check for "stop accepting new connections", as
// opposed to inferring it indirectly from a routing assertion.
func waitPortClosed(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("%s is still accepting new connections after %s", addr, timeout)
}

func addr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// TestLiveConfig_POSTAddsListenerWithoutRestart is the direct test for the
// ticket's "POST starts a listener without restarting the proxy" and "New
// listeners are created for newly enabled servers" acceptance criteria: the
// proxy starts knowing about only one upstream, and a server POSTed to the
// API afterwards becomes reachable through the very same still-running
// process — nothing here restarts it.
func TestLiveConfig_POSTAddsListenerWithoutRestart(t *testing.T) {
	a, b := upstreams(t)
	seedUpstream(t, a)
	seedUpstream(t, b)

	s := startLiveStack(t, liveStackOpts{}, a)

	if got := len(s.servers(t)); got != 1 {
		t.Fatalf("stack started with %d server(s), want 1 (only %s)", got, a.name)
	}

	bPort := freePort(t)
	host, upPortStr, err := net.SplitHostPort(b.addr)
	if err != nil {
		t.Fatalf("upstream %s address %q: %v", b.name, b.addr, err)
	}
	var upPort int
	fmt.Sscanf(upPortStr, "%d", &upPort)

	t.Log("Step 1: POSTing a brand-new server while the proxy is already running")
	s.postServer(t, serverReq{
		Name: b.name, ListenPort: bPort, UpstreamHost: host, UpstreamPort: upPort,
		UpstreamUser: bootstrapUser, UpstreamPass: bootstrapPass(t, b), Enabled: true,
	})

	t.Log("Step 2: waiting for the new listener to come up — no restart of our own happens here")
	waitTCP(t, "proxy-"+b.name, addr(bPort))

	db := s.dial(t, addr(bPort), aliceUser, alicePass(t), "")
	var got string
	if err := db.QueryRow(markerQuery).Scan(&got); err != nil {
		t.Fatalf("querying newly-added server %s: %v", b.name, err)
	}
	if got != b.name {
		t.Errorf("new listener reached %q, want %q", got, b.name)
	}
}

// TestLiveConfig_PUTUpdatesRoutingForNewConnectionsOnly is the direct test
// for "PUT changes routing for new connections" and "Existing sessions are
// not unexpectedly rerouted" together: a session opened before the PUT must
// keep talking to the original upstream for its whole lifetime, while a
// session opened after the PUT must reach the new one — proven against real
// databases, not just the in-process Reconciler test.
func TestLiveConfig_PUTUpdatesRoutingForNewConnectionsOnly(t *testing.T) {
	a, b := upstreams(t)
	seedUpstream(t, a)
	seedUpstream(t, b)

	s := startLiveStack(t, liveStackOpts{}, a)
	s1 := s.serverByName(t, a.name)

	t.Log("Step 1: opening a session before the update — it must reach the original upstream")
	before := s.dial(t, addr(s1.ListenPort), aliceUser, alicePass(t), "")
	var got string
	if err := before.QueryRow(markerQuery).Scan(&got); err != nil {
		t.Fatalf("querying before the update: %v", err)
	}
	if got != a.name {
		t.Fatalf("session opened before the update reached %q, want %q", got, a.name)
	}

	bHost, bPortStr, err := net.SplitHostPort(b.addr)
	if err != nil {
		t.Fatalf("upstream %s address %q: %v", b.name, b.addr, err)
	}
	var bPort int
	fmt.Sscanf(bPortStr, "%d", &bPort)

	t.Log("Step 2: PUTting the same server's upstream to point at a different database, same listen port")
	s.putServer(t, serverReq{
		ID: s1.ID, Name: s1.Name, ListenPort: s1.ListenPort,
		UpstreamHost: bHost, UpstreamPort: bPort,
		UpstreamUser: bootstrapUser, UpstreamPass: bootstrapPass(t, b), // must supply the new password explicitly
		Enabled: true,
	})

	t.Log("Step 3: a fresh connection after the update must reach the new upstream")
	waitForRouting(t, addr(s1.ListenPort), aliceUser, alicePass(t), b.name, 10*time.Second)

	t.Log("Step 4: the session opened before the update must still reach the original upstream")
	if err := before.QueryRow(markerQuery).Scan(&got); err != nil {
		t.Fatalf("querying the pre-existing session after the update: %v", err)
	}
	if got != a.name {
		t.Errorf("pre-existing session now reaches %q, want it to still reach %q", got, a.name)
	}
}

// TestLiveConfig_PortChangeBringsUpNewPortAndRetiresOld is the direct test
// for "Port changes bring up the new port and retire the old one": moving a
// server's listen_port must make the new port serve traffic and the old
// port stop accepting new connections, without anything here restarting the
// proxy.
func TestLiveConfig_PortChangeBringsUpNewPortAndRetiresOld(t *testing.T) {
	a, _ := upstreams(t)
	seedUpstream(t, a)

	s := startLiveStack(t, liveStackOpts{}, a)
	s1 := s.serverByName(t, a.name)
	oldPort := s1.ListenPort
	newPort := freePort(t)

	t.Log("Step 1: PUTting a new listen_port for the same server")
	s.putServer(t, serverReq{
		ID: s1.ID, Name: s1.Name, ListenPort: newPort,
		UpstreamHost: s1.UpstreamHost, UpstreamPort: s1.UpstreamPort,
		UpstreamUser: bootstrapUser, UpstreamPass: bootstrapPass(t, a),
		Enabled: true,
	})

	t.Log("Step 2: the new port must come up and route correctly")
	waitTCP(t, "proxy-"+a.name+"-new-port", addr(newPort))
	waitForRouting(t, addr(newPort), aliceUser, alicePass(t), a.name, 10*time.Second)

	t.Log("Step 3: the old port must stop accepting new connections")
	waitPortClosed(t, addr(oldPort), 10*time.Second)
}

// TestLiveConfig_DeleteDrainsExistingSessionSuccessfully is the direct test
// for "DELETE and disable stop new connections" and "Existing sessions
// drain successfully": a session with a query already in flight when its
// server is deleted must still get its result back, while a brand-new
// connection attempt after the delete must be refused.
func TestLiveConfig_DeleteDrainsExistingSessionSuccessfully(t *testing.T) {
	a, _ := upstreams(t)
	seedUpstream(t, a)

	// A drain timeout comfortably longer than the in-flight query below, so
	// the session has every opportunity to finish on its own rather than
	// racing DrainTimeout's forced close.
	s := startLiveStack(t, liveStackOpts{DrainTimeout: 8 * time.Second}, a)
	s1 := s.serverByName(t, a.name)

	inFlight := s.dial(t, addr(s1.ListenPort), aliceUser, alicePass(t), "")
	// A throwaway connection first, so the pool actually opens the
	// underlying session before the slow query starts — otherwise the
	// DELETE below could race sql.Open's lazy dial.
	if err := inFlight.Ping(); err != nil {
		t.Fatalf("priming the session before delete: %v", err)
	}

	resultCh := make(chan error, 1)
	go func() {
		var discard int
		resultCh <- inFlight.QueryRow("SELECT SLEEP(2)").Scan(&discard)
	}()

	// Give the SLEEP query a moment to actually start before deleting —
	// otherwise the delete could land before the session is doing anything
	// the drain needs to wait out.
	time.Sleep(300 * time.Millisecond)

	t.Log("Step 1: deleting the server while a query is in flight on an existing session")
	s.deleteServer(t, s1.ID)

	t.Log("Step 2: new connection attempts must eventually be refused")
	waitPortClosed(t, addr(s1.ListenPort), 10*time.Second)

	t.Log("Step 3: the in-flight query must still complete successfully — drained, not severed")
	select {
	case err := <-resultCh:
		if err != nil {
			t.Errorf("in-flight query on the deleted server's session failed: %v, want it to drain to completion", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight query never returned — the session was neither drained nor forcibly closed")
	}
}

// TestLiveConfig_DisableStopsNewConnectionsAndDrainsExisting is
// TestLiveConfig_DeleteDrainsExistingSessionSuccessfully's sibling for the
// ticket's "disabled or deleted" wording: Manager.Plan produces the same
// ActionRemove for a server that is merely disabled (still present in the
// snapshot, Enabled: false) as for one that is deleted outright, but that
// equivalence is only proven at the Manager/Reconciler unit level so far —
// this confirms it holds through the real API and a real database too.
func TestLiveConfig_DisableStopsNewConnectionsAndDrainsExisting(t *testing.T) {
	a, _ := upstreams(t)
	seedUpstream(t, a)

	s := startLiveStack(t, liveStackOpts{DrainTimeout: 8 * time.Second}, a)
	s1 := s.serverByName(t, a.name)

	inFlight := s.dial(t, addr(s1.ListenPort), aliceUser, alicePass(t), "")
	if err := inFlight.Ping(); err != nil {
		t.Fatalf("priming the session before disabling: %v", err)
	}

	resultCh := make(chan error, 1)
	go func() {
		var discard int
		resultCh <- inFlight.QueryRow("SELECT SLEEP(2)").Scan(&discard)
	}()
	time.Sleep(300 * time.Millisecond)

	t.Log("Step 1: disabling the server (PUT Enabled: false) while a query is in flight")
	s.putServer(t, serverReq{
		ID: s1.ID, Name: s1.Name, ListenPort: s1.ListenPort,
		UpstreamHost: s1.UpstreamHost, UpstreamPort: s1.UpstreamPort,
		UpstreamUser: bootstrapUser, UpstreamPass: bootstrapPass(t, a),
		Enabled: false,
	})

	t.Log("Step 2: new connection attempts must eventually be refused")
	waitPortClosed(t, addr(s1.ListenPort), 10*time.Second)

	t.Log("Step 3: the in-flight query must still complete successfully — drained, not severed")
	select {
	case err := <-resultCh:
		if err != nil {
			t.Errorf("in-flight query on the disabled server's session failed: %v, want it to drain to completion", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight query never returned — the session was neither drained nor forcibly closed")
	}
}

// TestLiveConfig_APIDowntimeLeavesListenersRunningAndCatchesUpAfterRecovery
// is the direct test for "API downtime does not interrupt active listeners"
// and "the proxy catches up after API recovery": killing the API must not
// affect the proxy's already-running listener at all, and once the API
// comes back, a configuration change made after recovery must still reach
// the proxy.
func TestLiveConfig_APIDowntimeLeavesListenersRunningAndCatchesUpAfterRecovery(t *testing.T) {
	a, b := upstreams(t)
	seedUpstream(t, a)
	seedUpstream(t, b)

	s := startLiveStack(t, liveStackOpts{}, a)
	s1 := s.serverByName(t, a.name)

	t.Log("Step 1: confirming routing works before any outage")
	waitForRouting(t, addr(s1.ListenPort), aliceUser, alicePass(t), a.name, 5*time.Second)

	t.Log("Step 2: killing the API")
	s.stopAPI(t)

	// Long enough for several failed poll attempts against the now-dead API
	// (PollInterval defaults to 300ms here) — if a poll failure were ever
	// going to tear down the listener, it would have happened by now.
	time.Sleep(2 * time.Second)

	t.Log("Step 3: the existing listener must still serve traffic while the API is down")
	waitForRouting(t, addr(s1.ListenPort), aliceUser, alicePass(t), a.name, 5*time.Second)

	t.Log("Step 4: bringing the API back up against the same on-disk store")
	s.restartAPI(t)

	bHost, bPortStr, err := net.SplitHostPort(b.addr)
	if err != nil {
		t.Fatalf("upstream %s address %q: %v", b.name, b.addr, err)
	}
	var bPort int
	fmt.Sscanf(bPortStr, "%d", &bPort)

	t.Log("Step 5: a config change made after recovery must still reach the proxy")
	s.putServer(t, serverReq{
		ID: s1.ID, Name: s1.Name, ListenPort: s1.ListenPort,
		UpstreamHost: bHost, UpstreamPort: bPort,
		UpstreamUser: bootstrapUser, UpstreamPass: bootstrapPass(t, b),
		Enabled: true,
	})
	waitForRouting(t, addr(s1.ListenPort), aliceUser, alicePass(t), b.name, 10*time.Second)
}

// TestLiveConfig_FailedBindPreservesLastWorkingConfig is the direct test
// for "A failed bind preserves the last working configuration": moving a
// server to a port that is already taken by something else must leave the
// original listener completely undisturbed.
func TestLiveConfig_FailedBindPreservesLastWorkingConfig(t *testing.T) {
	a, _ := upstreams(t)
	seedUpstream(t, a)

	s := startLiveStack(t, liveStackOpts{}, a)
	s1 := s.serverByName(t, a.name)
	oldPort := s1.ListenPort

	t.Log("Step 1: occupying the port the move is about to try to bind")
	blocker, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen (blocker): %v", err)
	}
	defer blocker.Close()
	occupiedPort := blocker.Addr().(*net.TCPAddr).Port

	t.Log("Step 2: PUTting a move to the already-occupied port")
	s.putServer(t, serverReq{
		ID: s1.ID, Name: s1.Name, ListenPort: occupiedPort,
		UpstreamHost: s1.UpstreamHost, UpstreamPort: s1.UpstreamPort,
		UpstreamUser: bootstrapUser, UpstreamPass: bootstrapPass(t, a),
		Enabled: true,
	})

	// Give the proxy several poll cycles' worth of time to have attempted
	// (and failed) the move.
	time.Sleep(2 * time.Second)

	t.Log("Step 3: the original listener must still be up and routing correctly")
	waitForRouting(t, addr(oldPort), aliceUser, alicePass(t), a.name, 5*time.Second)
}

// TestLiveConfig_ProxyRestartReconstructsLatestDatabaseState is the direct
// test for "Proxy restart reconstructs the latest database state": a
// server added to the API purely through live reconciliation (this process
// never had it in its own original startup snapshot) must still be there,
// correctly routed, after the proxy process is killed and a brand-new one
// takes its place.
func TestLiveConfig_ProxyRestartReconstructsLatestDatabaseState(t *testing.T) {
	a, b := upstreams(t)
	seedUpstream(t, a)
	seedUpstream(t, b)

	s := startLiveStack(t, liveStackOpts{}, a)
	s1 := s.serverByName(t, a.name)

	bPort := freePort(t)
	bHost, bPortStr, err := net.SplitHostPort(b.addr)
	if err != nil {
		t.Fatalf("upstream %s address %q: %v", b.name, b.addr, err)
	}
	var bUpPort int
	fmt.Sscanf(bPortStr, "%d", &bUpPort)

	t.Log("Step 1: adding a second server purely through live reconciliation")
	s.postServer(t, serverReq{
		Name: b.name, ListenPort: bPort, UpstreamHost: bHost, UpstreamPort: bUpPort,
		UpstreamUser: bootstrapUser, UpstreamPass: bootstrapPass(t, b), Enabled: true,
	})
	waitTCP(t, "proxy-"+b.name+"-live", addr(bPort))

	t.Log("Step 2: restarting the proxy process — the new process has no memory of the live update above")
	s.restartProxy(t)

	t.Log("Step 3: both listeners must come back up from the API's current state, not the original startup snapshot")
	waitTCP(t, "proxy-"+a.name+"-restarted", addr(s1.ListenPort))
	waitTCP(t, "proxy-"+b.name+"-restarted", addr(bPort))

	waitForRouting(t, addr(s1.ListenPort), aliceUser, alicePass(t), a.name, 5*time.Second)
	waitForRouting(t, addr(bPort), aliceUser, alicePass(t), b.name, 5*time.Second)
}
