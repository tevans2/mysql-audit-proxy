package runtime

import (
	"context"
	"log"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/proxy/config"
)

// heartbeatInterval is how often an idle proxy re-reports the revision it is
// running, so the back-office can tell it is still alive rather than stopped.
// The viewer treats a report older than twice this as a proxy that may have
// stopped, so the two belong together: changing one without the other makes
// the viewer lie.
const heartbeatInterval = 30 * time.Second

// configPoller turns config.ConfigClient.Poll's poll outcomes into Manager.Plan +
// Reconciler.Apply cycles, and is what makes "will be retried on the next
// poll" actually true. It remembers the most recent snapshot that failed to
// fully apply — because Manager.Plan rejected it, or because one or more
// items in Reconciler.Apply's results came back with an error — and
// retries exactly that snapshot on every later poll, including one that
// reports no change at all (304 Not Modified), until it either fully
// succeeds or a newer snapshot from the API supersedes it.
//
// Without this, a transient failure (a port briefly in use, say) would
// leave that server down until a human edited the config again to produce
// a genuinely new revision for the API to serve — the next poll of an
// unchanged revision legitimately gets a 304, and there would be nothing
// here to retry against.
type configPoller struct {
	manager    *Manager
	reconciler *Reconciler
	client     *config.ConfigClient

	// pending is the last snapshot seen that has not yet been confirmed
	// fully applied. See the type comment for why this exists.
	pending *config.ConfigSnapshot
	// lastReported throttles the heartbeat. The poll loop runs every few
	// seconds; the back-office only needs applied_at fresh enough to tell a
	// running proxy from a stopped one.
	lastReported time.Time
}

// onPollResult is config.ConfigClient.Poll's onResult callback.
func (cp *configPoller) onPollResult(ctx context.Context, snap *config.ConfigSnapshot, err error) {
	if err != nil {
		log.Printf("config poll failed, keeping last known-good configuration (revision %d): %v", cp.manager.AppliedRevision(), err)
		return
	}

	if snap != nil {
		// A genuinely new snapshot always supersedes whatever was pending
		// before, whether or not that one ever applied.
		cp.pending = snap
	} else {
		// 304 Not Modified. If the last snapshot we saw never fully
		// applied, this is exactly the poll that should retry it, not a
		// no-op.
		if cp.pending == nil {
			// Nothing to do, but the proxy is alive and up to date: say so,
			// or the back-office cannot tell it from one that has stopped.
			cp.reportApplied(ctx, cp.manager.AppliedRevision(), false)
			return
		}
		snap = cp.pending
	}

	cp.apply(ctx, snap)
}

// reportApplied tells the API which revision is running. A change reports
// immediately; an idle poll reports at most once per heartbeatInterval.
func (cp *configPoller) reportApplied(ctx context.Context, rev int, force bool) {
	if !force && time.Since(cp.lastReported) < heartbeatInterval {
		return
	}
	cp.lastReported = time.Now()
	if err := cp.client.ReportApplied(ctx, rev); err != nil {
		log.Printf("could not report applied revision %d to the API: %v", rev, err)
	}
}

// apply runs one Manager.Plan + Reconciler.Apply cycle for snap. cp.pending
// is only cleared once snap is confirmed fully applied (or confirmed to
// need no reconciliation at all) — every other return path leaves it set,
// so the next poll result (304 or not) retries this same snapshot instead
// of being silently dropped. The client's cached ETag is committed for real
// (CommitETag) on those same success paths only, so a poll that follows a
// failure compares against the last snapshot actually *applied*, not the
// last one merely *seen* — see config.ConfigClient.Fetch and CommitETag's comments
// in ../config/configclient.go for why committing it any earlier caused this bug in
// the first place.
func (cp *configPoller) apply(ctx context.Context, snap *config.ConfigSnapshot) {
	// Anything recorded as applied that has no listener any more (a failed
	// port swap that could not put a server back, say) would otherwise be
	// planned as "unchanged" forever. Forget it so this Plan re-adds it.
	if gone := forgetMissingListeners(cp.manager, cp.reconciler); len(gone) > 0 {
		log.Printf("reconcile: server(s) %v were recorded as applied but have no running listener; they will be added again", gone)
	}

	items, err := cp.manager.Plan(snap)
	if err != nil {
		log.Printf("config revision %d failed validation, keeping last known-good configuration (revision %d): %v",
			snap.Revision, cp.manager.AppliedRevision(), err)
		return
	}
	if len(items) == 0 {
		// Same servers, only the revision number moved
		cp.manager.RecordRevision(snap.Revision)
		cp.client.CommitETag(snap.ETag)
		cp.pending = nil
		cp.reportApplied(ctx, snap.Revision, true)
		return
	}

	prevRevision := cp.manager.AppliedRevision()
	results := cp.reconciler.Apply(ctx, items)
	succeeded := 0
	var added, updated, moved, removed int
	for _, res := range results {
		if res.Err != nil {
			continue
		}
		cp.manager.RecordApplied(res.Item)
		succeeded++
		switch res.Item.Action {
		case ActionAdd:
			added++
		case ActionUpdateInPlace:
			updated++
		case ActionMovePort:
			moved++
		case ActionRemove:
			removed++
		}
	}

	if AllSucceeded(results) {
		cp.manager.RecordRevision(snap.Revision)
		cp.client.CommitETag(snap.ETag)
		cp.pending = nil
		log.Printf("reconciled config revision %d -> %d: %d added, %d updated, %d moved, %d removed (%d total)",
			prevRevision, snap.Revision, added, updated, moved, removed, len(items))
		// Best effort: a failed report leaves the back-office showing a stale
		// applied revision, but must not undo a reconciliation that worked.
		cp.reportApplied(ctx, snap.Revision, true)
		return
	}
	// cp.pending is already snap (set in onPollResult above), so the next
	// poll result — 304 or not — retries exactly this snapshot.
	log.Printf("partial reconciliation failure for config revision %d: %d of %d change(s) applied "+
		"(%d added, %d updated, %d moved, %d removed); revision not advanced, unresolved item(s) will be "+
		"retried on the next poll — see the preceding per-server errors", snap.Revision, succeeded, len(items),
		added, updated, moved, removed)
}

// PollConfig applies API configuration updates until ctx is cancelled, retrying
// partially applied snapshots even when the API reports no new revision.
func PollConfig(ctx context.Context, client *config.ConfigClient, manager *Manager, reconciler *Reconciler) {
	poller := &configPoller{manager: manager, reconciler: reconciler, client: client}
	client.Poll(ctx, func(polled *config.ConfigSnapshot, pollErr error) {
		poller.onPollResult(ctx, polled, pollErr)
	})
}
