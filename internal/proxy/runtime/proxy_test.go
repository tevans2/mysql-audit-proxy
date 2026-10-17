package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"lustre/internal/audit"
	"lustre/internal/proxy/config"
)

// TestProxyConnectEvent verifies that connecting a MySQL client through the
// proxy emits a connect event with the correct username to the fake Sink.
func TestProxyConnectEvent(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Connecting through the proxy using the standard MySQL driver")
	// Connect through the proxy using the standard MySQL driver.
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("db.Ping through proxy: %v", err)
	}

	t.Log("Step 2: Waiting for the proxy to emit a connect event")
	// Wait for the connect event.
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}

	events := sink.getEvents()
	var connectEvent *audit.Event
	for i := range events {
		if events[i].Kind == audit.KindConnect {
			connectEvent = &events[i]
			break
		}
	}
	if connectEvent == nil {
		t.Fatal("no connect event found")
	}
	if connectEvent.ConnID == "" {
		t.Errorf("connect event conn_id is empty")
	}

	_, portStr, _ := net.SplitHostPort(proxyAddr)
	expectedPort, _ := strconv.Atoi(portStr)
	if connectEvent.ProxyPort != expectedPort {
		t.Errorf("connect event proxy_port = %d, want %d", connectEvent.ProxyPort, expectedPort)
	}
	if connectEvent.Upstream != "test-db" {
		t.Errorf("connect event upstream = %q, want %q", connectEvent.Upstream, "test-db")
	}
	if connectEvent.User != upstreamUser {
		t.Errorf("connect event username = %q, want %q", connectEvent.User, upstreamUser)
	}
	if connectEvent.UpstreamAddr != upstreamAddr {
		t.Errorf("connect event upstream_addr = %q, want %q", connectEvent.UpstreamAddr, upstreamAddr)
	}
	// Reaching a connect event means the handshake succeeded. The UI renders a
	// badge from this field, so an unset one shows blank on every connection.
	if connectEvent.AuthResult != audit.AuthOK {
		t.Errorf("connect event auth_result = %q, want %q", connectEvent.AuthResult, audit.AuthOK)
	}

	db.Close()
}

// TestProxyDisconnectEvent verifies that closing the client connection
// emits a disconnect event.
func TestProxyDisconnectEvent(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Connecting and pinging the proxy")
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Fatalf("db.Ping through proxy: %v", err)
	}

	t.Log("Step 2: Waiting for connect event to register session")
	// Wait for connect first.
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}

	t.Log("Step 3: Closing the client connection directly")
	// Close the connection — this should trigger a disconnect event.
	db.Close()

	t.Log("Step 4: Waiting for proxy to emit disconnect event")
	if !sink.waitForEvent(audit.KindDisconnect, 5*time.Second) {
		t.Fatal("timed out waiting for disconnect event")
	}

	events := sink.getEvents()
	var connectEvent *audit.Event
	var disconnectEvent *audit.Event
	for i := range events {
		if events[i].Kind == audit.KindConnect {
			connectEvent = &events[i]
		}
		if events[i].Kind == audit.KindDisconnect {
			disconnectEvent = &events[i]
		}
	}
	if connectEvent == nil {
		t.Fatal("no connect event found")
	}
	if disconnectEvent == nil {
		t.Fatal("no disconnect event found")
	}
	if disconnectEvent.User != upstreamUser {
		t.Errorf("disconnect event username = %q, want %q", disconnectEvent.User, upstreamUser)
	}
	if disconnectEvent.ConnID != connectEvent.ConnID {
		t.Errorf("disconnect event conn_id = %q, want %q", disconnectEvent.ConnID, connectEvent.ConnID)
	}
}

// TestProxyConcurrentSessions verifies that two concurrent client sessions produce
// distinct conn_ids, and each disconnect event carries the same conn_id as its connect.
func TestProxyConcurrentSessions(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)

	t.Log("Step 1: Opening two concurrent sessions (db1 and db2)")
	// Session 1
	db1, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open db1: %v", err)
	}
	if err := db1.Ping(); err != nil {
		t.Fatalf("db1.Ping: %v", err)
	}

	// Session 2
	db2, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open db2: %v", err)
	}
	if err := db2.Ping(); err != nil {
		t.Fatalf("db2.Ping: %v", err)
	}

	t.Log("Step 2: Closing both sessions")
	// Close both
	db1.Close()
	db2.Close()

	t.Log("Step 3: Waiting for both disconnect events")
	// Wait for 2 disconnects
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		disconnects := 0
		for _, e := range sink.getEvents() {
			if e.Kind == audit.KindDisconnect {
				disconnects++
			}
		}
		if disconnects >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var connects []audit.Event
	var disconnects []audit.Event
	for _, e := range sink.getEvents() {
		if e.Kind == audit.KindConnect {
			connects = append(connects, e)
		} else if e.Kind == audit.KindDisconnect {
			disconnects = append(disconnects, e)
		}
	}

	if len(connects) < 2 {
		t.Fatalf("got %d connect events, want 2", len(connects))
	}
	if len(disconnects) < 2 {
		t.Fatalf("got %d disconnect events, want 2", len(disconnects))
	}

	if connects[0].ConnID == connects[1].ConnID {
		t.Errorf("concurrent sessions have identical conn_ids: %s", connects[0].ConnID)
	}

	for _, conn := range connects[:2] {
		found := false
		for _, disc := range disconnects {
			if conn.ConnID == disc.ConnID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no disconnect event found for conn_id %s", conn.ConnID)
		}
	}
}

// TestBuildProxies_MissingPassword verifies a server with no matching entry
// in the passwords map is a fatal configuration error, not a silent fallback
// to some other upstream's password — see BuildProxies's doc comment for why
// that matters here specifically (db-a and db-b intentionally don't share
// credentials).
func TestBuildProxies_MissingPassword(t *testing.T) {
	servers := []audit.Server{
		{Name: "db-a", ListenPort: 3307, UpstreamHost: "db-a", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true},
	}
	_, err := BuildProxies(servers, map[string]string{"db-b": "rootpass-b"}, &testSink{}, config.DefaultUpstreamReadTimeout, 0)
	if err == nil {
		t.Fatal("buildProxies with no password for db-a: want an error, got nil")
	}
}

// TestBuildProxies_PerServerFields verifies each configured server produces a
// *Proxy carrying its own listen address, upstream address, name and
// password — not, say, every entry pointing at the last one processed.
func TestBuildProxies_PerServerFields(t *testing.T) {
	servers := []audit.Server{
		{Name: "db-a", ListenPort: 3307, UpstreamHost: "db-a", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true},
		{Name: "db-b", ListenPort: 3308, UpstreamHost: "db-b", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true},
	}
	passwords := map[string]string{"db-a": "rootpass", "db-b": "rootpass-b"}

	proxies, err := BuildProxies(servers, passwords, &testSink{}, config.DefaultUpstreamReadTimeout, 0)
	if err != nil {
		t.Fatalf("buildProxies: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("got %d proxies, want 2", len(proxies))
	}

	want := map[string]struct {
		listenAddr, upstreamAddr, pass string
	}{
		"db-a": {"0.0.0.0:3307", "db-a:3306", "rootpass"},
		"db-b": {"0.0.0.0:3308", "db-b:3306", "rootpass-b"},
	}
	for _, p := range proxies {
		w, ok := want[p.UpstreamName]
		if !ok {
			t.Fatalf("unexpected proxy for upstream %q", p.UpstreamName)
		}
		if p.ListenAddr != w.listenAddr {
			t.Errorf("%s: ListenAddr = %q, want %q", p.UpstreamName, p.ListenAddr, w.listenAddr)
		}
		if p.UpstreamAddr != w.upstreamAddr {
			t.Errorf("%s: UpstreamAddr = %q, want %q", p.UpstreamName, p.UpstreamAddr, w.upstreamAddr)
		}
		if p.UpstreamPass != w.pass {
			t.Errorf("%s: UpstreamPass = %q, want %q", p.UpstreamName, p.UpstreamPass, w.pass)
		}
	}
}

// TestBuildProxies_JoinsIPv6Host verifies upstream addresses are built with
// net.JoinHostPort, not fmt.Sprintf("%s:%d", ...) — an IPv6 literal host
// needs brackets ("[::1]:3306") to be a valid, parseable address; naive
// string concatenation produces "::1:3306", which is ambiguous and not what
// net.Dial expects.
func TestBuildProxies_JoinsIPv6Host(t *testing.T) {
	servers := []audit.Server{
		{Name: "db-a", ListenPort: 3307, UpstreamHost: "::1", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true},
	}
	proxies, err := BuildProxies(servers, map[string]string{"db-a": "rootpass"}, &testSink{}, config.DefaultUpstreamReadTimeout, 0)
	if err != nil {
		t.Fatalf("buildProxies: %v", err)
	}
	want := "[::1]:3306"
	if proxies[0].UpstreamAddr != want {
		t.Errorf("UpstreamAddr = %q, want %q", proxies[0].UpstreamAddr, want)
	}
}

// TestMultiUpstreamRouting is the "two databases simultaneously" case end to
// end: two listeners from one config, each fronting a different real
// upstream (db-a, db-b). It asserts both halves of the ticket's "done when" —
// a client on each port reaches the matching database's own data, and the
// audit log attributes each session to the right upstream, not just to
// "whichever proxy happened to log it first".
//
// Requires both db-a and db-b (`make test-env`); skips if either is
// unreachable, same as the single-upstream integration tests above.
func TestMultiUpstreamRouting(t *testing.T) {
	addrA, userA, passA := testUpstreamConfig(t)
	addrB, userB, passB := testUpstreamBConfig(t)

	seedRoutingMarker(t, addrA, userA, passA, "db-a")
	seedRoutingMarker(t, addrB, userB, passB, "db-b")

	hostA, portAStr, err := net.SplitHostPort(addrA)
	if err != nil {
		t.Fatalf("parsing upstream A address %q: %v", addrA, err)
	}
	upPortA, err := strconv.Atoi(portAStr)
	if err != nil {
		t.Fatalf("upstream A port %q: %v", portAStr, err)
	}
	hostB, portBStr, err := net.SplitHostPort(addrB)
	if err != nil {
		t.Fatalf("parsing upstream B address %q: %v", addrB, err)
	}
	upPortB, err := strconv.Atoi(portBStr)
	if err != nil {
		t.Fatalf("upstream B port %q: %v", portBStr, err)
	}

	listenPortA := freePort(t)
	listenPortB := freePort(t)

	servers := []audit.Server{
		{ID: 1, Name: "db-a", ListenPort: listenPortA, UpstreamHost: hostA, UpstreamPort: upPortA, UpstreamUser: userA, Enabled: true},
		{ID: 2, Name: "db-b", ListenPort: listenPortB, UpstreamHost: hostB, UpstreamPort: upPortB, UpstreamUser: userB, Enabled: true},
	}
	passwords := map[string]string{"db-a": passA, "db-b": passB}

	sink := &testSink{}
	proxies, err := BuildProxies(servers, passwords, sink, config.DefaultUpstreamReadTimeout, 0)
	if err != nil {
		t.Fatalf("buildProxies: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	for _, p := range proxies {
		p.TLS = config.GetEnv("PROXY_TEST_TLS", "skip-verify")
		p.TLSCAFile = os.Getenv("PROXY_TEST_TLS_CA_FILE")
		p := p

		done := &atomic.Bool{}
		t.Cleanup(func() { done.Store(true) })
		p.Logger = log.New(testLogger{t, done}, "", 0)

		go func() {
			if err := p.Run(ctx, nil); err != nil && ctx.Err() == nil {
				t.Logf("proxy %s: Run returned: %v", p.UpstreamName, err)
			}
		}()
	}

	proxyAddrA := fmt.Sprintf("127.0.0.1:%d", listenPortA)
	proxyAddrB := fmt.Sprintf("127.0.0.1:%d", listenPortB)
	waitTCPReachable(t, proxyAddrA, 5*time.Second)
	waitTCPReachable(t, proxyAddrB, 5*time.Second)

	// Each listener's clients log in as themselves, and the database decides:
	// root on db-a and root on db-b have different passwords, so reaching the
	// wrong database with the other's credentials would fail, not pass.
	queryMarker := func(proxyAddr, user, pass string) string {
		t.Helper()
		dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", user, pass, proxyAddr)
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatalf("sql.Open %s: %v", proxyAddr, err)
		}
		defer db.Close()

		var name string
		if err := db.QueryRow("SELECT name FROM lustre_test.upstream_marker ORDER BY name LIMIT 1").Scan(&name); err != nil {
			t.Fatalf("querying marker through %s: %v", proxyAddr, err)
		}
		return name
	}

	// A client on each port must reach that port's own database, not the
	// other one — the routing half of "done when".
	if got := queryMarker(proxyAddrA, userA, passA); got != "db-a" {
		t.Errorf("marker through port %d (db-a's listener) = %q, want \"db-a\"", listenPortA, got)
	}
	if got := queryMarker(proxyAddrB, userB, passB); got != "db-b" {
		t.Errorf("marker through port %d (db-b's listener) = %q, want \"db-b\"", listenPortB, got)
	}

	// Both sessions' connect events must carry the right upstream — the audit
	// half of "done when".
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect events")
	}
	deadline := time.Now().Add(5 * time.Second)
	var connects []audit.Event
	for time.Now().Before(deadline) {
		connects = nil
		for _, e := range sink.getEvents() {
			if e.Kind == audit.KindConnect {
				connects = append(connects, e)
			}
		}
		if len(connects) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(connects) < 2 {
		t.Fatalf("got %d connect events, want 2: %+v", len(connects), connects)
	}

	byPort := map[int]audit.Event{}
	for _, e := range connects {
		byPort[e.ProxyPort] = e
	}

	evA, ok := byPort[listenPortA]
	if !ok {
		t.Fatalf("no connect event recorded for proxy_port=%d (db-a's listener); got ports %v", listenPortA, portsOf(connects))
	}
	if evA.Upstream != "db-a" || evA.UpstreamAddr != addrA {
		t.Errorf("connect event on port %d: upstream=%q upstream_addr=%q, want db-a / %s",
			listenPortA, evA.Upstream, evA.UpstreamAddr, addrA)
	}

	evB, ok := byPort[listenPortB]
	if !ok {
		t.Fatalf("no connect event recorded for proxy_port=%d (db-b's listener); got ports %v", listenPortB, portsOf(connects))
	}
	if evB.Upstream != "db-b" || evB.UpstreamAddr != addrB {
		t.Errorf("connect event on port %d: upstream=%q upstream_addr=%q, want db-b / %s",
			listenPortB, evB.Upstream, evB.UpstreamAddr, addrB)
	}
}
