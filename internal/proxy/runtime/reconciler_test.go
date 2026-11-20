package runtime

// Tests for Reconciler.Apply — turning Manager-produced PlanItems into real
// listener bind/update/drain/remove operations (see reconciler.go's package
// comment). These are the "lifecycle tests" and "E2E"-flavoured cases from
// the ticket's own test list that specifically need a real *Proxy and a
// real listener, as opposed to manager_test.go's pure in-memory diffing
// tests or ../config/configclient_test.go's pure HTTP-client tests.
//
// Like session_test.go, these need no real upstream MySQL server:
// reconcilerSilentUpstream accepts a connection and then stays silent, which
// is enough to prove which upstream a session actually reached without
// needing a full MySQL handshake against it. The proxy only contacts its
// upstream once a client has started logging in, so loginProbe below starts
// one (and then waits for an answer that never comes).

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/packet"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/proxy/config"
)

// reconcilerSilentUpstream accepts exactly one TCP connection and then goes
// silent, handing the accepted net.Conn back over a channel — a reconciler
// test needs to assert *which* of two upstreams a session's connection
// attempt actually reached.
func reconcilerSilentUpstream(t *testing.T) (addr string, accepted chan net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	ch := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		ch <- conn
	}()
	return listener.Addr().String(), ch
}

// loginProbe dials a Reconciler-managed listener and starts a login as the
// user "probe": it reads the greeting, switches to TLS and sends a handshake
// response. That is as far as a client has to get for the proxy to open its
// connection to the upstream, which is what these tests look for. The
// returned connection is left open (closed on test cleanup).
func loginProbe(t *testing.T, port int) net.Conn {
	t.Helper()
	raw := dialProxy(t, port)
	t.Cleanup(func() { raw.Close() })
	_ = raw.SetDeadline(time.Now().Add(3 * time.Second))

	pc := packet.NewConn(raw)
	if _, err := pc.ReadPacket(); err != nil { // the server greeting
		t.Fatalf("reading the proxy's greeting on port %d: %v", port, err)
	}
	const caps = mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SSL
	header := func() []byte {
		b := make([]byte, 4+32) // packet header slot, then caps, max packet, charset, 23 reserved
		binary.LittleEndian.PutUint32(b[4:], caps)
		b[4+8] = mysql.DEFAULT_COLLATION_ID
		return b
	}
	if err := pc.WritePacket(header()); err != nil { // SSLRequest
		t.Fatalf("sending the SSL request: %v", err)
	}
	tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake with the proxy: %v", err)
	}
	pc2 := packet.NewConn(tc)
	pc2.Sequence = pc.Sequence

	resp := append(header(), "probe"...)
	resp = append(resp, 0, 0) // end of user name, empty auth response
	resp = append(resp, mysql.AUTH_CACHING_SHA2_PASSWORD...)
	resp = append(resp, 0)
	if err := pc2.WritePacket(resp); err != nil {
		t.Fatalf("sending the handshake response: %v", err)
	}
	_ = raw.SetDeadline(time.Time{})
	return tc
}

// splitHostPortInt is net.SplitHostPort plus strconv.Atoi, for building an
// audit.Server (which wants UpstreamHost and UpstreamPort separately) from
// a reconcilerSilentUpstream/freePort address string.
func splitHostPortInt(t *testing.T, addr string) (host string, port int) {
	t.Helper()
	h, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return h, p
}

// dialProxy connects to a Reconciler-managed listener bound (via
// newManagedProxy) to 0.0.0.0:port.
func dialProxy(t *testing.T, port int) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy on port %d: %v", port, err)
	}
	return conn
}

// newTestReconciler returns a Reconciler configured with short timeouts, so
// a test that exercises a failed bind or a drain doesn't wait out
// production-sized defaults.
func newTestReconciler() *Reconciler {
	return &Reconciler{
		Sink:         &testSink{},
		DrainTimeout: 2 * time.Second,
		BindTimeout:  2 * time.Second,
		listeners:    make(map[int]*managedListener),
	}
}

// TestReconciler_ApplyAddBindsListenerAndAcceptsConnections is the "Add
// listener" case at the Apply level (the ticket's E2E "POST starts a
// listener without restarting the proxy" acceptance criterion, minus the
// API/HTTP layer a later chunk adds): a fresh Reconciler, given an
// ActionAdd item, actually binds the configured port and accepts a real
// TCP connection on it.
func TestReconciler_ApplyAddBindsListenerAndAcceptsConnections(t *testing.T) {
	r := newTestReconciler()
	upstreamAddr, upstreamAccepted := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	listenPort := freePort(t)

	desired := audit.Server{
		ID: 1, Name: "s1", ListenPort: listenPort,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}

	results := r.Apply(context.Background(), []PlanItem{{ServerID: 1, Action: ActionAdd, Desired: desired}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("Apply(Add) = %+v, want one successful result", results)
	}
	if ids := r.ManagedServerIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ManagedServerIDs() = %v, want [1]", ids)
	}

	clientConn := loginProbe(t, listenPort)
	t.Cleanup(func() { clientConn.Close() })

	select {
	case <-upstreamAccepted:
	case <-time.After(2 * time.Second):
		t.Fatal("newly added listener never reached the configured upstream")
	}
}

// TestReconciler_ApplyMovePortPreservesOldListenerWhenNewPortBindFails is
// the direct test for the ticket's "Preserve the old listener if the new
// port cannot be bound" requirement: pre-occupy the target port with
// something else, so the bind Apply attempts is guaranteed to fail, and
// confirm the original listener is completely undisturbed by the attempt.
func TestReconciler_ApplyMovePortPreservesOldListenerWhenNewPortBindFails(t *testing.T) {
	r := newTestReconciler()
	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	oldPort := freePort(t)

	original := audit.Server{
		ID: 1, Name: "s1", ListenPort: oldPort,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}
	addResults := r.Apply(context.Background(), []PlanItem{{ServerID: 1, Action: ActionAdd, Desired: original}})
	if addResults[0].Err != nil {
		t.Fatalf("initial Add: %v", addResults[0].Err)
	}

	// Occupy the port the move is about to try to bind, so it is guaranteed
	// to fail no matter what else is going on in this test's environment.
	// This must bind the same address newManagedProxy will attempt
	// ("0.0.0.0:port", not "127.0.0.1:port") — with SO_REUSEADDR, several
	// platforms (notably macOS) happily let a wildcard bind coexist with an
	// existing loopback-only listener on the same port, so a loopback-only
	// blocker would not reliably collide.
	blocker, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen (blocker): %v", err)
	}
	defer blocker.Close()
	newPort := blocker.Addr().(*net.TCPAddr).Port

	moved := original
	moved.ListenPort = newPort

	moveResults := r.Apply(context.Background(), []PlanItem{{ServerID: 1, Action: ActionMovePort, Desired: moved, Previous: original}})
	if moveResults[0].Err == nil {
		t.Fatal("Apply(MovePort) onto an already-occupied port returned no error")
	}

	if ids := r.ManagedServerIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ManagedServerIDs() after a failed move = %v, want [1] (the old listener must still be managed)", ids)
	}

	// The old listener must still be reachable: a fresh dial to oldPort
	// should still succeed.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort)), 2*time.Second)
	if err != nil {
		t.Fatalf("dial old listener after a failed move: %v", err)
	}
	conn.Close()
}

// TestReconciler_ApplyRemoveDrainsAndRemovesListener is the "Disabled or
// deleted server" case: ActionRemove stops the listener from accepting new
// connections and removes it from the Reconciler's managed set.
func TestReconciler_ApplyRemoveDrainsAndRemovesListener(t *testing.T) {
	r := newTestReconciler()
	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	listenPort := freePort(t)

	desired := audit.Server{
		ID: 1, Name: "s1", ListenPort: listenPort,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}
	addResults := r.Apply(context.Background(), []PlanItem{{ServerID: 1, Action: ActionAdd, Desired: desired}})
	if addResults[0].Err != nil {
		t.Fatalf("initial Add: %v", addResults[0].Err)
	}

	removeResults := r.Apply(context.Background(), []PlanItem{{ServerID: 1, Action: ActionRemove, Previous: desired}})
	if removeResults[0].Err != nil {
		t.Fatalf("Apply(Remove): %v", removeResults[0].Err)
	}
	if ids := r.ManagedServerIDs(); len(ids) != 0 {
		t.Fatalf("ManagedServerIDs() after Remove = %v, want none", ids)
	}

	// The listener must actually be gone — dialling it should fail (or,
	// worst case briefly during shutdown, be refused shortly after).
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(listenPort)), 200*time.Millisecond)
		if err != nil {
			return // expected: nothing is listening anymore
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("removed listener is still accepting connections")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReconciler_ApplyUpdateInPlaceLeavesExistingSessionOnOldUpstream is the
// direct test for the ticket's "Immutable per-session configuration"
// requirement, the central design challenge of this chunk: an
// ActionUpdateInPlace must apply the new upstream to new connections only,
// while a session that connected before the update keeps talking to the
// upstream it originally captured — and must do so with no data race
// (raced by go test -race, which the user's own suite runs).
func TestReconciler_ApplyUpdateInPlaceLeavesExistingSessionOnOldUpstream(t *testing.T) {
	r := newTestReconciler()
	oldAddr, oldAccepted := reconcilerSilentUpstream(t)
	newAddr, newAccepted := reconcilerSilentUpstream(t)
	oldHost, oldPort := splitHostPortInt(t, oldAddr)
	newHost, newPort := splitHostPortInt(t, newAddr)
	listenPort := freePort(t)

	original := audit.Server{
		ID: 1, Name: "s1", ListenPort: listenPort,
		UpstreamHost: oldHost, UpstreamPort: oldPort,
		UpstreamUser: "root", UpstreamPass: "old-pass", Enabled: true,
	}
	addResults := r.Apply(context.Background(), []PlanItem{{ServerID: 1, Action: ActionAdd, Desired: original}})
	if addResults[0].Err != nil {
		t.Fatalf("initial Add: %v", addResults[0].Err)
	}

	t.Log("Step 1: opening a session before the update — it must reach the old upstream")
	existingSession := loginProbe(t, listenPort)
	t.Cleanup(func() { existingSession.Close() })

	select {
	case <-oldAccepted:
	case <-time.After(2 * time.Second):
		t.Fatal("session opened before the update never reached the old upstream")
	}
	select {
	case <-newAccepted:
		t.Fatal("the new upstream received a connection before any update was ever applied")
	case <-time.After(100 * time.Millisecond):
	}

	t.Log("Step 2: applying ActionUpdateInPlace, pointing new connections at the new upstream")
	changed := original
	changed.UpstreamHost = newHost
	changed.UpstreamPort = newPort
	changed.UpstreamPass = "new-pass"

	updateResults := r.Apply(context.Background(), []PlanItem{{ServerID: 1, Action: ActionUpdateInPlace, Desired: changed, Previous: original}})
	if updateResults[0].Err != nil {
		t.Fatalf("Apply(UpdateInPlace): %v", updateResults[0].Err)
	}

	t.Log("Step 3: the already-open session must still be exactly as it was — nothing to assert-fail on the old upstream side, since UpdateUpstream never touches an open connection")
	if _, err := existingSession.Write([]byte{0}); err != nil {
		t.Fatalf("existing session's client-side connection was disturbed by the update: %v", err)
	}

	t.Log("Step 4: a fresh connection after the update must reach the new upstream, not the old one")
	newSession := loginProbe(t, listenPort)
	t.Cleanup(func() { newSession.Close() })

	select {
	case <-newAccepted:
	case <-time.After(2 * time.Second):
		t.Fatal("a connection opened after the update never reached the new upstream")
	}
}

// TestReconciler_ApplyPartiallyFailsAcrossMultipleItemsInOneCall is the
// direct test for the ticket's "One server in a snapshot cannot be applied"
// failure-safety requirement at the Reconciler.Apply level specifically
// (manager_test.go's TestManager_PartiallyFailedReconciliationKeepsFailedItemPending
// covers the same requirement, but only Manager's own bookkeeping, with no
// real listener involved). A single Apply call carrying three items — one
// update, one remove, one add that is guaranteed to fail to bind — must
// still carry out the two good ones independently: the failing item must
// not block, roll back, or otherwise affect the others in the same batch.
func TestReconciler_ApplyPartiallyFailsAcrossMultipleItemsInOneCall(t *testing.T) {
	r := newTestReconciler()

	upstreamAddr1, _ := reconcilerSilentUpstream(t)
	upstreamHost1, upstreamPort1 := splitHostPortInt(t, upstreamAddr1)
	upstreamAddr2, _ := reconcilerSilentUpstream(t)
	upstreamHost2, upstreamPort2 := splitHostPortInt(t, upstreamAddr2)

	listenPort1 := freePort(t)
	listenPort2 := freePort(t)

	server1 := audit.Server{
		ID: 1, Name: "s1", ListenPort: listenPort1,
		UpstreamHost: upstreamHost1, UpstreamPort: upstreamPort1,
		UpstreamUser: "root", UpstreamPass: "pw1", Enabled: true,
	}
	server2 := audit.Server{
		ID: 2, Name: "s2", ListenPort: listenPort2,
		UpstreamHost: upstreamHost1, UpstreamPort: upstreamPort1,
		UpstreamUser: "root", UpstreamPass: "pw2", Enabled: true,
	}

	setup := r.Apply(context.Background(), []PlanItem{
		{ServerID: 1, Action: ActionAdd, Desired: server1},
		{ServerID: 2, Action: ActionAdd, Desired: server2},
	})
	if !AllSucceeded(setup) {
		t.Fatalf("setup Apply (adding server1, server2) had failures: %+v", setup)
	}

	// Guarantee server3's bind fails: occupy the port it will try to use.
	blocker, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen (blocker): %v", err)
	}
	defer blocker.Close()
	occupiedPort := blocker.Addr().(*net.TCPAddr).Port

	server1Updated := server1
	server1Updated.UpstreamHost = upstreamHost2
	server1Updated.UpstreamPort = upstreamPort2
	server1Updated.UpstreamPass = "new-pw1"

	server3 := audit.Server{
		ID: 3, Name: "s3", ListenPort: occupiedPort,
		UpstreamHost: upstreamHost1, UpstreamPort: upstreamPort1,
		UpstreamUser: "root", UpstreamPass: "pw3", Enabled: true,
	}

	items := []PlanItem{
		{ServerID: 1, Action: ActionUpdateInPlace, Desired: server1Updated, Previous: server1},
		{ServerID: 2, Action: ActionRemove, Previous: server2},
		{ServerID: 3, Action: ActionAdd, Desired: server3},
	}
	results := r.Apply(context.Background(), items)
	if len(results) != 3 {
		t.Fatalf("Apply returned %d result(s), want 3: %+v", len(results), results)
	}

	if err := results[0].Err; err != nil {
		t.Errorf("server1's ActionUpdateInPlace failed: %v, want it to succeed independently of server3's failure", err)
	}
	if err := results[1].Err; err != nil {
		t.Errorf("server2's ActionRemove failed: %v, want it to succeed independently of server3's failure", err)
	}
	if results[2].Err == nil {
		t.Error("server3's ActionAdd onto an already-occupied port returned no error")
	}
	if AllSucceeded(results) {
		t.Error("AllSucceeded(results) = true, want false — one of the three items was supposed to fail")
	}

	ids := r.ManagedServerIDs()
	idSet := map[int]bool{}
	for _, id := range ids {
		idSet[id] = true
	}
	if !idSet[1] {
		t.Errorf("ManagedServerIDs() = %v, want server 1 still present (its update succeeded)", ids)
	}
	if idSet[2] {
		t.Errorf("ManagedServerIDs() = %v, want server 2 gone (its remove succeeded)", ids)
	}
	if idSet[3] {
		t.Errorf("ManagedServerIDs() = %v, want server 3 absent (its add failed)", ids)
	}

	// server1's listener must still be reachable — unaffected by server3's
	// failure elsewhere in the same batch.
	conn := dialProxy(t, listenPort1)
	conn.Close()

	// server2's listener must be gone — retried briefly since Run's listener
	// actually closes asynchronously once its context is cancelled (see
	// TestReconciler_ApplyRemoveDrainsAndRemovesListener for the same
	// pattern).
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(listenPort2)), 200*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Error("server2's listener is still accepting connections after a successful ActionRemove")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAllSucceeded is a small direct test of the AllSucceeded helper a
// caller uses to decide whether it's safe to call Manager.RecordRevision
// for a whole snapshot.
func TestAllSucceeded(t *testing.T) {
	if !AllSucceeded(nil) {
		t.Error("AllSucceeded(nil) = false, want true (vacuously all succeeded)")
	}
	if !AllSucceeded([]ApplyResult{{}, {}}) {
		t.Error("AllSucceeded with no errors = false, want true")
	}
	if AllSucceeded([]ApplyResult{{}, {Err: context.Canceled}}) {
		t.Error("AllSucceeded with one error = true, want false")
	}
}

// Tests for configPoller — the direct test for the review's "a failed
// reconcile is never retried" bug: a snapshot that fails to apply must be
// retried on a later poll, even one that reports no change at all (304 Not
// Modified, represented here the same way config.ConfigClient.Poll's onResult
// callback sees it: a nil snapshot and a nil error).

func TestConfigPoller_RetriesFailedSnapshotOn304(t *testing.T) {
	manager := NewManager()
	reconciler := newTestReconciler()
	client := &config.ConfigClient{}
	poller := &configPoller{manager: manager, reconciler: reconciler, client: client}

	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)

	// Occupy the port the snapshot below will try to bind, so applying it
	// is guaranteed to fail — standing in for the review comment's own
	// example, "a port briefly in use".
	blocker, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen (blocker): %v", err)
	}
	occupiedPort := blocker.Addr().(*net.TCPAddr).Port

	snap := &config.ConfigSnapshot{
		Revision: 1,
		ETag:     `"rev-1"`,
		Servers: []audit.Server{{
			ID: 1, Name: "s1", ListenPort: occupiedPort,
			UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
			UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
		}},
	}

	t.Log("Step 1: the first poll delivers a snapshot that fails to apply (its port is occupied)")
	poller.onPollResult(context.Background(), snap, nil)

	if got := manager.AppliedRevision(); got != 0 {
		t.Fatalf("AppliedRevision after a failed apply = %d, want 0 (nothing should have been recorded as applied)", got)
	}
	if ids := reconciler.ManagedServerIDs(); len(ids) != 0 {
		t.Fatalf("ManagedServerIDs after a failed apply = %v, want none", ids)
	}

	t.Log("Step 2: freeing the port, then delivering a 304 (nil snapshot, nil error) — the API reporting no NEW revision, exactly what happens if nobody edits the config again")
	blocker.Close()

	poller.onPollResult(context.Background(), nil, nil)

	t.Log("Step 3: the previously-failed snapshot must have been retried, and must now have succeeded")
	if got := manager.AppliedRevision(); got != 1 {
		t.Fatalf("AppliedRevision after the retrying 304 poll = %d, want 1 (the pending snapshot should have been retried and applied)", got)
	}
	if ids := reconciler.ManagedServerIDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("ManagedServerIDs after the retry = %v, want [1]", ids)
	}

	t.Log("Step 4: a further 304 with nothing pending must be a true no-op")
	before := manager.AppliedRevision()
	poller.onPollResult(context.Background(), nil, nil)
	if got := manager.AppliedRevision(); got != before {
		t.Fatalf("AppliedRevision changed on a 304 with nothing pending: %d -> %d", before, got)
	}
}

// The proxy reports once per reconcile, and heartbeats while idle so the
// back-office can tell a running proxy from a stopped one. A change must not
// wait for the heartbeat window: an operator who saves an edit should see it
// go live within a poll, not within heartbeatInterval.
func TestConfigPoller_ReportsOncePerReconcileAndHeartbeatsWhenIdle(t *testing.T) {
	var reports atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/proxy/status" {
			reports.Add(1)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	manager := NewManager()
	reconciler := newTestReconciler()
	client := &config.ConfigClient{BaseURL: srv.URL, Token: "t"}
	poller := &configPoller{manager: manager, reconciler: reconciler, client: client}

	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)

	snap := &config.ConfigSnapshot{
		Revision: 1,
		ETag:     `"rev-1"`,
		Servers: []audit.Server{{
			ID: 1, Name: "s1", ListenPort: freePort(t),
			UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
			UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
		}},
	}

	t.Log("Step 1: a snapshot that applies cleanly reports exactly once")
	poller.onPollResult(context.Background(), snap, nil)
	if manager.AppliedRevision() != 1 {
		t.Fatalf("AppliedRevision = %d after a clean apply, want 1", manager.AppliedRevision())
	}
	if got := reports.Load(); got != 1 {
		t.Fatalf("reports after one reconcile = %d, want 1", got)
	}

	t.Log("Step 2: idle 304 polls inside the heartbeat window are throttled")
	for i := 0; i < 5; i++ {
		poller.onPollResult(context.Background(), nil, nil)
	}
	if got := reports.Load(); got != 1 {
		t.Errorf("reports after five idle polls = %d, want 1 (the heartbeat is throttled)", got)
	}

	t.Log("Step 3: once the window has passed, an idle poll heartbeats again")
	poller.lastReported = time.Now().Add(-2 * heartbeatInterval)
	poller.onPollResult(context.Background(), nil, nil)
	if got := reports.Load(); got != 2 {
		t.Errorf("reports after the heartbeat window elapsed = %d, want 2", got)
	}
}
