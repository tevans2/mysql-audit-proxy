package runtime

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	sqldriver "github.com/go-sql-driver/mysql"

	"lustre/internal/audit"
)

// ---------------------------------------------------------------------------
// 1. Every database user can log in, and is recorded under their own name.
// ---------------------------------------------------------------------------

// TestVerify_AcceptsOtherUsernames connects as "demo", a real upstream user, and
// checks it is allowed through and recorded under its own name.
func TestVerify_AcceptsOtherUsernames(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Logf("Step 1: Connecting proxied client as %q", "demo")
	db, err := sql.Open("mysql", proxyDSN("demo", "demopass", proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Errorf("connecting through the proxy as 'demo' failed: %v", err)
		t.Logf("the database accepts 'demo' (it is a real upstream user), so the proxy must too, not only %q", upstreamUser)
		return
	}

	t.Log("Step 2: Waiting for connect event with username 'demo'")
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}
	for _, e := range sink.getEvents() {
		if e.Kind != audit.KindConnect {
			continue
		}
		t.Logf("connect event recorded username=%q", e.User)
		if e.User != "demo" {
			t.Errorf("audit username = %q, want %q", e.User, "demo")
		}
		return
	}
}

// ---------------------------------------------------------------------------
// A rejected login is audited with the attempted username and a reason — it
// never reaches handleClient's connect event, so it needs its own hook
// (auditingAuthHandler.OnAuthFailure in auth.go).
// ---------------------------------------------------------------------------

// TestVerify_AuthFailureEmitsEvent verifies a wrong password for a known user
// produces an auth_fail event carrying the attempted username and a non-empty
// reason, per "Failed-login auditing": "a failed login produces an auth_fail
// event with the attempted username and reason."
func TestVerify_AuthFailureEmitsEvent(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN("demo", "not-the-real-password", proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err == nil {
		t.Fatal("Ping with a wrong password unexpectedly succeeded")
	}

	if !sink.waitForEvent(audit.KindAuthFail, 5*time.Second) {
		t.Fatal("timed out waiting for auth_fail event")
	}

	for _, e := range sink.getEvents() {
		if e.Kind != audit.KindAuthFail {
			continue
		}
		if e.User != "demo" {
			t.Errorf("auth_fail User = %q, want %q", e.User, "demo")
		}
		if e.AuthResult != audit.AuthFail {
			t.Errorf("auth_fail AuthResult = %q, want %q", e.AuthResult, audit.AuthFail)
		}
		if e.Err == "" {
			t.Error("auth_fail Err is empty, want a reason")
		}
		return
	}
	t.Fatal("no auth_fail event found after waitForEvent reported one")
}

// TestVerify_AuthFailureUnknownUser covers the other way a login is
// rejected — a username the proxy has no credential for at all, not just a
// wrong password for a known one. go-mysql's OnAuthFailure hook fires either
// way, and the attempted (bogus) username still needs to reach the event, so
// whoever is watching the log knows who was probing, not just that someone
// was.
func TestVerify_AuthFailureUnknownUser(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN("nosuchuser", "whatever", proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err == nil {
		t.Fatal("Ping as an unconfigured user unexpectedly succeeded")
	}

	if !sink.waitForEvent(audit.KindAuthFail, 5*time.Second) {
		t.Fatal("timed out waiting for auth_fail event")
	}

	for _, e := range sink.getEvents() {
		if e.Kind != audit.KindAuthFail {
			continue
		}
		if e.User != "nosuchuser" {
			t.Errorf("auth_fail User = %q, want %q", e.User, "nosuchuser")
		}
		return
	}
	t.Fatal("no auth_fail event found after waitForEvent reported one")
}

// TestVerify_AuthFailureDoesNotEmitConnect verifies a rejected login produces
// no connect event — only auth_fail. A stray connect event for a session that
// never actually authenticated would misreport the failed attempt as a
// successful one wherever the audit log is read.
func TestVerify_AuthFailureDoesNotEmitConnect(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN("demo", "not-the-real-password", proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err == nil {
		t.Fatal("Ping with a wrong password unexpectedly succeeded")
	}

	if !sink.waitForEvent(audit.KindAuthFail, 5*time.Second) {
		t.Fatal("timed out waiting for auth_fail event")
	}

	for _, e := range sink.getEvents() {
		if e.Kind == audit.KindConnect {
			t.Errorf("unexpected connect event for a rejected login: %+v", e)
		}
	}
}

// waitAndAssertNoAuthFail polls the sink for the given window and fails the
// test the moment an auth_fail event shows up. It is the negative-case
// counterpart to testSink.waitForEvent: there is no single event to wait for
// that proves "no auth_fail was emitted", so this instead gives OnAuthFailure
// a generous window to have fired (handleClient's handshake runs in its own
// per-connection goroutine, so the event, if any, arrives asynchronously
// relative to the caller) and checks throughout, rather than sleeping once
// and checking only at the end.
func waitAndAssertNoAuthFail(t *testing.T, sink *testSink, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		for _, e := range sink.getEvents() {
			if e.Kind == audit.KindAuthFail {
				t.Fatalf("unexpected auth_fail event: %+v", e)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestVerify_BareTCPDisconnectDoesNotEmitAuthFail simulates a plain TCP
// health check: open a connection to the proxy and close it again without
// ever sending a MySQL handshake response. go-mysql still calls
// OnAuthFailure for this — readHandshakeResponse fails as soon as it tries
// to read the packet that never arrives, before readUserName has run — but
// no username was ever parsed. Recording that as a failed login for user ""
// is exactly the misleading record the review comment called out.
func TestVerify_BareTCPDisconnectDoesNotEmitAuthFail(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Opening a bare TCP connection to the proxy and closing it immediately")
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	conn.Close()

	t.Log("Step 2: Confirming no auth_fail event is emitted for the disconnect")
	waitAndAssertNoAuthFail(t, sink, 1*time.Second)
}

// TestVerify_ValidUserInvalidDBDoesNotEmitAuthFail connects with correct
// proxy credentials but names a database that doesn't exist upstream.
// UseDB no longer touches the upstream during the handshake itself (see
// proxyHandler.UseDB), so credentials check out fine and this instead fails
// in OnAuthSuccess, when applyPendingDB forwards the requested database for
// real and the upstream rejects it — not in OnAuthFailure, and not a
// credential rejection. It must not be recorded as a failed login for
// "demo".
func TestVerify_ValidUserInvalidDBDoesNotEmitAuthFail(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Connecting as a valid user with a database that doesn't exist upstream")
	dsn := fmt.Sprintf("demo:demopass@tcp(%s)/no_such_database?timeout=5s&tls=skip-verify", proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err == nil {
		t.Fatal("Ping against a nonexistent database unexpectedly succeeded")
	}

	t.Log("Step 2: Confirming no auth_fail event is emitted for a valid user with an invalid database")
	waitAndAssertNoAuthFail(t, sink, 1*time.Second)
}

// waitForAuthFailEvent polls the sink for an auth_fail event for the given
// user and, once found, returns its Err text. Fails the test if none shows
// up within the timeout.
func waitForAuthFailEvent(t *testing.T, sink *testSink, user string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range sink.getEvents() {
			if e.Kind == audit.KindAuthFail && e.User == user {
				return e.Err
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for an auth_fail event for user %q", user)
	return ""
}

// TestVerify_WrongPasswordWithInvalidDBIsStillAudited covers the false
// negative raised in code review: a rejected login used to go completely
// unaudited if the client also named a database that doesn't exist, because
// go-mysql's readHandshakeResponse called UseDB (which forwarded straight to
// the upstream and failed with "unknown database") before it ever compared
// credentials — so the wrong-password check never ran, and OnAuthFailure
// only ever saw the database error, which correctly-but-unhelpfully doesn't
// look like a credential rejection. A brute-force attempt could dodge the
// audit log entirely just by appending a junk database name to every guess.
//
// UseDB now defers touching the upstream until after OnAuthSuccess confirms
// the credentials, so this must produce a genuine auth_fail event, with a
// real credential-rejection reason — not empty, and not the database error.
func TestVerify_WrongPasswordWithInvalidDBIsStillAudited(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Connecting as a valid user, wrong password, nonexistent database")
	dsn := fmt.Sprintf("demo:TOTALLY-WRONG@tcp(%s)/no_such_database?timeout=5s&tls=skip-verify", proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err == nil {
		t.Fatal("Ping with a wrong password and a bad database unexpectedly succeeded")
	}

	t.Log("Step 2: Waiting for a genuine auth_fail event, not a database error")
	reason := waitForAuthFailEvent(t, sink, "demo", 5*time.Second)
	if reason == "" {
		t.Fatal("auth_fail event has an empty reason")
	}
	if strings.Contains(strings.ToLower(reason), "database") {
		t.Errorf("auth_fail reason looks like the database error, not a credential rejection: %q", reason)
	}
}

// TestVerify_UnknownUserWithInvalidDBIsStillAudited is
// TestVerify_WrongPasswordWithInvalidDBIsStillAudited's counterpart for an
// unknown username rather than a known one with the wrong password — the
// other credential-rejection path (acquireCredential, not compareAuthData),
// which the fix has to cover the same way.
func TestVerify_UnknownUserWithInvalidDBIsStillAudited(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Connecting as an unconfigured user against a nonexistent database")
	dsn := fmt.Sprintf("attacker:x@tcp(%s)/no_such_database?timeout=5s&tls=skip-verify", proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err == nil {
		t.Fatal("Ping as an unconfigured user with a bad database unexpectedly succeeded")
	}

	reason := waitForAuthFailEvent(t, sink, "attacker", 5*time.Second)
	if reason == "" {
		t.Fatal("auth_fail event has an empty reason")
	}
}

// TestVerify_UnknownUserReasonIsNormalized verifies the stored reason for an
// unknown-user rejection is legible and names the attempted user. It is the
// database's own "Access denied" text now (the database is who rejects the
// login); before pass-through the proxy rejected unknown users itself, with
// go-mysql's ER_NO_SUCH_USER message about a stored routine's "definer",
// which would be baffling in an audit log, and this test keeps it from
// coming back.
func TestVerify_UnknownUserReasonIsNormalized(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN("nosuchuser", "whatever", proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err == nil {
		t.Fatal("Ping as an unconfigured user unexpectedly succeeded")
	}

	reason := waitForAuthFailEvent(t, sink, "nosuchuser", 5*time.Second)
	if strings.Contains(reason, "definer") {
		t.Errorf("auth_fail reason still contains go-mysql's confusing raw text: %q", reason)
	}
	if !strings.Contains(reason, "nosuchuser") {
		t.Errorf("auth_fail reason doesn't name the attempted user: %q", reason)
	}
}

// TestVerify_RejectedLoginDoesNotRevealDatabaseExistence covers the second,
// related bug from the same review comment: because UseDB used to run
// against the real upstream during the handshake, before any credential
// check, an unauthenticated client could tell a real database name from a
// made-up one just from how the connection attempt failed — 1049 for a bad
// name, 1045/1449 for bad credentials against a real one. That is a schema
// enumeration channel with no authentication required at all.
//
// With UseDB deferred until after OnAuthSuccess, a rejected login must look
// the same to the client regardless of which database name it named: the
// credential error is all that's ever returned pre-auth, whether the named
// database is real or not.
func TestVerify_RejectedLoginDoesNotRevealDatabaseExistence(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	tryWrongPassword := func(t *testing.T, dbName string) *sqldriver.MySQLError {
		t.Helper()
		dsn := fmt.Sprintf("demo:TOTALLY-WRONG@tcp(%s)/%s?timeout=5s&tls=skip-verify", proxyAddr, dbName)
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer db.Close()

		err = db.Ping()
		if err == nil {
			t.Fatal("Ping with a wrong password unexpectedly succeeded")
		}
		var mysqlErr *sqldriver.MySQLError
		if !errors.As(err, &mysqlErr) {
			t.Fatalf("error is not a *mysql.MySQLError: %v", err)
		}
		return mysqlErr
	}

	t.Log("Step 1: Wrong password against the real 'demo' database")
	realDBErr := tryWrongPassword(t, "demo")

	t.Log("Step 2: The identical wrong password against a made-up database name")
	fakeDBErr := tryWrongPassword(t, "no_such_database")

	if realDBErr.Number != fakeDBErr.Number {
		t.Errorf("error code differs by database existence: real db = %d, fake db = %d — "+
			"an unauthenticated client can tell them apart", realDBErr.Number, fakeDBErr.Number)
	}
	if realDBErr.Number == 1049 || fakeDBErr.Number == 1049 {
		t.Errorf("got ER_BAD_DB_ERROR (1049) pre-authentication: real db = %d, fake db = %d — "+
			"the upstream was consulted about the database before credentials were checked",
			realDBErr.Number, fakeDBErr.Number)
	}
}

// ---------------------------------------------------------------------------
// MySQL 8 default authentication (caching_sha2_password)
// ---------------------------------------------------------------------------

// TestVerify_OffersCachingSha2PasswordByDefault reads the server's initial
// handshake packet directly off the wire — before any client response, so
// before any driver-level auth negotiation happens — and checks the
// advertised default auth plugin is caching_sha2_password, not
// mysql_native_password.
//
// This is the actual, observable claim behind "a default MySQL 8.0 client
// connects without --default-auth": that decision is made client-side, from
// this exact packet, so proving what the packet advertises is a more direct
// test than trying to observe which plugin a *sql.DB happened to negotiate
// (go-sql-driver/mysql supports both and would connect successfully either
// way, silently masking a regression back to mysql_native_password).
//
// The plugin name sits in a variable-length, non-length-prefixed field at
// the end of the handshake packet (see server.Conn.writeInitialHandshake),
// so rather than replicate that byte-for-byte layout here, this just checks
// the raw packet bytes contain the plugin name string — the connection-id
// and salt fields around it are binary/random, so an accidental match is
// not a realistic concern.
func TestVerify_OffersCachingSha2PasswordByDefault(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Dialing the proxy directly and reading the initial handshake packet")
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("read packet header: %v", err)
	}
	length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read handshake payload: %v", err)
	}

	t.Log("Step 2: Confirming caching_sha2_password is the advertised default, not mysql_native_password")
	if !bytes.Contains(payload, []byte(mysql.AUTH_CACHING_SHA2_PASSWORD)) {
		t.Errorf("initial handshake packet does not advertise %q — a default MySQL 8.0+ "+
			"client will fall back to asking for --default-auth instead of connecting "+
			"straight away. Packet: %x", mysql.AUTH_CACHING_SHA2_PASSWORD, payload)
	}
	if bytes.Contains(payload, []byte(mysql.AUTH_NATIVE_PASSWORD)) {
		t.Errorf("initial handshake packet still advertises %q — MySQL 9 removed that "+
			"plugin outright, so a client built against it fails before reaching the proxy "+
			"at all. Packet: %x", mysql.AUTH_NATIVE_PASSWORD, payload)
	}
}

// TestVerify_CachingSha2PasswordLoginsThroughTheProxy logs in as the same user
// twice, from the same client, over TLS. The database owns the caching_sha2
// password cache now, not the proxy: the first login is the database's "full
// authentication" (the client's cleartext password answer, relayed over TLS),
// and the second takes the database's "fast authentication" shortcut, which
// the proxy has to pass on to the client. They are different code in the
// proxy's relay, and in the client's handling of it, so one login alone would
// not exercise the second.
func TestVerify_CachingSha2PasswordLoginsThroughTheProxy(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	connectAndPing := func(step string) {
		t.Helper()
		t.Log(step)
		// A fresh *sql.DB per call, not a shared one reused across both
		// steps: reusing one *sql.DB risks its connection pool keeping the
		// first physical connection alive and simply reusing it for the
		// second Ping, which would silently skip the second login this test
		// exists to force.
		db, err := sql.Open("mysql", proxyDSN("demo", "demopass", proxyAddr))
		if err != nil {
			t.Fatalf("sql.Open: %v", err)
		}
		defer db.Close()
		if err := db.Ping(); err != nil {
			t.Fatalf("Ping failed: %v", err)
		}
	}

	connectAndPing("Step 1: First connection as demo/demopass")
	connectAndPing("Step 2: Second connection as demo/demopass")
}
