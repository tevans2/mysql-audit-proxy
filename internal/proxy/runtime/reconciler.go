package runtime

// Reconciler is the "turn a Plan into real listener changes" half of the
// live-config-reconciliation work. Manager (manager.go)
// only ever computes what needs to change, as pure PlanItems, against no
// real listener at all — Reconciler is what actually binds, updates,
// drains, and removes *Proxy instances to carry one out, on top of the
// cancellable, session-tracking Proxy.Run built in chunk 1
// (session_test.go) and the atomic per-session upstream snapshot
// added alongside it in this chunk (see upstreamConfig on Proxy).
//
// Reconciler deliberately does not touch Manager's own state directly —
// every PlanItem it succeeds on is reported back via ApplyResult, and it is
// the caller's job (the startup/polling loop landing in a later chunk) to
// call Manager.RecordApplied/RecordRevision based on those results. That
// split is what keeps Manager pure and testable without a real listener,
// and keeps Reconciler testable without a network-polling loop.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
)

// ApplyResult reports what happened when the Reconciler carried out one
// PlanItem. Err is nil on success. A caller uses this — not any side effect
// on Manager — to decide whether to call Manager.RecordApplied for this
// item, and AllSucceeded (below) to decide whether it's safe to call
// Manager.RecordRevision for the whole snapshot.
type ApplyResult struct {
	Item PlanItem
	Err  error
}

// AllSucceeded reports whether every result in results succeeded — the
// direct check for the ticket's "Only update the stored applied revision
// after successful reconciliation" requirement.
func AllSucceeded(results []ApplyResult) bool {
	for _, r := range results {
		if r.Err != nil {
			return false
		}
	}
	return true
}

// Reconciler owns one real *Proxy per currently-listening server ID and
// carries out Manager-produced PlanItems against them.
type Reconciler struct {
	// Deps used to build a new *Proxy for ActionAdd/ActionMovePort — the
	// same ingredients BuildProxies uses for the static-config startup
	// path, since a reconciler-managed listener is otherwise identical.
	Sink                   audit.Sink
	Logger                 *log.Logger
	UpstreamReadTimeout    time.Duration
	UpstreamCommandTimeout time.Duration

	// DrainTimeout bounds how long applyRemove/applyMovePort wait for a
	// retired listener's sessions to finish on their own before forcing
	// them closed — see Proxy.Drain.
	DrainTimeout time.Duration

	// BindTimeout overrides defaultBindTimeout when non-zero. Exists mainly
	// so tests can keep a failed-bind case fast rather than waiting out the
	// full default.
	BindTimeout time.Duration

	// RestartBackoff is how long watch waits before its first attempt to
	// restart a listener that exited on its own, doubling after each failed
	// attempt up to RestartBackoffMax. Zero means defaultRestartBackoff and
	// defaultRestartBackoffMax. Exists mainly so tests can keep this fast.
	RestartBackoff    time.Duration
	RestartBackoffMax time.Duration

	// beforeRestartInstall, when set, is called by watch after a replacement
	// listener has been built (bound, but not yet serving) and before it is
	// installed. beforeMoveInstall is the equivalent for applyMovePort. They
	// exist only so tests can hold an operation at exactly that point while
	// something else happens.
	beforeRestartInstall func(restarted *managedListener)
	beforeMoveInstall    func()

	// injectBindError, when set, is consulted by newManagedProxy before it
	// binds anything and can make it fail for a given server. Tests only: it
	// lets a failure be forced at exactly one step of a multi-listener
	// operation.
	injectBindError func(audit.Server) error

	mu        sync.Mutex
	listeners map[int]*managedListener // server ID -> currently-running listener
}

// NewReconciler returns a Reconciler with no listeners running yet. The
// zero value of every field except the required Sink is usable, but Logger
// defaults to log.Default() if left nil (see the internal logger method)
// and DrainTimeout/BindTimeout fall back to sensible defaults the same way.
func NewReconciler(sink audit.Sink) *Reconciler {
	return &Reconciler{
		Sink:      sink,
		listeners: make(map[int]*managedListener),
	}
}

func (r *Reconciler) logger() *log.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return log.Default()
}

func (r *Reconciler) drainTimeout() time.Duration {
	if r.DrainTimeout > 0 {
		return r.DrainTimeout
	}
	return 30 * time.Second
}

func (r *Reconciler) bindTimeout() time.Duration {
	if r.BindTimeout > 0 {
		return r.BindTimeout
	}
	return defaultBindTimeout
}

// Apply carries out every item in items independently: one item failing
// (a port that won't bind, say) never stops the rest from being attempted,
// matching the ticket's "Do not tear down working listeners merely because
// a refresh failed" and "One server in a snapshot cannot be applied" failure
// -safety requirements. The results are returned in the same order as
// items; the caller (not Reconciler) decides what to do with them — see the
// package comment.
//
// Apply itself never blocks on a drain: applyRemove and applyMovePort each
// hand their retired listener's drain off to its own goroutine (see retire
// in reconciler_listener.go and its callers in reconciler_apply.go) before returning, specifically so that one
// removed or moved server's drain (up to DrainTimeout, plus a further grace
// period if it has to force-close) can never hold up Apply — and, through
// it, the polling loop that calls Apply — for as long as that drain takes.
func (r *Reconciler) Apply(ctx context.Context, items []PlanItem) []ApplyResult {
	results := make([]ApplyResult, len(items))
	order, stopFirst := r.schedule(items)

	// Servers exchanging ports with each other cannot be moved one at a time
	// (each one's new port is still held by the next one's old listener), so
	// they are handled together as one group the first time one of them comes
	// up in the order: see applyPortCycle.
	var cycleErrs map[int]error
	cycleDone := false

	for _, i := range order {
		item := items[i]
		var err error
		switch item.Action {
		case ActionAdd:
			err = r.applyAdd(ctx, item)
		case ActionUpdateInPlace:
			err = r.applyUpdateInPlace(item)
		case ActionMovePort:
			if stopFirst[i] {
				if !cycleDone {
					cycleErrs = r.applyPortCycle(items, stopFirst)
					cycleDone = true
				}
				err = cycleErrs[i]
			} else {
				err = r.applyMovePort(ctx, item)
			}
		case ActionRemove:
			err = r.applyRemove(item)
		default:
			err = fmt.Errorf("server %d: unknown action %v", item.ServerID, item.Action)
		}
		results[i] = ApplyResult{Item: item, Err: err}
		if err != nil {
			name := item.Desired.Name
			if name == "" {
				name = item.Previous.Name
			}
			r.logger().Printf("reconcile: server %d (%s) %s failed: %v", item.ServerID, name, item.Action, err)
		}
	}
	return results
}

// IsManaged reports whether the Reconciler currently has a listener registered
// for serverID.
func (r *Reconciler) IsManaged(serverID int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.listeners[serverID]
	return ok
}

// forgetMissingListeners removes from m every server it records as applied but
// that r has no listener for, and returns their IDs. Without this, a server that
// lost its listener (a failed swap that could not restore it) would be planned
// as "unchanged" for as long as its configuration stays the same, and never
// brought back.
func forgetMissingListeners(m *Manager, r *Reconciler) []int {
	var gone []int
	for id, s := range m.AppliedServers() {
		if !r.IsManaged(id) {
			m.RecordApplied(PlanItem{Action: ActionRemove, ServerID: id, Previous: s})
			gone = append(gone, id)
		}
	}
	sort.Ints(gone)
	return gone
}

// ManagedServerIDs returns the server IDs this Reconciler currently has a
// listener running for — used by tests, and by a later chunk's
// observability logging.
func (r *Reconciler) ManagedServerIDs() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]int, 0, len(r.listeners))
	for id := range r.listeners {
		ids = append(ids, id)
	}
	return ids
}
