package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqldriver "github.com/go-sql-driver/mysql"

	"github.com/tevans2/mysql-audit-proxy/internal/proxy/config"
)

// Tests for Proxy.Run's context-cancellation lifecycle and session tracking —
// the "stoppable listeners" step of live-config reconciliation.
// These deliberately need no real upstream MySQL server:
// Run only ever touches the upstream once a client logs in, and most of what's
// being tested here (ctx cancellation stopping Accept, a closed listener not
// being logged as an accept error) never gets that far. The tests that
// exercise an in-flight session (TestVerify_ActiveSessions...) use a client
// that connects and then says nothing, so the session stays open exactly
// until the test ends it, rather than racing a real server's timing.

// recordingWriter captures every line written to it via a *log.Logger, so a
// test can assert on exact log content. testLogger (used elsewhere in this
// package) only forwards to t.Log and keeps nothing around to inspect
// afterwards, which is enough for narration but not for asserting that a
// specific line was (or wasn't) logged.
type recordingWriter struct {
	mu    sync.Mutex
	lines []string
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, string(p))
	return len(p), nil
}

func (w *recordingWriter) contains(substr string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range w.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// newLifecycleTestProxy builds a *Proxy for exercising Run's own start/stop
// lifecycle. upstreamAddr can be any address — for the tests that never
// accept a client connection, it's never dialled at all.
func newLifecycleTestProxy(upstreamAddr string, rec *recordingWriter) *Proxy {
	return &Proxy{
		ListenAddr:   "127.0.0.1:0",
		UpstreamAddr: upstreamAddr,
		UpstreamName: "lifecycle-test",
		UpstreamUser: "root",
		UpstreamPass: "unused",
		Sink:         &testSink{},
		Logger:       log.New(rec, "", 0),
	}
}

// TestVerify_ContextCancellationStopsAccept is the direct test for the
// ticket's "Closing a listener must stop the accept loop" requirement:
// cancelling ctx must make Run actually return, promptly, and the listener
// must genuinely be gone afterwards — not just have Run return while
// something is still bound to the port.
func TestVerify_ContextCancellationStopsAccept(t *testing.T) {
	rec := &recordingWriter{}
	p := newLifecycleTestProxy("127.0.0.1:1", rec) // port 1: never dialled here

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Listener, 1)
	runErr := make(chan error, 1)

	t.Log("Step 1: Starting the proxy")
	go func() { runErr <- p.Run(ctx, ready) }()
	listener := <-ready
	addr := listener.Addr().String()

	t.Log("Step 2: Cancelling ctx")
	cancel()

	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of ctx being cancelled — the accept loop is still blocked")
	}

	t.Log("Step 3: Confirming the listener is actually closed to new connections")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("dial succeeded against a proxy whose context was cancelled — the listener was not actually closed")
	}
}

// TestVerify_ClosedListenerNotLoggedAsAcceptError is the direct test for the
// ticket's "net.ErrClosed during intentional shutdown must not be logged as
// an operational failure" requirement.
func TestVerify_ClosedListenerNotLoggedAsAcceptError(t *testing.T) {
	rec := &recordingWriter{}
	p := newLifecycleTestProxy("127.0.0.1:1", rec)

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Listener, 1)
	runErr := make(chan error, 1)

	t.Log("Step 1: Starting the proxy, then cancelling ctx")
	go func() { runErr <- p.Run(ctx, ready) }()
	<-ready
	cancel()

	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}

	t.Log("Step 2: Confirming the shutdown was not logged as an accept error")
	if rec.contains("accept error") {
		t.Errorf("intentional shutdown via ctx cancellation was logged as an accept error: %v", rec.lines)
	}
}

// TestVerify_ActiveSessionsTracksSessionLifecycle is the direct test for the
// ticket's "Active sessions must be tracked" requirement: ActiveSessions
// must reflect a session that is genuinely in flight, and WaitSessions must
// actually block while one is still open and return once it ends — the
// primitive a later drain step needs (see the field comments on Proxy).
func TestVerify_ActiveSessionsTracksSessionLifecycle(t *testing.T) {
	rec := &recordingWriter{}
	p := newLifecycleTestProxy("127.0.0.1:1", rec) // never contacted: nobody logs in

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan net.Listener, 1)
	go func() { p.Run(ctx, ready) }()
	listener := <-ready
	addr := listener.Addr().String()

	if got := p.ActiveSessions(); got != 0 {
		t.Fatalf("ActiveSessions before any connection = %d, want 0", got)
	}

	t.Log("Step 1: Opening a client connection that never answers the handshake — handleClient waits for it")
	clientConn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { clientConn.Close() })

	t.Log("Step 2: Waiting for ActiveSessions to reflect the in-flight session")
	deadline := time.Now().Add(2 * time.Second)
	for p.ActiveSessions() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.ActiveSessions(); got != 1 {
		t.Fatalf("ActiveSessions while the session is open = %d, want 1", got)
	}

	t.Log("Step 3: WaitSessions must not return early while the session is still open")
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer shortCancel()
	if err := p.WaitSessions(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitSessions with an open session returned %v, want context.DeadlineExceeded", err)
	}

	t.Log("Step 4: Ending the session from the client side")
	clientConn.Close()

	t.Log("Step 5: Confirming ActiveSessions drops back to 0 and WaitSessions returns")
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	if err := p.WaitSessions(waitCtx); err != nil {
		t.Fatalf("WaitSessions after the session ended: %v", err)
	}
	if got := p.ActiveSessions(); got != 0 {
		t.Fatalf("ActiveSessions after the session ended = %d, want 0", got)
	}
}

// TestVerify_SilentClientIsDroppedAtTheLoginDeadline connects and says nothing.
// The handshake must not wait for ever: the session ends at the proxy's login
// timeout, the client connection is closed, and ActiveSessions drops back to 0.
func TestVerify_SilentClientIsDroppedAtTheLoginDeadline(t *testing.T) {
	rec := &recordingWriter{}
	p := newLifecycleTestProxy("127.0.0.1:1", rec)
	p.LoginTimeout = 300 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan net.Listener, 1)
	go func() { p.Run(ctx, ready) }()
	listener := <-ready

	clientConn, err := net.DialTimeout("tcp", listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { clientConn.Close() })

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	deadline := time.Now().Add(2 * time.Second)
	for p.ActiveSessions() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.WaitSessions(waitCtx); err != nil {
		t.Fatalf("a silent client still holds a session well after the login timeout: %v", err)
	}
	if got := p.ActiveSessions(); got != 0 {
		t.Fatalf("ActiveSessions after the login timeout = %d, want 0", got)
	}
}

// Cancelling a listener that is bound but has not started accepting yet (its
// accept gate is still closed) must still release the port.

func TestRun_CancelledWhileAcceptGateIsClosedReleasesThePort(t *testing.T) {
	for i := 0; i < 60; i++ {
		gate := make(chan struct{})
		p := &Proxy{
			ListenAddr:   "127.0.0.1:0",
			UpstreamAddr: "db.example:3306",
			UpstreamName: "s1",
			UpstreamUser: "root",
			UpstreamPass: "pw",
			Logger:       log.New(io.Discard, "", 0),
			acceptGate:   gate,
		}
		ctx, cancel := context.WithCancel(context.Background())
		ready := make(chan net.Listener, 1)
		done := make(chan error, 1)
		go func() { done <- p.Run(ctx, ready) }()

		var addr string
		select {
		case l := <-ready:
			addr = l.Addr().String()
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("proxy did not start")
		}

		// Cancel straight away: Run is waiting on the gate, not accepting.
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after cancellation while waiting on the accept gate")
		}

		// The moment Run has returned, its port must be free.
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("iteration %d: %s is still bound after Run returned: %v", i, addr, err)
		}
		l.Close()
	}
}

// Tests for Proxy.Drain's forced-close path — the ticket's "close remaining
// sessions after the configured drain timeout" requirement. The existing
// drain-adjacent coverage (reconciler_test.go's UpdateInPlace test,
// session_test.go's ActiveSessions test, and the E2E delete-drain
// test) all exercise a session that finishes on its own before any timeout
// is reached; none of them actually let a drain timeout elapse and check
// that Drain then forces the session closed. This file is that missing
// case, against a real upstream — a session sitting in the command loop
// (post-handshake, idle) is the only kind CloseActiveSessions can actually
// unblock: closing the client-facing connection only helps once something
// is reading from it, and a session still stuck in its initial upstream
// connect (as in session_test.go's silent-upstream tests) is not.

// newDrainTestProxy is startTestProxy's (helpers_test.go) sibling: same
// construction, but it hands back the *Proxy itself, which
// TestVerify_DrainForciblyClosesSessionAfterTimeout needs in order to call
// Drain and ActiveSessions directly rather than only dialling its address.
func newDrainTestProxy(t *testing.T, upstreamAddr, upstreamUser, upstreamPass string) (proxyAddr string, proxy *Proxy) {
	t.Helper()
	ready := make(chan net.Listener, 1)

	done := &atomic.Bool{}
	t.Cleanup(func() { done.Store(true) })

	proxy = &Proxy{
		TLS:          config.GetEnv("PROXY_TEST_TLS", "skip-verify"),
		TLSCAFile:    os.Getenv("PROXY_TEST_TLS_CA_FILE"),
		ListenAddr:   "127.0.0.1:0",
		UpstreamAddr: upstreamAddr,
		UpstreamName: "drain-test-db",
		UpstreamUser: upstreamUser,
		UpstreamPass: upstreamPass,
		Sink:         &testSink{},
		Logger:       log.New(testLogger{t, done}, "", 0),
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		if err := proxy.Run(ctx, ready); err != nil && ctx.Err() == nil {
			t.Logf("proxy.Run returned: %v", err)
		}
	}()

	listener := <-ready
	return listener.Addr().String(), proxy
}

// TestVerify_DrainForciblyClosesSessionAfterTimeout is the direct test for
// "close remaining sessions after the configured drain timeout": a session
// with a long-running query in flight, given a drain timeout shorter than
// that query, must be forcibly disconnected once the timeout elapses —
// Drain must not wait for it to finish on its own.
func TestVerify_DrainForciblyClosesSessionAfterTimeout(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, proxy := newDrainTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("opening %s: %v", proxyAddr, err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)

	t.Log("Step 1: priming the session so the drain below has something real to force-close")
	if err := db.Ping(); err != nil {
		t.Fatalf("priming the session: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for proxy.ActiveSessions() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := proxy.ActiveSessions(); got != 1 {
		t.Fatalf("ActiveSessions before the long query = %d, want 1", got)
	}

	t.Log("Step 2: starting a query that outlives the drain timeout below")
	queryErr := make(chan error, 1)
	go func() {
		var discard int
		queryErr <- db.QueryRow("SELECT SLEEP(5)").Scan(&discard)
	}()
	// Give SELECT SLEEP a moment to actually be in flight on the upstream
	// before draining, so this is genuinely testing an in-flight session
	// rather than racing the query's own start.
	time.Sleep(300 * time.Millisecond)

	t.Log("Step 3: draining with a timeout far shorter than the in-flight query")
	const drainTimeout = 500 * time.Millisecond
	start := time.Now()
	proxy.Drain(drainTimeout)
	elapsed := time.Since(start)

	if elapsed < drainTimeout {
		t.Errorf("Drain returned after %s, before its own %s timeout had even elapsed", elapsed, drainTimeout)
	}
	if elapsed > drainTimeout+forceCloseGracePeriod+2*time.Second {
		t.Errorf("Drain took %s to force-close a session past a %s timeout — far too slow", elapsed, drainTimeout)
	}
	if got := proxy.ActiveSessions(); got != 0 {
		t.Errorf("ActiveSessions after Drain = %d, want 0 (the session should have been forced closed)", got)
	}

	t.Log("Step 4: the in-flight query must have been severed, not left to finish 5s later")
	select {
	case err := <-queryErr:
		if err == nil {
			t.Error("in-flight query completed successfully despite the session being force-closed mid-query")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight query's goroutine never returned after Drain forcibly closed its session")
	}
}

// Tests for the bootstrap account (the upstream user/password in a server's
// configuration): the powerless login the proxy opens each upstream connection
// with before switching it to the connecting client's own identity. Clients
// log in as themselves, so it has nothing to do with who may connect, but a
// live configuration change must still reach new connections.

// startIdleProxy starts a proxy far enough that its first upstream snapshot
// exists. It never accepts a real client.
func startIdleProxy(t *testing.T) *Proxy {
	t.Helper()
	p := &Proxy{
		ListenAddr:   "127.0.0.1:0",
		UpstreamAddr: "db.example:3306",
		UpstreamName: "s1",
		UpstreamUser: "bootstrap",
		UpstreamPass: "pw1",
		Logger:       log.New(io.Discard, "", 0),
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Listener, 1)
	go p.Run(ctx, ready)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("proxy did not start")
	}
	t.Cleanup(cancel)
	return p
}

func TestUpdateUpstream_ReplacesBootstrapCredentialsWithoutTouchingTheOldSnapshot(t *testing.T) {
	p := startIdleProxy(t)
	before := p.upstream.Load()
	if before.User != "bootstrap" || before.Pass != "pw1" {
		t.Fatalf("initial snapshot has bootstrap %q/%q, want bootstrap/pw1", before.User, before.Pass)
	}

	if err := p.UpdateUpstream("db.example:3306", "s1", "bootstrap2", "pw2"); err != nil {
		t.Fatalf("UpdateUpstream: %v", err)
	}
	after := p.upstream.Load()
	if after.User != "bootstrap2" || after.Pass != "pw2" {
		t.Fatalf("snapshot after the update has bootstrap %q/%q, want bootstrap2/pw2", after.User, after.Pass)
	}
	// A session in flight holds a pointer to the snapshot it was accepted
	// under, so the update must replace it, never edit it in place.
	if before == after || before.User != "bootstrap" || before.Pass != "pw1" {
		t.Fatal("UpdateUpstream modified the snapshot a running session may be using")
	}
}

// TestUpdateUpstream_NewSessionsUseNewBootstrapCredentials is the live version:
// a proxy whose bootstrap password is wrong cannot log anybody in (the client
// gets a generic error that does not mention the account), and fixing the
// password through UpdateUpstream lets the very next login through, with no
// restart.
func TestUpdateUpstream_NewSessionsUseNewBootstrapCredentials(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)

	var proxy *Proxy
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, "not-the-bootstrap-password", func(p *Proxy) { proxy = p })

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	t.Log("Step 1: login fails while the bootstrap password is wrong, without naming the account")
	err = db.Ping()
	var mysqlErr *sqldriver.MySQLError
	if !errors.As(err, &mysqlErr) {
		t.Fatalf("Ping with a broken bootstrap credential: got %v, want a MySQL error", err)
	}
	if strings.Contains(mysqlErr.Message, upstreamUser) || strings.Contains(mysqlErr.Message, "Access denied") {
		t.Errorf("the client was told about the bootstrap account: %q", mysqlErr.Message)
	}

	t.Log("Step 2: UpdateUpstream installs the right password; the next login succeeds")
	if err := proxy.UpdateUpstream(upstreamAddr, "test-db", upstreamUser, upstreamPass); err != nil {
		t.Fatalf("UpdateUpstream: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping after the bootstrap credential was corrected: %v", err)
	}
}

// RunAllNotify must call onBound only once every listener is up: readiness
// reported earlier would tell a probe the proxy can take connections before it
// can.
func TestRunAllNotifyCallsOnBoundAfterEveryListenerIsUp(t *testing.T) {
	rec := &recordingWriter{}
	proxies := []*Proxy{
		newLifecycleTestProxy("127.0.0.1:1", rec),
		newLifecycleTestProxy("127.0.0.1:1", rec),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bound := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- RunAllNotify(ctx, proxies, func() { close(bound) }) }()

	select {
	case <-bound:
	case err := <-done:
		t.Fatalf("RunAllNotify returned before onBound: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("onBound was not called within 5s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunAllNotify did not return after cancel")
	}
}
