package runtime

import (
	"fmt"
	"time"
)

const (
	defaultRestartBackoff    = 1 * time.Second
	defaultRestartBackoffMax = 30 * time.Second
)

func (r *Reconciler) restartBackoff() time.Duration {
	if r.RestartBackoff > 0 {
		return r.RestartBackoff
	}
	return defaultRestartBackoff
}

func (r *Reconciler) restartBackoffMax() time.Duration {
	if r.RestartBackoffMax > 0 {
		return r.RestartBackoffMax
	}
	return defaultRestartBackoffMax
}

// owns reports whether ml is still the listener registered for serverID —
// i.e. nothing has removed it or replaced it (a move-port, an add, a restart)
// since it was installed.
func (r *Reconciler) owns(serverID int, ml *managedListener) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.listeners[serverID]
	return ok && cur == ml
}

// watch waits for ml's Run to actually stop, then decides whether that was
// expected or not. retire (in reconciler_listener.go) is responsible for
// stopping a managed listener, and it marks ml.retiring before it does — so
// if Run stops and that flag isn't set, Run exited entirely on its own.
//
// Nothing else would ever notice that: Manager's idea of what's "applied"
// only changes in response to an ApplyResult this Reconciler itself reports,
// so a listener quietly dying leaves Manager still believing it is up, and
// even a newer revision with unchanged details for that server would plan
// nothing. watch is what notices instead, and keeps trying to bring the same
// listener back up — with the configuration it last had, including any
// in-place update since — until it succeeds or the listener is no longer
// wanted. It retries with a capped exponential backoff rather than giving up
// after one attempt, so a port that is briefly unavailable (the usual reason
// a restart fails) comes back on its own once it frees up.
//
// Until a replacement is installed, ml stays registered in r.listeners:
// the server is still "managed", a later remove or move-port still finds it
// (and still drains whatever sessions it had), and a later add for the same
// ID simply replaces it. Every pass re-checks ownership, and a replacement is
// only installed — under r.mu, in the same step as that check — if ml is
// still the registered listener. If a remove, move-port or add got there
// first, the freshly built replacement is discarded rather than installed, so
// a removal can never be undone by a restart that was already under way.
func (r *Reconciler) watch(serverID int, ml *managedListener) {
	err := <-ml.runErr
	if ml.retiring.Load() {
		// Expected: retire already cancelled this listener on purpose and
		// is handling (or has handled) its own logging and draining.
		return
	}
	// Run has returned, so this generation can never accept another
	// connection and its set of sessions is final.
	<-ml.done

	name := ml.getDesired().Name
	r.logger().Printf("reconcile: server %d (%s) listener exited unexpectedly (%v); will keep trying to restart it",
		serverID, name, err)

	delay := r.restartBackoff()
	for {
		time.Sleep(delay)

		if ml.retiring.Load() || !r.owns(serverID, ml) {
			// Removed or superseded while we were waiting.
			return
		}

		desired := ml.getDesired()
		restarted, restartErr := r.newManagedProxy(desired)
		if restartErr == nil {
			if r.beforeRestartInstall != nil {
				r.beforeRestartInstall(restarted)
			}

			// Ownership check, "has the configuration changed since I built
			// this?" check and install all happen under r.mu. applyUpdateInPlace
			// also does its whole update under r.mu, so the two are strictly
			// ordered: either the update ran before this point (and is picked up
			// below) or it runs after the install (and then finds and updates the
			// replacement). It can never land on the old listener in between and
			// be lost. The replacement is not serving yet (see newManagedProxy),
			// so nothing is ever handled with out-of-date settings.
			r.mu.Lock()
			cur, ok := r.listeners[serverID]
			if !ok || cur != ml {
				r.mu.Unlock()
				// Superseded or removed while the replacement was starting up:
				// it must not be installed. Shut it down through retire.
				r.logger().Printf("reconcile: server %d (%s) restart discarded: the listener was removed or replaced while it was starting",
					serverID, desired.Name)
				go r.retire(restarted, serverID, desired.Name)
				return
			}

			var syncErr error
			if latest := ml.getDesired(); latest != desired {
				// An in-place update was acknowledged while the replacement was
				// being built; it was built from the older configuration.
				syncErr = applyUpstream(restarted, latest)
			}
			if syncErr == nil {
				// The sessions the dead generation still has open stay this
				// server's responsibility: carry them forward so a later
				// remove or move drains them too. Generations without sessions
				// are dropped; a dead one cannot gain any.
				for _, p := range ml.lineage() {
					if p.ActiveSessions() > 0 {
						restarted.prevs = append(restarted.prevs, p)
					}
				}
				r.listeners[serverID] = restarted
				r.mu.Unlock()
				restarted.start()
				r.logger().Printf("reconcile: server %d (%s) restarted, listening on %s",
					serverID, restarted.getDesired().Name, restarted.proxy.ListenAddr)
				go r.watch(serverID, restarted)
				return
			}
			r.mu.Unlock()

			// The replacement could not take the newer settings, so it must not
			// serve with stale ones. Stop it (waiting briefly so its port is
			// free again) and go round once more using the latest settings.
			restartErr = fmt.Errorf("replacement could not take the settings updated while it started: %w", syncErr)
			restarted.retiring.Store(true)
			restarted.cancel()
			select {
			case <-restarted.done:
			case <-time.After(runStopTimeout):
			}
			go r.retire(restarted, serverID, desired.Name)
		}

		r.logger().Printf("reconcile: server %d (%s) restart after unexpected exit failed (retrying in %s): %v",
			serverID, desired.Name, minDuration(delay*2, r.restartBackoffMax()), restartErr)
		delay = minDuration(delay*2, r.restartBackoffMax())
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
