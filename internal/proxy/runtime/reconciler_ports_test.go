package runtime

// Tests for ports that change hands inside one snapshot: a port freed by a
// removal or a move and taken by another server in the same Apply, and servers
// exchanging ports.

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"lustre/internal/audit"
	"lustre/internal/proxy/config"
)

func portServer(id int, name string, listenPort int, upstreamHost string, upstreamPort int) audit.Server {
	return audit.Server{
		ID: id, Name: name, ListenPort: listenPort,
		UpstreamHost: upstreamHost, UpstreamPort: upstreamPort,
		UpstreamUser: "root", UpstreamPass: "pw", Enabled: true,
	}
}

// listensOn reports the port the Reconciler's listener for serverID is bound to.
func listensOn(t *testing.T, r *Reconciler, serverID int) int {
	t.Helper()
	ml := currentListener(r, serverID)
	if ml == nil {
		t.Fatalf("no managed listener for server %d", serverID)
	}
	_, portStr, err := net.SplitHostPort(ml.proxy.ListenAddr)
	if err != nil {
		t.Fatalf("listen address %q: %v", ml.proxy.ListenAddr, err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("listen port %q: %v", portStr, err)
	}
	return p
}

func distinctPorts(t *testing.T, n int) []int {
	t.Helper()
	seen := map[int]bool{}
	var out []int
	for len(out) < n {
		p := freeTCPPort(t)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func cleanupAll(t *testing.T, r *Reconciler, servers ...audit.Server) {
	t.Cleanup(func() {
		var items []PlanItem
		for _, s := range servers {
			items = append(items, PlanItem{Action: ActionRemove, ServerID: s.ID, Previous: s})
		}
		r.Apply(context.Background(), items)
	})
}

// A server removed and another added on the same port in one snapshot: the
// add must succeed even when the plan lists it first.
func TestReconciler_RemoveThenAddSamePortInOneApply(t *testing.T) {
	r := newTestReconciler()
	upstreamAddr, _ := reconcilerSilentUpstream(t)
	host, upPort := splitHostPortInt(t, upstreamAddr)
	port := freeTCPPort(t)

	one := portServer(1, "s1", port, host, upPort)
	two := portServer(2, "s2", port, host, upPort)

	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 1, Desired: one}}); !AllSucceeded(res) {
		t.Fatalf("initial add failed: %+v", res)
	}
	cleanupAll(t, r, one, two)

	// Add listed before the remove on purpose.
	res := r.Apply(context.Background(), []PlanItem{
		{Action: ActionAdd, ServerID: 2, Desired: two},
		{Action: ActionRemove, ServerID: 1, Previous: one},
	})
	if !AllSucceeded(res) {
		t.Fatalf("remove + add on the same port failed: %+v", res)
	}
	if res[0].Item.ServerID != 2 || res[1].Item.ServerID != 1 {
		t.Fatalf("results are not in the original item order: %+v", res)
	}
	if ids := r.ManagedServerIDs(); len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("ManagedServerIDs = %v, want [2]", ids)
	}
	if !canDial(port) {
		t.Fatalf("port %d is not accepting connections after the hand-over", port)
	}
}

// B moves into the port A is leaving, and A moves to a free port. Listed with B
// first; it must still work, one move after the other and without any outage.
func TestReconciler_MoveIntoPortAnotherServerIsLeaving(t *testing.T) {
	r := newTestReconciler()
	upstreamAddr, _ := reconcilerSilentUpstream(t)
	host, upPort := splitHostPortInt(t, upstreamAddr)
	ports := distinctPorts(t, 3)
	p1, p2, p3 := ports[0], ports[1], ports[2]

	a := portServer(1, "a", p1, host, upPort)
	b := portServer(2, "b", p2, host, upPort)
	if res := r.Apply(context.Background(), []PlanItem{
		{Action: ActionAdd, ServerID: 1, Desired: a},
		{Action: ActionAdd, ServerID: 2, Desired: b},
	}); !AllSucceeded(res) {
		t.Fatalf("initial adds failed: %+v", res)
	}

	a2 := a
	a2.ListenPort = p3
	b2 := b
	b2.ListenPort = p1
	cleanupAll(t, r, a2, b2)

	res := r.Apply(context.Background(), []PlanItem{
		{Action: ActionMovePort, ServerID: 2, Desired: b2, Previous: b},
		{Action: ActionMovePort, ServerID: 1, Desired: a2, Previous: a},
	})
	if !AllSucceeded(res) {
		t.Fatalf("chained moves failed: %+v", res)
	}
	if got := listensOn(t, r, 1); got != p3 {
		t.Fatalf("server 1 listens on %d, want %d", got, p3)
	}
	if got := listensOn(t, r, 2); got != p1 {
		t.Fatalf("server 2 listens on %d, want %d", got, p1)
	}
	if !canDial(p1) || !canDial(p3) {
		t.Fatal("a moved server is not accepting connections on its new port")
	}
	if !eventuallyTrue(3*time.Second, func() bool { return !canDial(p2) }) {
		t.Fatalf("port %d still accepts connections although nobody is configured on it", p2)
	}
}

// Two servers exchanging ports cannot be moved one at a time; the swap must
// still complete.
func TestReconciler_SwapPortsBetweenTwoServers(t *testing.T) {
	r := newTestReconciler()
	upstreamAddr, _ := reconcilerSilentUpstream(t)
	host, upPort := splitHostPortInt(t, upstreamAddr)
	ports := distinctPorts(t, 2)
	p1, p2 := ports[0], ports[1]

	a := portServer(1, "a", p1, host, upPort)
	b := portServer(2, "b", p2, host, upPort)
	if res := r.Apply(context.Background(), []PlanItem{
		{Action: ActionAdd, ServerID: 1, Desired: a},
		{Action: ActionAdd, ServerID: 2, Desired: b},
	}); !AllSucceeded(res) {
		t.Fatalf("initial adds failed: %+v", res)
	}

	a2 := a
	a2.ListenPort = p2
	b2 := b
	b2.ListenPort = p1
	cleanupAll(t, r, a2, b2)

	res := r.Apply(context.Background(), []PlanItem{
		{Action: ActionMovePort, ServerID: 1, Desired: a2, Previous: a},
		{Action: ActionMovePort, ServerID: 2, Desired: b2, Previous: b},
	})
	if !AllSucceeded(res) {
		t.Fatalf("port swap failed: %+v", res)
	}
	if got := listensOn(t, r, 1); got != p2 {
		t.Fatalf("server 1 listens on %d, want %d", got, p2)
	}
	if got := listensOn(t, r, 2); got != p1 {
		t.Fatalf("server 2 listens on %d, want %d", got, p1)
	}
	if !canDial(p1) || !canDial(p2) {
		t.Fatal("a swapped server is not accepting connections")
	}
}

// A swap in which one member cannot be applied at all (here an unreadable CA
// file) must be refused before anything is touched: both listeners keep
// running on their original ports, and both members report an error, so the
// recorded configuration still matches what is running.
func TestReconciler_SwapWithUnbuildableMemberChangesNothing(t *testing.T) {
	r := newTestReconciler()
	upstreamAddr, _ := reconcilerSilentUpstream(t)
	host, upPort := splitHostPortInt(t, upstreamAddr)
	ports := distinctPorts(t, 2)
	p1, p2 := ports[0], ports[1]

	a := portServer(1, "a", p1, host, upPort)
	b := portServer(2, "b", p2, host, upPort)
	if res := r.Apply(context.Background(), []PlanItem{
		{Action: ActionAdd, ServerID: 1, Desired: a},
		{Action: ActionAdd, ServerID: 2, Desired: b},
	}); !AllSucceeded(res) {
		t.Fatalf("initial adds failed: %+v", res)
	}
	cleanupAll(t, r, a, b)
	origA, origB := currentListener(r, 1), currentListener(r, 2)

	a2 := a
	a2.ListenPort = p2
	a2.TLS = "verify"
	a2.TLSCAFile = "/nonexistent/ca.pem"
	b2 := b
	b2.ListenPort = p1

	res := r.Apply(context.Background(), []PlanItem{
		{Action: ActionMovePort, ServerID: 1, Desired: a2, Previous: a},
		{Action: ActionMovePort, ServerID: 2, Desired: b2, Previous: b},
	})
	if res[0].Err == nil || res[1].Err == nil {
		t.Fatalf("a swap with an unbuildable member must fail for every member: %+v", res)
	}
	if currentListener(r, 1) != origA || currentListener(r, 2) != origB {
		t.Fatal("the swap was refused but the running listeners were still replaced")
	}
	if !canDial(p1) || !canDial(p2) {
		t.Fatal("a listener stopped serving although the swap was refused")
	}
}

// A swap that fails after the old listeners were released must be rolled back
// for every member: both servers end up listening on their original ports
// again, both report an error, and a session that was open on the old listener
// is still owned by the restored one.
func TestReconciler_FailedSwapRollsBackEveryMember(t *testing.T) {
	r := newTestReconciler()
	upstreamAddr, _ := reconcilerSilentUpstream(t)
	host, upPort := splitHostPortInt(t, upstreamAddr)
	ports := distinctPorts(t, 2)
	p1, p2 := ports[0], ports[1]

	a := portServer(1, "a", p1, host, upPort)
	b := portServer(2, "b", p2, host, upPort)
	if res := r.Apply(context.Background(), []PlanItem{
		{Action: ActionAdd, ServerID: 1, Desired: a},
		{Action: ActionAdd, ServerID: 2, Desired: b},
	}); !AllSucceeded(res) {
		t.Fatalf("initial adds failed: %+v", res)
	}
	cleanupAll(t, r, a, b)
	origA := currentListener(r, 1)

	// A session open on server a's listener.
	c1, c2 := net.Pipe()
	defer c2.Close()
	origA.proxy.sessionWG.Add(1)
	origA.proxy.activeSessions.Add(1)
	origA.proxy.trackConn(c1)
	defer func() {
		origA.proxy.untrackConn(c1)
		origA.proxy.activeSessions.Add(-1)
		origA.proxy.sessionWG.Done()
	}()

	a2 := a
	a2.ListenPort = p2
	b2 := b
	b2.ListenPort = p1

	// Binding b's new port fails; everything else works, including restoring
	// the previous listeners.
	r.injectBindError = func(s audit.Server) error {
		if s.Name == "b" && s.ListenPort == p1 {
			return net.UnknownNetworkError("injected bind failure")
		}
		return nil
	}

	res := r.Apply(context.Background(), []PlanItem{
		{Action: ActionMovePort, ServerID: 1, Desired: a2, Previous: a},
		{Action: ActionMovePort, ServerID: 2, Desired: b2, Previous: b},
	})
	if res[0].Err == nil || res[1].Err == nil {
		t.Fatalf("a rolled-back swap must report an error for every member: %+v", res)
	}
	if got := listensOn(t, r, 1); got != p1 {
		t.Fatalf("server 1 listens on %d after the rollback, want its original %d", got, p1)
	}
	if got := listensOn(t, r, 2); got != p2 {
		t.Fatalf("server 2 listens on %d after the rollback, want its original %d", got, p2)
	}
	if !canDial(p1) || !canDial(p2) {
		t.Fatal("a server is not serving on its original port after the rollback")
	}
	restoredA := currentListener(r, 1)
	if len(restoredA.prevs) != 1 || restoredA.prevs[0] != origA.proxy {
		t.Fatalf("the restored listener does not own the session opened before the swap (prevs = %v)", restoredA.prevs)
	}
}

// A server that is recorded as applied but has lost its listener must be
// forgotten, so the next plan adds it again instead of treating it as unchanged.
func TestForgetMissingListeners_ServerWithoutListenerIsPlannedAgain(t *testing.T) {
	r := newTestReconciler()
	upstreamAddr, _ := reconcilerSilentUpstream(t)
	host, upPort := splitHostPortInt(t, upstreamAddr)
	ports := distinctPorts(t, 2)

	a := portServer(1, "a", ports[0], host, upPort)
	b := portServer(2, "b", ports[1], host, upPort)

	m := NewManager()
	m.RecordApplied(PlanItem{Action: ActionAdd, ServerID: 1, Desired: a})
	m.RecordApplied(PlanItem{Action: ActionAdd, ServerID: 2, Desired: b})

	// Only b really has a listener.
	if res := r.Apply(context.Background(), []PlanItem{{Action: ActionAdd, ServerID: 2, Desired: b}}); !AllSucceeded(res) {
		t.Fatalf("add failed: %+v", res)
	}
	cleanupAll(t, r, a, b)

	gone := forgetMissingListeners(m, r)
	if len(gone) != 1 || gone[0] != 1 {
		t.Fatalf("forgotten = %v, want [1]", gone)
	}
	applied := m.AppliedServers()
	if _, ok := applied[1]; ok {
		t.Fatal("server 1 is still recorded as applied")
	}
	if _, ok := applied[2]; !ok {
		t.Fatal("server 2, which has a listener, was forgotten")
	}

	items, err := m.Plan(&config.ConfigSnapshot{Revision: 5, Servers: []audit.Server{a, b}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(items) != 1 || items[0].ServerID != 1 || items[0].Action != ActionAdd {
		t.Fatalf("plan = %+v, want exactly one add for server 1", items)
	}
}
