package runtime

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
)

// defaultBindTimeout bounds how long applyAdd/applyMovePort wait for a new
// *Proxy's Run to report its listener as ready (or fail) before giving up.
// Run itself either sends on ready or returns an error essentially
// immediately once net.Listen has been attempted, so this only exists to
// keep a call to Apply from being able to hang forever if something
// unexpected happens (Run wedged before reaching either).
const defaultBindTimeout = 5 * time.Second

// validateReconcilableServer checks the one thing about a desired server
// that Manager.Plan's own validation (config.ValidateServers, shared with the
// PROXY_CONFIG_FILE path) deliberately does not: that it actually carries
// an upstream password.
//
// config.ValidateServers is shared with BuildProxies' static-file path, where a
// server's password legitimately arrives out of band via
// PROXY_UPSTREAM_PASSWORDS rather than the audit.Server record itself, so
// it cannot require UpstreamPass to be set. The live API path is different:
// config.ConfigSnapshot's servers come straight from GET /api/proxy/config, which
// is the endpoint that returns decrypted credentials, and an empty
// UpstreamPass reaching here means that particular record was sanitised or
// could not be decrypted — connecting anyway would only surface later as a
// per-connection "Access denied ... (using password: NO)" error that reads
// like a network fault. Checking it here, at the one place a desired
// server is actually about to be turned into a live *Proxy, means it fails
// as an ordinary per-item Apply error — one bad record does not block the
// rest of the same reconciliation, matching the ticket's "one server in a
// snapshot cannot be applied" failure-safety requirement.
func validateReconcilableServer(s audit.Server) error {
	if s.UpstreamPass == "" {
		return fmt.Errorf("server %q (id %d): no upstream password — the API record is missing one "+
			"or could not be decrypted with its config key", s.Name, s.ID)
	}
	return nil
}

// managedListener is everything the Reconciler needs to remember about one
// server ID's currently-running listener: the *Proxy actually serving it,
// and the cancel func that stops its Run — cancelling is the only way to
// stop a Proxy's accept loop (see the Run doc comment on proxy.go), so it
// has to be kept around from when Run was started.
type managedListener struct {
	proxy  *Proxy
	cancel context.CancelFunc

	// desired is the audit.Server this listener was created from, kept so
	// that if this listener's Run ever exits on its own — something it never
	// asked for — watch can try to bring the exact same listener back up
	// without waiting for a future poll to notice (see the watch doc comment
	// for why nothing else would ever do that). It is read by watch and
	// rewritten by applyUpdateInPlace from different goroutines, so it is
	// only ever touched through getDesired/setDesired, under desiredMu.
	desiredMu sync.Mutex
	desired   audit.Server

	// runErr receives Run's return value exactly once, whenever Run
	// actually stops for any reason — including a stop this Reconciler
	// itself requested via cancel. watch is its only reader.
	runErr <-chan error

	// done is closed once Run has returned, i.e. once this listener can
	// never accept another connection. retire waits on it between cancelling
	// the listener and draining its sessions — see retire for why.
	done <-chan struct{}

	// retiring is set by retire just before it calls cancel, so watch can
	// tell "we meant to stop this one" apart from "this one stopped on its
	// own" — only the latter is unexpected and worth restarting.
	retiring atomic.Bool

	// gate is closed by start to let the listener's Run begin accepting
	// connections. A listener is bound — and its port therefore held — as soon
	// as newManagedProxy returns, but serves nothing until start is called,
	// which is only done once it is installed and fully configured.
	gate      chan struct{}
	startOnce sync.Once

	// prevs are the earlier generations of this server's listener: when watch
	// replaces a listener whose Run exited, the sessions the old *Proxy still
	// has open do not go away, and they belong to this server just as much as
	// the replacement's do. Keeping the old *Proxy values here is what lets a
	// later retire drain every session this server ever had open, not just
	// those of the newest generation. Only generations that still have
	// sessions are kept. Set before the listener is installed and never
	// modified afterwards.
	prevs []*Proxy
}

// start lets the listener begin serving. It is safe to call more than once.
func (ml *managedListener) start() {
	ml.startOnce.Do(func() { close(ml.gate) })
}

// lineage returns every *Proxy whose sessions this server's listener is
// responsible for: the current one and the earlier generations that still
// have sessions open.
func (ml *managedListener) lineage() []*Proxy {
	out := make([]*Proxy, 0, len(ml.prevs)+1)
	out = append(out, ml.prevs...)
	return append(out, ml.proxy)
}

func (ml *managedListener) getDesired() audit.Server {
	ml.desiredMu.Lock()
	defer ml.desiredMu.Unlock()
	return ml.desired
}

func (ml *managedListener) setDesired(s audit.Server) {
	ml.desiredMu.Lock()
	defer ml.desiredMu.Unlock()
	ml.desired = s
}

// runStopTimeout bounds how long retire waits for a cancelled listener's Run
// to actually return before draining anyway. Run returns essentially as soon
// as its listener is closed, so this only exists to keep a retire from being
// able to hang forever if something unexpected wedges it.
const runStopTimeout = 5 * time.Second

// newManagedProxy builds a *Proxy for s and starts it via Run, waiting up
// to r.bindTimeout() for its listener to come up (or for Run to fail
// outright). On success it returns the *managedListener with its port bound
// but NOT yet serving: the caller must install it and then call start. That
// split is what lets a replacement be brought fully up to date before it
// handles a single connection. On
// failure it returns a nil *managedListener and an error, and nothing is
// left running — the caller (applyAdd) never adds a failed bind to
// r.listeners, which is what naturally satisfies "preserve the old
// listener if the new port cannot be bound" for applyMovePort: the old
// entry in r.listeners is simply never touched until this succeeds.
func (r *Reconciler) newManagedProxy(s audit.Server) (*managedListener, error) {
	if err := validateReconcilableServer(s); err != nil {
		return nil, err
	}
	if r.injectBindError != nil {
		if err := r.injectBindError(s); err != nil {
			return nil, err
		}
	}

	proxy := &Proxy{
		TLS:                    s.TLS,
		TLSCAFile:              s.TLSCAFile,
		ListenAddr:             net.JoinHostPort("0.0.0.0", strconv.Itoa(s.ListenPort)),
		UpstreamAddr:           net.JoinHostPort(s.UpstreamHost, strconv.Itoa(s.UpstreamPort)),
		UpstreamName:           s.Name,
		UpstreamUser:           s.UpstreamUser,
		UpstreamPass:           s.UpstreamPass,
		UpstreamReadTimeout:    r.UpstreamReadTimeout,
		UpstreamCommandTimeout: r.UpstreamCommandTimeout,
		Sink:                   r.Sink,
		Logger:                 r.logger(),
	}

	gate := make(chan struct{})
	proxy.acceptGate = gate

	runCtx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Listener, 1)
	runErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		runErr <- proxy.Run(runCtx, ready)
		close(done)
	}()

	select {
	case <-ready:
		return &managedListener{proxy: proxy, cancel: cancel, desired: s, runErr: runErr, done: done, gate: gate}, nil
	case err := <-runErr:
		cancel()
		if err == nil {
			err = fmt.Errorf("proxy for server %q exited before its listener became ready", s.Name)
		}
		return nil, fmt.Errorf("bind %s: %w", proxy.ListenAddr, err)
	case <-time.After(r.bindTimeout()):
		cancel()
		return nil, fmt.Errorf("bind %s: timed out after %s waiting for the listener", proxy.ListenAddr, r.bindTimeout())
	}
}

// liveLineage returns the generations of ml's listener that still have
// sessions open, for handing on to a listener that takes its place.
func (ml *managedListener) liveLineage() []*Proxy {
	var out []*Proxy
	for _, p := range ml.lineage() {
		if p.ActiveSessions() > 0 {
			out = append(out, p)
		}
	}
	return out
}

// retire stops ml from accepting new connections and drains its existing
// sessions, logging completion or timeout of the drain per the ticket's
// observability requirements. It never returns an error: once a listener
// has been removed from r.listeners (by applyRemove) or superseded (by
// applyMovePort), there is nothing left for a caller to do differently
// based on how the drain went — Drain itself already falls back to forcibly
// closing whatever is left after DrainTimeout.
//
// retire is always run in its own goroutine by its callers (applyRemove,
// applyMovePort) rather than inline, so a drain — which can legitimately
// take up to DrainTimeout, plus a further forceCloseGracePeriod if it has
// to force-close stragglers — never blocks Apply, and through it, the
// config-polling loop that's waiting to pick up the next revision.
//
// It cancels the listener, then waits for its Run to actually return
// (ml.done) before it looks at any session count or drains anything. That
// ordering is what makes the drain reliable. cancel() only *asks* Run to
// stop: closing a listener does not retroactively un-accept a connection
// Accept() had already handed back to Run's accept loop a moment earlier,
// and that connection's session may not be counted yet — so reading
// ActiveSessions() (or starting to wait on the session WaitGroup) right
// after cancel() can miss it entirely, and a connection accepted at that
// instant could then never be waited for or force-closed. Once Run has
// returned, nothing can accept any more, so every session that will ever
// exist for this listener has already been counted and Drain sees all of
// them. runStopTimeout only keeps a wedged Run from hanging this goroutine
// forever; if it ever fires, draining proceeds anyway.
//
// It still always calls Drain, even when the count then reads 0: it is
// cheap when there is nothing to wait for.
func (r *Reconciler) retire(ml *managedListener, serverID int, name string) {
	ml.retiring.Store(true)
	ml.cancel()

	select {
	case <-ml.done:
	case <-time.After(runStopTimeout):
		r.logger().Printf("reconcile: server %d (%s) listener did not stop within %s of being cancelled; draining anyway",
			serverID, name, runStopTimeout)
	}

	// Every generation of this server's listener: sessions opened before a
	// restart belong to the older *Proxy, and must be drained like the rest.
	owners := ml.lineage()
	countSessions := func() int64 {
		var n int64
		for _, p := range owners {
			n += p.ActiveSessions()
		}
		return n
	}

	before := countSessions()
	r.logger().Printf("reconcile: server %d (%s) retiring, draining up to %d active session(s) seen so far (timeout %s)",
		serverID, name, before, r.drainTimeout())

	var wg sync.WaitGroup
	for _, p := range owners {
		wg.Add(1)
		go func(p *Proxy) {
			defer wg.Done()
			p.Drain(r.drainTimeout())
		}(p)
	}
	wg.Wait()

	if after := countSessions(); after > 0 {
		r.logger().Printf("reconcile: server %d (%s) drain timed out with %d session(s) forcibly closed", serverID, name, after)
	} else {
		r.logger().Printf("reconcile: server %d (%s) drain completed, all sessions ended", serverID, name)
	}
}
