package runtime

// Tests for Reconciler.watch — the "a listener whose Run exits on its own is
// still listed as managed and nothing restarts it" review item — and for the
// other half of that contract: a listener the Reconciler itself retires must
// NOT be restarted.

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
)

// freeTCPPort returns a port that was free a moment ago. The listener is
// closed again before returning, so the caller can bind it.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen for a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// currentListener returns the *managedListener the Reconciler currently has
// for serverID, or nil.
func currentListener(r *Reconciler, serverID int) *managedListener {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listeners[serverID]
}

func canDial(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestReconciler_RestartsListenerThatExitsOnItsOwn(t *testing.T) {
	r := newTestReconciler()

	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	port := freeTCPPort(t)

	srv := audit.Server{
		ID: 1, Name: "s1", ListenPort: port,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}

	t.Log("Step 1: add a server so the Reconciler has a managed, listening proxy for it")
	results := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: srv}})
	if !AllSucceeded(results) {
		t.Fatalf("initial add failed: %+v", results)
	}
	t.Cleanup(func() {
		r.Apply(context.Background(), []PlanItem{{Action: ActionRemove, ServerID: 1, Previous: srv}})
	})

	first := currentListener(r, 1)
	if first == nil {
		t.Fatal("no managed listener for server 1 after a successful add")
	}

	t.Log("Step 2: make its Run exit without going through retire — cancelling it directly stands in for any exit the Reconciler did not ask for")
	first.cancel()

	t.Log("Step 3: watch must notice and replace it with a fresh listener on the same port")
	deadline := time.Now().Add(5 * time.Second)
	var second *managedListener
	for time.Now().Before(deadline) {
		if cur := currentListener(r, 1); cur != nil && cur != first {
			second = cur
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if second == nil {
		t.Fatal("the listener that exited on its own was never replaced")
	}
	if !canDial(port) {
		t.Fatalf("restarted listener is not accepting connections on port %d", port)
	}
}

func TestReconciler_DoesNotRestartListenerItRetiredItself(t *testing.T) {
	r := newTestReconciler()

	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	port := freeTCPPort(t)

	srv := audit.Server{
		ID: 1, Name: "s1", ListenPort: port,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}

	t.Log("Step 1: add, then remove through the Reconciler itself")
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: srv}}); !AllSucceeded(res) {
		t.Fatalf("initial add failed: %+v", res)
	}
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionRemove, ServerID: 1, Previous: srv}}); !AllSucceeded(res) {
		t.Fatalf("remove failed: %+v", res)
	}

	t.Log("Step 2: give watch ample time to wrongly restart it; a correct Reconciler leaves it gone")
	time.Sleep(500 * time.Millisecond)

	if ids := r.ManagedServerIDs(); len(ids) != 0 {
		t.Fatalf("ManagedServerIDs after an intentional remove = %v, want none (watch must not restart a retired listener)", ids)
	}
	if canDial(port) {
		t.Fatalf("port %d still accepts connections after an intentional remove", port)
	}
}

// holdPort binds port on all interfaces as soon as it becomes free, standing
// in for "something else is using this port right now". It returns the
// listener so the caller can release the port later.
func holdPort(t *testing.T, port int) net.Listener {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		l, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
		if err == nil {
			t.Cleanup(func() { l.Close() })
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not take over port %d: %v", port, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A restart that fails (the port is taken) must not be the end of the story:
// the server stays managed, the restart keeps being retried, and it succeeds
// by itself once the port frees up — with no new revision involved.
func TestReconciler_RetriesFailedRestartUntilPortFrees(t *testing.T) {
	r := newTestReconciler()
	r.RestartBackoff = 100 * time.Millisecond
	r.RestartBackoffMax = 200 * time.Millisecond

	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	port := freeTCPPort(t)

	srv := audit.Server{
		ID: 1, Name: "s1", ListenPort: port,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}

	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: srv}}); !AllSucceeded(res) {
		t.Fatalf("initial add failed: %+v", res)
	}
	t.Cleanup(func() {
		r.Apply(context.Background(), []PlanItem{{Action: ActionRemove, ServerID: 1, Previous: srv}})
	})
	first := currentListener(r, 1)

	t.Log("Step 1: the listener dies on its own, and something else takes its port before any restart can")
	first.cancel()
	blocker := holdPort(t, port)

	t.Log("Step 2: several restart attempts fail; the server must still be managed throughout")
	time.Sleep(600 * time.Millisecond)
	if cur := currentListener(r, 1); cur != first {
		t.Fatalf("while the port was unavailable the managed listener changed (%p -> %p); it should stay registered until a restart succeeds", first, cur)
	}

	t.Log("Step 3: the port frees up — with no new revision at all — and the restart must now succeed by itself")
	blocker.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cur := currentListener(r, 1); cur != nil && cur != first {
			if !canDial(port) {
				t.Fatalf("restarted listener is not accepting connections on port %d", port)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the listener was never restarted after its port became free")
}

// A removal that happens while restarts are still being retried must stick:
// the restart must not later reopen a listener that was deliberately removed.
func TestReconciler_RemovalDuringRestartRetriesIsNotUndone(t *testing.T) {
	r := newTestReconciler()
	r.RestartBackoff = 100 * time.Millisecond
	r.RestartBackoffMax = 200 * time.Millisecond

	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	port := freeTCPPort(t)

	srv := audit.Server{
		ID: 1, Name: "s1", ListenPort: port,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}

	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: srv}}); !AllSucceeded(res) {
		t.Fatalf("initial add failed: %+v", res)
	}
	first := currentListener(r, 1)

	t.Log("Step 1: the listener dies on its own and its port is taken, so restarts are failing and being retried")
	first.cancel()
	blocker := holdPort(t, port)
	time.Sleep(300 * time.Millisecond)

	t.Log("Step 2: the server is removed while that is going on")
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionRemove, ServerID: 1, Previous: srv}}); !AllSucceeded(res) {
		t.Fatalf("remove failed: %+v", res)
	}
	if ids := r.ManagedServerIDs(); len(ids) != 0 {
		t.Fatalf("ManagedServerIDs after remove = %v, want none", ids)
	}

	t.Log("Step 3: the port frees up; ample time for a wrongly surviving restart loop to reopen it")
	blocker.Close()
	time.Sleep(800 * time.Millisecond)

	if ids := r.ManagedServerIDs(); len(ids) != 0 {
		t.Fatalf("ManagedServerIDs = %v, want none: a removed server was resurrected by a restart", ids)
	}
	if canDial(port) {
		t.Fatalf("port %d is accepting connections again after the server was removed", port)
	}
}

// An in-place update acknowledged while a restart is under way must not be
// lost: the replacement is built from the configuration captured before the
// update, so it has to pick the update up before it is installed.
func TestReconciler_InPlaceUpdateDuringRestartIsNotLost(t *testing.T) {
	r := newTestReconciler()
	r.RestartBackoff = 50 * time.Millisecond
	r.RestartBackoffMax = 100 * time.Millisecond
	port := freeTCPPort(t)

	reached := make(chan struct{}, 1)
	proceed := make(chan struct{})
	r.beforeRestartInstall = func(*managedListener) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-proceed
	}

	srv := audit.Server{
		ID: 1, Name: "s1", ListenPort: port,
		UpstreamHost: "db-a.example", UpstreamPort: 3306,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
		TLS: audit.TLSSkipVerify,
	}
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: srv}}); !AllSucceeded(res) {
		t.Fatalf("initial add failed: %+v", res)
	}
	t.Cleanup(func() {
		r.Apply(context.Background(), []PlanItem{{Action: ActionRemove, ServerID: 1, Previous: srv}})
	})
	first := currentListener(r, 1)

	t.Log("Step 1: the listener dies on its own; watch builds a replacement from the old settings and is held just before installing it")
	first.cancel()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		close(proceed)
		t.Fatal("watch never reached the point of installing a replacement")
	}

	t.Log("Step 2: an in-place update (verify + new host) is applied and acknowledged while the replacement is waiting")
	updated := srv
	updated.UpstreamHost = "db-b.example"
	updated.TLS = "verify"
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionUpdateInPlace, ServerID: 1, Desired: updated, Previous: srv}}); !AllSucceeded(res) {
		close(proceed)
		t.Fatalf("update-in-place failed: %+v", res)
	}

	t.Log("Step 3: the replacement is released; whatever ends up serving must use the updated settings")
	close(proceed)
	deadline := time.Now().Add(5 * time.Second)
	var second *managedListener
	for time.Now().Before(deadline) {
		if cur := currentListener(r, 1); cur != nil && cur != first {
			second = cur
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if second == nil {
		t.Fatal("the listener was never restarted")
	}
	cfg := second.proxy.upstream.Load()
	if cfg.TLS == nil || cfg.TLS.InsecureSkipVerify {
		t.Fatal("the restarted listener still skips certificate verification: the acknowledged update was lost")
	}
	if cfg.TLS.ServerName != "db-b.example" {
		t.Fatalf("restarted listener TLS ServerName = %q, want %q", cfg.TLS.ServerName, "db-b.example")
	}
	if cfg.Addr != "db-b.example:3306" {
		t.Fatalf("restarted listener upstream = %q, want %q", cfg.Addr, "db-b.example:3306")
	}
	if got := second.getDesired(); got != updated {
		t.Fatalf("restarted listener desired = %+v, want %+v", got, updated)
	}
}

// eventuallyTrue polls cond until it holds or the timeout passes.
func eventuallyTrue(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

// A port move whose new listener finished binding while a restart replaced the
// old listener must retire the listener that is actually displaced (the
// restarted one), not the one it saw earlier — otherwise the restarted listener
// keeps serving the old port with nothing managing it.
func TestReconciler_MovePortDuringRestartRetiresTheListenerActuallyDisplaced(t *testing.T) {
	r := newTestReconciler()
	r.RestartBackoff = 50 * time.Millisecond
	r.RestartBackoffMax = 100 * time.Millisecond

	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	port1 := freeTCPPort(t)
	port2 := freeTCPPort(t)
	for port2 == port1 {
		port2 = freeTCPPort(t)
	}

	srv := audit.Server{
		ID: 1, Name: "s1", ListenPort: port1,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}
	moved := srv
	moved.ListenPort = port2

	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: srv}}); !AllSucceeded(res) {
		t.Fatalf("initial add failed: %+v", res)
	}
	t.Cleanup(func() {
		r.Apply(context.Background(), []PlanItem{{Action: ActionRemove, ServerID: 1, Previous: moved}})
	})
	first := currentListener(r, 1)

	reached := make(chan struct{}, 1)
	proceed := make(chan struct{})
	r.beforeMoveInstall = func() {
		reached <- struct{}{}
		<-proceed
	}

	t.Log("Step 1: start the move; it binds the new port and is held just before installing it")
	applied := make(chan []ApplyResult, 1)
	go func() {
		applied <- r.Apply(context.Background(), []PlanItem{{Action: ActionMovePort, ServerID: 1, Desired: moved, Previous: srv}})
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		close(proceed)
		t.Fatal("the move never reached the point of installing the new listener")
	}

	t.Log("Step 2: meanwhile the old listener dies and watch replaces it")
	first.cancel()
	if !eventuallyTrue(5*time.Second, func() bool { cur := currentListener(r, 1); return cur != nil && cur != first }) {
		close(proceed)
		t.Fatal("the listener was never restarted")
	}

	t.Log("Step 3: the move completes; the old port must stop accepting, whichever listener held it")
	close(proceed)
	select {
	case res := <-applied:
		if !AllSucceeded(res) {
			t.Fatalf("move failed: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the move never completed")
	}
	if !eventuallyTrue(5*time.Second, func() bool { return !canDial(port1) }) {
		t.Fatalf("old port %d still accepts connections after the move: an unmanaged listener was left running", port1)
	}
	if !canDial(port2) {
		t.Fatalf("new port %d is not accepting connections after the move", port2)
	}
}

// Sessions that were open when a listener was restarted still belong to that
// server: removing the server must drain them too, not only the sessions of the
// newest generation.
func TestReconciler_RemoveDrainsSessionsOpenedBeforeARestart(t *testing.T) {
	r := newTestReconciler()
	r.RestartBackoff = 50 * time.Millisecond
	r.RestartBackoffMax = 100 * time.Millisecond
	r.DrainTimeout = 200 * time.Millisecond

	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	port := freeTCPPort(t)

	srv := audit.Server{
		ID: 1, Name: "s1", ListenPort: port,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: srv}}); !AllSucceeded(res) {
		t.Fatalf("initial add failed: %+v", res)
	}
	first := currentListener(r, 1)

	t.Log("Step 1: a session is open on the first generation (stood in for by a tracked connection)")
	c1, c2 := net.Pipe()
	defer c2.Close()
	first.proxy.sessionWG.Add(1)
	first.proxy.activeSessions.Add(1)
	first.proxy.trackConn(c1)
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		first.proxy.untrackConn(c1)
		first.proxy.activeSessions.Add(-1)
		first.proxy.sessionWG.Done()
	}
	defer release()

	t.Log("Step 2: the listener dies on its own and is restarted")
	first.cancel()
	if !eventuallyTrue(5*time.Second, func() bool { cur := currentListener(r, 1); return cur != nil && cur != first }) {
		t.Fatal("the listener was never restarted")
	}

	t.Log("Step 3: remove the server; after the drain timeout the old generation's session must be force-closed")
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionRemove, ServerID: 1, Previous: srv}}); !AllSucceeded(res) {
		t.Fatalf("remove failed: %+v", res)
	}
	_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := c2.Read(make([]byte, 1))
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the session opened before the restart was never closed by the removal (read err: %v)", err)
	}
	release()
}

// A replacement listener must not serve anything until it is installed and has
// been brought up to date: it holds its port, but a connection that arrives in
// that window waits rather than being handled with out-of-date settings.
func TestReconciler_ReplacementDoesNotServeUntilInstalled(t *testing.T) {
	r := newTestReconciler()
	r.RestartBackoff = 50 * time.Millisecond
	r.RestartBackoffMax = 100 * time.Millisecond

	upstreamAddr, _ := reconcilerSilentUpstream(t)
	upstreamHost, upstreamPort := splitHostPortInt(t, upstreamAddr)
	port := freeTCPPort(t)

	reached := make(chan *managedListener, 1)
	proceed := make(chan struct{})
	r.beforeRestartInstall = func(restarted *managedListener) {
		select {
		case reached <- restarted:
		default:
		}
		<-proceed
	}

	srv := audit.Server{
		ID: 1, Name: "s1", ListenPort: port,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: srv}}); !AllSucceeded(res) {
		t.Fatalf("initial add failed: %+v", res)
	}
	t.Cleanup(func() {
		r.Apply(context.Background(), []PlanItem{{Action: ActionRemove, ServerID: 1, Previous: srv}})
	})
	first := currentListener(r, 1)

	t.Log("Step 1: the listener dies; watch builds a replacement and is held before installing it")
	first.cancel()
	var replacement *managedListener
	select {
	case replacement = <-reached:
	case <-time.After(5 * time.Second):
		close(proceed)
		t.Fatal("watch never built a replacement")
	}

	t.Log("Step 2: a client connects meanwhile; the not-yet-installed replacement must not handle it")
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		close(proceed)
		t.Fatalf("the replacement should already hold the port: %v", err)
	}
	defer c.Close()
	time.Sleep(300 * time.Millisecond)
	if n := replacement.proxy.ActiveSessions(); n != 0 {
		close(proceed)
		t.Fatalf("the replacement handled %d connection(s) before it was installed and synchronised", n)
	}

	t.Log("Step 3: once released it is installed and serves")
	close(proceed)
	if !eventuallyTrue(5*time.Second, func() bool { return currentListener(r, 1) == replacement }) {
		t.Fatal("the replacement was never installed")
	}
}

// An in-place update must carry the TLS settings with it: a new host gets its
// own certificate check, a change of mode really takes effect, invalid
// settings are rejected without disturbing the running configuration, and a
// session that already captured the old configuration keeps it.
func TestReconciler_UpdateInPlaceAppliesTLSSettings(t *testing.T) {
	r := newTestReconciler()
	port := freeTCPPort(t)

	srv := audit.Server{
		ID: 1, Name: "s1", ListenPort: port,
		UpstreamHost: "db-a.example", UpstreamPort: 3306,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
		TLS: audit.TLSSkipVerify,
	}

	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: srv}}); !AllSucceeded(res) {
		t.Fatalf("initial add failed: %+v", res)
	}
	t.Cleanup(func() {
		r.Apply(context.Background(), []PlanItem{{Action: ActionRemove, ServerID: 1, Previous: srv}})
	})
	ml := currentListener(r, 1)

	// What an in-flight session would be holding.
	before := ml.proxy.upstream.Load()
	if before.TLS == nil || !before.TLS.InsecureSkipVerify {
		t.Fatalf("setup: expected the initial config to skip verification, got %+v", before.TLS)
	}

	t.Log("Step 1: switch to verify and move to a different host, in place")
	updated := srv
	updated.UpstreamHost = "db-b.example"
	updated.TLS = "verify"
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionUpdateInPlace, ServerID: 1, Desired: updated, Previous: srv}}); !AllSucceeded(res) {
		t.Fatalf("update-in-place failed: %+v", res)
	}
	after := ml.proxy.upstream.Load()
	if after.TLS == nil || after.TLS.InsecureSkipVerify {
		t.Fatal("after switching to verify, new connections still skip certificate verification")
	}
	if after.TLS.ServerName != "db-b.example" {
		t.Fatalf("TLS ServerName = %q, want the new host %q", after.TLS.ServerName, "db-b.example")
	}

	t.Log("Step 2: a session that captured the old configuration is unaffected")
	if !before.TLS.InsecureSkipVerify || before.TLS.ServerName == "db-b.example" {
		t.Fatalf("the earlier snapshot was modified by the update: %+v", before.TLS)
	}

	t.Log("Step 3: settings that cannot be applied are rejected and change nothing")
	bad := updated
	bad.TLSCAFile = "/nonexistent/ca.pem"
	res := r.Apply(context.Background(), []PlanItem{{Action: ActionUpdateInPlace, ServerID: 1, Desired: bad, Previous: updated}})
	if AllSucceeded(res) {
		t.Fatal("an update with an unreadable CA file was reported as applied")
	}
	if ml.proxy.upstream.Load() != after {
		t.Fatal("a rejected update changed the running upstream configuration")
	}
	if got := ml.getDesired(); got != updated {
		t.Fatalf("a rejected update changed the remembered desired config: %+v", got)
	}
}
