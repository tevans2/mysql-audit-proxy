package runtime

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"lustre/internal/audit"
)

// applyAdd is ActionAdd: bind the configured port and start accepting.
// Per the ticket, the caller only marks the revision applied once this
// (or, for ActionMovePort, the equivalent bind-first step) has actually
// succeeded — Apply reports that via this returning nil, and it's
// RecordApplied that then does the marking.
func (r *Reconciler) applyAdd(_ context.Context, item PlanItem) error {
	ml, err := r.newManagedProxy(item.Desired)
	if err != nil {
		return err
	}

	r.mu.Lock()
	displaced, hadDisplaced := r.listeners[item.ServerID]
	r.listeners[item.ServerID] = ml
	r.mu.Unlock()

	ml.start()
	go r.watch(item.ServerID, ml)

	r.logger().Printf("reconcile: server %d (%s) added, listening on %s", item.ServerID, item.Desired.Name, ml.proxy.ListenAddr)

	if hadDisplaced {
		// Plan never adds a server that is already managed, but if one ever
		// did, the listener it replaced must not be left running unowned.
		go r.retire(displaced, item.ServerID, item.Desired.Name)
	}
	return nil
}

// applyUpdateInPlace is ActionUpdateInPlace: the listen port is unchanged,
// so there is nothing to bind or drain — UpdateUpstreamTLS atomically swaps
// the already-running listener's upstream snapshot (address, credentials
// and TLS settings together), which only affects
// connections accepted after this call (see upstreamConfig's comment on
// proxy.go). Existing sessions keep whatever they captured when they
// connected, entirely untouched by this call.
func (r *Reconciler) applyUpdateInPlace(item PlanItem) error {
	if err := validateReconcilableServer(item.Desired); err != nil {
		return err
	}

	// The lookup and the update both happen under r.mu. watch installs a
	// restarted listener under the same lock, so an update can never be applied
	// to a listener that is in the middle of being replaced (and then lost):
	// it lands either before the install (watch carries it over) or on the
	// replacement itself.
	r.mu.Lock()
	defer r.mu.Unlock()

	ml, ok := r.listeners[item.ServerID]
	if !ok {
		return fmt.Errorf("server %d: update-in-place requested but no listener is currently managed for it", item.ServerID)
	}

	// The TLS settings are part of what an in-place update changes (a new
	// host needs a certificate check against that host; a switch from
	// skip-verify to verify, or a new CA file, must really take effect), and
	// they are built and validated here, before anything is swapped. If they
	// are invalid this fails the item without touching the listener, so the
	// last good configuration keeps serving new connections and nothing is
	// reported as applied that is not.
	d := item.Desired
	if err := applyUpstream(ml, d); err != nil {
		return fmt.Errorf("server %d (%s): rejected the new upstream settings, keeping the current ones: %w",
			item.ServerID, d.Name, err)
	}

	r.logger().Printf("reconcile: server %d (%s) upstream updated in place", item.ServerID, d.Name)
	return nil
}

// applyUpstream swaps ml's upstream address, credentials and TLS settings to
// those of d and records d as the listener's desired configuration, so a later
// restart brings it back with these settings rather than stale ones. Settings
// that cannot be built (a bad CA file, say) are rejected before anything
// changes. Callers hold r.mu.
func applyUpstream(ml *managedListener, d audit.Server) error {
	if err := ml.proxy.UpdateUpstreamTLS(
		net.JoinHostPort(d.UpstreamHost, strconv.Itoa(d.UpstreamPort)),
		d.Name,
		d.UpstreamUser,
		d.UpstreamPass,
		d.TLS,
		d.TLSCAFile,
	); err != nil {
		return err
	}
	ml.setDesired(d)
	return nil
}

// applyMovePort is ActionMovePort: bind the new port first, and only once
// that succeeds does the old listener start draining. If the new port
// can't bind, newManagedProxy's failure is returned as-is and r.listeners
// is never touched, so the old listener is left running exactly as before
// — this is what "preserve the old listener if the new port cannot be
// bound" means in practice: there is simply nothing here that could tear
// it down before the replacement is confirmed up.
//
// The old listener's retire (draining its sessions, potentially up to
// DrainTimeout) runs in its own goroutine rather than inline here — see the
// Apply doc comment for why: this call, and the Apply/poll cycle it's part
// of, must not block for as long as that drain takes.
func (r *Reconciler) applyMovePort(_ context.Context, item PlanItem) error {
	ml, err := r.newManagedProxy(item.Desired)
	if err != nil {
		return err
	}

	if r.beforeMoveInstall != nil {
		r.beforeMoveInstall()
	}

	// Which listener is being displaced is decided here, at the moment the new
	// one is installed, and under the same lock: the listener registered for
	// this server can change while the new port was being bound (watch may
	// have restarted it), and retiring one captured earlier would leave the
	// real one running with nobody managing it.
	r.mu.Lock()
	old, hadOld := r.listeners[item.ServerID]
	r.listeners[item.ServerID] = ml
	r.mu.Unlock()

	ml.start()
	go r.watch(item.ServerID, ml)

	r.logger().Printf("reconcile: server %d (%s) moved from port %d to %d, new listener up",
		item.ServerID, item.Desired.Name, item.Previous.ListenPort, item.Desired.ListenPort)

	if hadOld {
		// Free the old port now, not whenever the background drain gets going:
		// a later item in the same snapshot may be waiting for it.
		r.stopAccepting(old)
		go r.retire(old, item.ServerID, item.Desired.Name)
	}
	return nil
}

// applyRemove is ActionRemove: stop accepting new connections immediately
// (cancelling the listener's Run context), let existing sessions drain,
// and remove the listener from the Reconciler once draining is done —
// covering both "disabled" and "deleted" servers identically, matching
// Manager.Plan producing the same Action for both.
//
// The listener is removed from r.listeners (making it unmanaged) the
// instant this is called; the drain itself runs in its own goroutine — see
// the Apply doc comment for why Apply can't wait for it inline.
func (r *Reconciler) applyRemove(item PlanItem) error {
	r.mu.Lock()
	ml, ok := r.listeners[item.ServerID]
	if ok {
		delete(r.listeners, item.ServerID)
	}
	r.mu.Unlock()
	if !ok {
		// Already gone — Plan can propose ActionRemove for a server this
		// Reconciler never actually got to add (e.g. an earlier ActionAdd
		// for it previously failed), so this is not itself an error.
		return nil
	}

	// The port is released before this returns (only the drain is deferred), so
	// another server in the same snapshot can take it straight away.
	r.stopAccepting(ml)
	go r.retire(ml, item.ServerID, item.Previous.Name)
	r.logger().Printf("reconcile: server %d (%s) removed", item.ServerID, item.Previous.Name)
	return nil
}
