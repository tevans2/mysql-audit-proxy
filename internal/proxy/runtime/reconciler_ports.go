package runtime

import (
	"fmt"
	"sort"
	"time"

	"lustre/internal/audit"
	"lustre/internal/upstream"
)

// schedule decides the order in which items are carried out; results are
// still reported in the original order. Within one snapshot, one server's
// change can depend on another's having happened first, because ports are
// exclusive:
//
//   - removals go first, since they free ports;
//   - then port moves, ordered so that a move into a port currently held by
//     another server that is itself moving away waits for that one;
//   - then everything else (adds, in-place updates), which may want a port a
//     move or removal just freed.
//
// Moves that wait on each other in a loop (two servers swapping ports, say)
// cannot be ordered at all; they are returned in stopFirst (by item index) so
// Apply can release their old listeners before binding any of the new ones.
func (r *Reconciler) schedule(items []PlanItem) (order []int, stopFirst map[int]bool) {
	stopFirst = make(map[int]bool)

	var removes, moves, others []int
	for i, it := range items {
		switch it.Action {
		case ActionRemove:
			removes = append(removes, i)
		case ActionMovePort:
			moves = append(moves, i)
		default:
			others = append(others, i)
		}
	}

	// Who holds which port right now.
	holder := make(map[int]int)
	r.mu.Lock()
	for id, ml := range r.listeners {
		holder[ml.getDesired().ListenPort] = id
	}
	r.mu.Unlock()

	pendingMove := make(map[int]bool, len(moves))
	for _, i := range moves {
		pendingMove[items[i].ServerID] = true
	}

	order = append(order, removes...)

	remaining := moves
	for len(remaining) > 0 {
		var blocked []int
		for _, i := range remaining {
			it := items[i]
			if h, held := holder[it.Desired.ListenPort]; held && h != it.ServerID && pendingMove[h] {
				blocked = append(blocked, i)
				continue
			}
			order = append(order, i)
			delete(pendingMove, it.ServerID)
			delete(holder, it.Previous.ListenPort)
			holder[it.Desired.ListenPort] = it.ServerID
		}
		if len(blocked) == len(remaining) {
			// No move could go: what is left is one or more cycles.
			for _, i := range blocked {
				stopFirst[i] = true
				order = append(order, i)
			}
			break
		}
		remaining = blocked
	}

	return append(order, others...), stopFirst
}

// stopAccepting stops ml from accepting new connections and waits (bounded) for
// its Run to return, so that its port is actually free when this returns. It
// does not touch sessions already open: draining those is retire's job, and
// can take a long time, which is why the two are separate.
func (r *Reconciler) stopAccepting(ml *managedListener) {
	ml.retiring.Store(true)
	ml.cancel()
	select {
	case <-ml.done:
	case <-time.After(runStopTimeout):
	}
}

// precheckBind verifies, without binding or changing anything, the things that
// would make bringing s up fail for a reason other than its port: it must carry
// an upstream password, and its TLS settings (mode, CA file) must be buildable.
func (r *Reconciler) precheckBind(s audit.Server) error {
	if err := validateReconcilableServer(s); err != nil {
		return err
	}
	if _, err := upstream.TLSConfig(s.UpstreamHost, s.TLS, s.TLSCAFile); err != nil {
		return fmt.Errorf("server %q: %w", s.Name, err)
	}
	return nil
}

// applyPortCycle carries out, as one all-or-nothing group, the port moves that
// depend on each other and so cannot be done bind-first (two servers swapping
// ports). All of them succeed or none changes:
//
//  1. Every member is validated first (password, TLS settings). If any one
//     cannot be applied, nothing is touched and every member reports an error,
//     so the running listeners and the recorded configuration still agree.
//  2. Only then are the old listeners released (they stop accepting; their
//     sessions are left alone for now) and the new ones bound.
//  3. If every bind succeeds, the new listeners are installed and the old ones
//     drained in the background.
//  4. If any bind fails, the whole group is rolled back: the new listeners are
//     closed and each member's previous listener is brought back up, carrying
//     over the old one's open sessions so a later removal still drains them.
//     Every member reports an error, so none is recorded as moved.
//
// If a previous listener cannot be restored (its port was taken by something
// outside this process in the meantime), the server is left without a listener;
// it is no longer registered, and forgetMissingListeners makes the poller treat
// it as not applied so it is added again on the next poll.
func (r *Reconciler) applyPortCycle(items []PlanItem, stopFirst map[int]bool) map[int]error {
	idxs := make([]int, 0, len(stopFirst))
	for i := range stopFirst {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	errs := make(map[int]error, len(idxs))

	// 1. Validate every member before touching anything.
	invalid := false
	for _, i := range idxs {
		if err := r.precheckBind(items[i].Desired); err != nil {
			errs[i] = err
			invalid = true
		}
	}
	if invalid {
		for _, i := range idxs {
			if errs[i] == nil {
				errs[i] = fmt.Errorf("server %d (%s): port swap not performed because another server in the swap cannot be applied; nothing was changed",
					items[i].ServerID, items[i].Desired.Name)
			}
		}
		return errs
	}

	// 2. Release the old listeners, then bind the new ones.
	type member struct {
		idx      int
		old      *managedListener
		hadOld   bool
		previous audit.Server
		fresh    *managedListener
	}
	members := make([]*member, 0, len(idxs))
	r.mu.Lock()
	for _, i := range idxs {
		m := &member{idx: i, previous: items[i].Previous}
		m.old, m.hadOld = r.listeners[items[i].ServerID]
		if m.hadOld {
			delete(r.listeners, items[i].ServerID)
			m.previous = m.old.getDesired()
		}
		members = append(members, m)
	}
	r.mu.Unlock()
	for _, m := range members {
		if m.hadOld {
			r.stopAccepting(m.old)
			r.logger().Printf("reconcile: server %d (%s) released its port ahead of a port swap",
				items[m.idx].ServerID, items[m.idx].Previous.Name)
		}
	}

	for _, m := range members {
		fresh, err := r.newManagedProxy(items[m.idx].Desired)
		if err != nil {
			errs[m.idx] = err
			continue
		}
		m.fresh = fresh
	}

	// 3. Everything bound: install the new listeners, drain the old ones.
	if len(errs) == 0 {
		r.mu.Lock()
		for _, m := range members {
			r.listeners[items[m.idx].ServerID] = m.fresh
		}
		r.mu.Unlock()
		for _, m := range members {
			it := items[m.idx]
			m.fresh.start()
			go r.watch(it.ServerID, m.fresh)
			r.logger().Printf("reconcile: server %d (%s) moved from port %d to %d, new listener up",
				it.ServerID, it.Desired.Name, it.Previous.ListenPort, it.Desired.ListenPort)
			if m.hadOld {
				go r.retire(m.old, it.ServerID, it.Desired.Name)
			}
		}
		return errs
	}

	// 4. Roll the whole group back.
	for _, m := range members {
		if m.fresh != nil {
			r.stopAccepting(m.fresh)
			m.fresh = nil
		}
	}
	for _, m := range members {
		if !m.hadOld {
			continue
		}
		it := items[m.idx]
		restored, err := r.newManagedProxy(m.previous)
		if err != nil {
			r.logger().Printf("reconcile: server %d (%s) could not be restored on port %d after a failed port swap: %v",
				it.ServerID, it.Previous.Name, m.previous.ListenPort, err)
			go r.retire(m.old, it.ServerID, it.Previous.Name)
			continue
		}
		restored.prevs = m.old.liveLineage()
		r.mu.Lock()
		r.listeners[it.ServerID] = restored
		r.mu.Unlock()
		restored.start()
		go r.watch(it.ServerID, restored)
		r.logger().Printf("reconcile: server %d (%s) port swap rolled back; listening again on port %d",
			it.ServerID, it.Previous.Name, m.previous.ListenPort)
	}
	for _, m := range members {
		if errs[m.idx] == nil {
			errs[m.idx] = fmt.Errorf("server %d (%s): port swap rolled back because another server in the swap could not be applied",
				items[m.idx].ServerID, items[m.idx].Desired.Name)
		}
	}
	return errs
}
