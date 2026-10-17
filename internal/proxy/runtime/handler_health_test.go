package runtime

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"

	"lustre/internal/audit"
)

// ---------------------------------------------------------------------------
// 5. A lost upstream must end the client session.
// ---------------------------------------------------------------------------

// TestVerify_UpstreamLossClosesClientSession kills the upstream, then issues one
// query. The client should be told it failed and the session should then close,
// rather than staying open with every subsequent query failing.
func TestVerify_UpstreamLossClosesClientSession(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)
	admin := upstreamAdmin(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN(upstreamUser, upstreamPass, proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	clientConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin client connection: %v", err)
	}
	defer clientConn.Close()

	var upstreamID uint64
	if err := clientConn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&upstreamID); err != nil {
		t.Fatalf("SELECT CONNECTION_ID(): %v", err)
	}
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}

	t.Logf("Step 2: Killing upstream connection ID %d via admin", upstreamID)
	if _, err := admin.Exec(fmt.Sprintf("KILL %d", upstreamID)); err != nil {
		t.Logf("KILL %d returned: %v (continuing)", upstreamID, err)
	}
	time.Sleep(300 * time.Millisecond)

	t.Log("Step 3: Attempting query on killed session")
	// This one should fail.
	if err := clientConn.QueryRowContext(ctx, "SELECT 1").Scan(new(int)); err == nil {
		t.Fatal("query after the upstream kill unexpectedly succeeded")
	}

	t.Log("Step 4: Waiting for proxy to emit disconnect event")
	// The client closed nothing, so a disconnect can only come from the proxy.
	if !sink.waitForEvent(audit.KindDisconnect, 5*time.Second) {
		t.Error("no disconnect event after the upstream was lost — the client session is still open with a dead upstream behind it")
	}
}

// TestVerify_IdleSessionDetectsUpstreamLoss is
// TestVerify_UpstreamLossClosesClientSession's idle counterpart: the upstream
// is killed with no follow-up query at all. Only watchUpstream's own health
// check can notice this — HandleQuery/UseDB never run, so the reactive path
// above does not apply here. A regression here means an idle client is left
// holding a dead upstream connection open indefinitely, with no disconnect
// ever emitted.
func TestVerify_IdleSessionDetectsUpstreamLoss(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)
	admin := upstreamAdmin(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN(upstreamUser, upstreamPass, proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	clientConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin client connection: %v", err)
	}
	defer clientConn.Close()

	var upstreamID uint64
	if err := clientConn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&upstreamID); err != nil {
		t.Fatalf("SELECT CONNECTION_ID(): %v", err)
	}
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}

	t.Logf("Step 2: Killing upstream connection ID %d while idle", upstreamID)
	if _, err := admin.Exec(fmt.Sprintf("KILL %d", upstreamID)); err != nil {
		t.Logf("KILL %d returned: %v (continuing)", upstreamID, err)
	}

	// No follow-up query — the idle session has to be caught by
	// watchUpstream's own ping, not by a command surfacing the failure.
	// Comfortably longer than upstreamHealthCheckInterval (3s) to allow for
	// a couple of missed ticks under load rather than chasing the exact
	// interval.
	t.Log("Step 3: Waiting for proxy's background watcher to emit disconnect event")
	if !sink.waitForEvent(audit.KindDisconnect, 10*time.Second) {
		t.Error("no disconnect event for an idle session after the upstream was lost — " +
			"the client never sent another command, so only the health-check watcher could have caught this")
	}
}

// Regression test for a hung (not closed) upstream connection — reported in
// review of the disconnect-handling fix: a wedged upstream that keeps the TCP
// connection open but never answers a command is a different failure mode
// from a closed/reset connection, and needs its own coverage. Unlike the rest
// of this package's integration tests, this one needs no real MySQL server —
// the fake upstream below is an in-process go-mysql server, so it runs
// unconditionally.

// hangingUpstreamHandler completes the MySQL handshake normally, like a real
// server, but then blocks forever on every query — simulating a wedged
// mysqld (deadlocked, stuck on a lock, whatever) rather than one that has
// closed or reset the connection. unblock lets the test end the blocked
// goroutine on cleanup instead of leaking it past the test.
type hangingUpstreamHandler struct {
	acceptsChangeUser
	unblock <-chan struct{}
}

// acceptsChangeUser is a passive handler that accepts COM_CHANGE_USER. The
// proxy logs every client in by opening the connection as its bootstrap
// account and then switching it to the client's own identity, so any fake
// upstream has to say OK to that before it can go on to misbehave. (Returning
// nil from HandleOtherCommand makes go-mysql answer with an OK packet.)
type acceptsChangeUser struct{ server.EmptyHandler }

func (h acceptsChangeUser) HandleOtherCommand(cmd byte, data []byte) error {
	if cmd == mysql.COM_CHANGE_USER {
		return nil
	}
	return h.EmptyHandler.HandleOtherCommand(cmd, data)
}

// connectThroughProxy logs in to the proxy with go-mysql's client over TLS
// (the proxy refuses a plaintext login), without checking its self-signed
// certificate.
func connectThroughProxy(addr, user, pass string) (*client.Conn, error) {
	return client.Connect(addr, user, pass, "", func(c *client.Conn) error {
		c.SetTLSConfig(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
		return nil
	})
}

func (h *hangingUpstreamHandler) HandleQuery(query string) (*mysql.Result, error) {
	<-h.unblock // never sent during the test — this is the point
	return nil, mysql.NewError(mysql.ER_UNKNOWN_ERROR, "test upstream unblocked at cleanup")
}

// startHangingUpstream starts an in-process MySQL server that accepts one
// connection, completes the handshake, and then never responds to a query.
// Returns its address and credentials, matching testUpstreamConfig's shape
// so it can stand in for a real upstream in startTestProxy.
func startHangingUpstream(t *testing.T) (addr, user, pass string) {
	t.Helper()
	// These in-process mocks generate their own untrusted certificates.
	t.Setenv("PROXY_TEST_TLS", "skip-verify")
	t.Setenv("PROXY_TEST_TLS_CA_FILE", "")
	user, pass = "root", "hangpass"

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	authHandler := server.NewInMemoryAuthenticationHandler("mysql_native_password")
	if err := authHandler.AddUser(user, pass); err != nil {
		t.Fatalf("add user: %v", err)
	}

	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })

	svr := server.NewDefaultServer()

	go func() {
		// One connection is all this test opens; Accept again afterwards
		// only to exit cleanly (with an error) once the listener closes
		// during cleanup, rather than leaking this goroutine.
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		h := &hangingUpstreamHandler{unblock: unblock}
		c, err := svr.NewCustomizedConn(conn, authHandler, h)
		if err != nil {
			conn.Close()
			return
		}
		for {
			if err := c.HandleCommand(); err != nil {
				return
			}
		}
	}()

	return listener.Addr().String(), user, pass
}

// TestVerify_HungUpstreamDetected uses the fallback path in watchUpstream
// directly (UpstreamReadTimeout left at its zero value — disabled — by
// startTestProxy), isolating it from the read-timeout fix that normally
// catches a hang first in production. Without that fallback, a query against
// a hung-but-not-closed upstream blocks forever: the command loop never
// returns from Execute, so nothing ever closes the session or emits a
// disconnect.
func TestVerify_HungUpstreamDetected(t *testing.T) {
	hungAddr, hungUser, hungPass := startHangingUpstream(t)
	proxyAddr, sink := startTestProxy(t, hungAddr, hungUser, hungPass, func(p *Proxy) {
		p.UpstreamCommandTimeout = 2 * time.Second
	})

	db, err := sql.Open("mysql", proxyDSN(hungUser, hungPass, proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// sql.Open is lazy — nothing has connected yet, so force it now. This
	// succeeds quickly: the proxy's bootstrap login, its switch to the
	// client's identity and the client-side handshake all complete fine
	// against the fake upstream,
	// which only hangs once a query actually reaches HandleQuery. Ping
	// itself never reaches the upstream at all (COM_PING is answered by the
	// go-mysql server library directly), so it can't hang here regardless.
	t.Log("Step 1: Connecting client to wedged server")
	if err := db.Ping(); err != nil {
		t.Fatalf("db.Ping through proxy: %v", err)
	}
	t.Log("Step 2: Waiting for connect event")
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}

	// The fake upstream never answers this. ctx is an outer safety net, not
	// the expected path — it should fire well before this, once the watcher's
	// tick notices commandStartedAt has aged past upstreamHangFallback.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t.Log("Step 3: Executing query against wedged server (should hang)")
	err = db.QueryRowContext(ctx, "SELECT 1").Scan(new(int))
	if err == nil {
		t.Fatal("query against a hung upstream unexpectedly succeeded")
	}
	if ctx.Err() != nil {
		t.Fatal("query against a hung upstream did not return until the test's own 15s deadline — " +
			"the watcher never noticed and unblocked it")
	}
	t.Logf("query against hung upstream failed as expected: %v", err)

	t.Log("Step 4: Waiting for proxy to detect hang and emit disconnect")
	if !sink.waitForEvent(audit.KindDisconnect, 5*time.Second) {
		t.Error("no disconnect event after the upstream hung — the client session may still be open")
	}
}

// startPingHangingUpstream starts an in-process MySQL server that accepts one
// connection, completes the handshake, and then never reads from the socket,
// which blocks any subsequent requests (like COM_PING).
func startPingHangingUpstream(t *testing.T) (addr, user, pass string) {
	t.Helper()
	// These in-process mocks generate their own untrusted certificates.
	t.Setenv("PROXY_TEST_TLS", "skip-verify")
	t.Setenv("PROXY_TEST_TLS_CA_FILE", "")
	user, pass = "root", "hangpass"

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	authHandler := server.NewInMemoryAuthenticationHandler("mysql_native_password")
	if err := authHandler.AddUser(user, pass); err != nil {
		t.Fatalf("add user: %v", err)
	}

	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })

	svr := server.NewDefaultServer()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		c, err := svr.NewCustomizedConn(conn, authHandler, acceptsChangeUser{})
		if err != nil {
			conn.Close()
			return
		}
		// Handshake complete. Answer exactly one command, the proxy's
		// COM_CHANGE_USER, then never call HandleCommand() again, so it
		// never reads the COM_PING packet!
		if err := c.HandleCommand(); err != nil {
			return
		}
		<-unblock
	}()

	return listener.Addr().String(), user, pass
}

// TestVerify_PingHungUpstreamDetected verifies that if a server completes the
// handshake but then wedges (never responds to COM_PING), the watcher detects
// it via its specific ping deadline and terminates the session, even when
// UpstreamReadTimeout is disabled (0).
func TestVerify_PingHungUpstreamDetected(t *testing.T) {
	hungAddr, hungUser, hungPass := startPingHangingUpstream(t)
	proxyAddr, sink := startTestProxy(t, hungAddr, hungUser, hungPass)

	t.Log("Step 1: Connecting client to wedged server")
	c, err := connectThroughProxy(proxyAddr, hungUser, hungPass)
	if err != nil {
		t.Fatalf("client.Connect through proxy: %v", err)
	}
	defer c.Close()

	t.Log("Step 2: Waiting for connect event")
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}

	// At this point, the client is idle. The proxy watcher will try to Ping()
	// the upstream. Since startPingHangingUpstream never reads COM_PING, the
	// proxy's ping will time out after upstreamHealthCheckInterval (3s) and it
	// will close the connection.

	t.Log("Step 3: Waiting for proxy to detect ping timeout and emit disconnect")
	if !sink.waitForEvent(audit.KindDisconnect, 8*time.Second) {
		t.Error("no disconnect event after the upstream ping hung — the client session is still open")
	}
}

// TestVerify_PingHungUpstreamDetectedWithReadTimeout verifies that the health-check
// ping deadline is strictly enforced even when UpstreamReadTimeout is set to a
// larger value. go-mysql's ReadPacket dynamically sets deadlines that would
// normally override ours; this ensures our concurrent timeout wrapper successfully
// unblocks it.
func TestVerify_PingHungUpstreamDetectedWithReadTimeout(t *testing.T) {
	hungAddr, hungUser, hungPass := startPingHangingUpstream(t)
	proxyAddr, sink := startTestProxy(t, hungAddr, hungUser, hungPass, func(p *Proxy) {
		p.UpstreamReadTimeout = 10 * time.Second
	})

	t.Log("Step 1: Connecting client to wedged server")
	c, err := connectThroughProxy(proxyAddr, hungUser, hungPass)
	if err != nil {
		t.Fatalf("client.Connect through proxy: %v", err)
	}
	defer c.Close()

	t.Log("Step 2: Waiting for connect event")
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}

	t.Log("Step 3: Waiting for proxy to detect ping timeout (must be < 8s, ignoring the 10s read timeout)")
	if !sink.waitForEvent(audit.KindDisconnect, 8*time.Second) {
		t.Error("no disconnect event after the upstream ping hung — the ping deadline was likely overwritten by the 10s read timeout")
	}
}
