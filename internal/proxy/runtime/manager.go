package runtime

// Manager is the reconciliation half of the live-config-reconciliation
// work — see ../config/configclient.go for the polling half that
// feeds it snapshots, and Proxy.Run / session_test.go for the
// cancellable-listener primitive a later chunk uses to actually carry out
// a Plan against real listeners.
//
// This file gives Manager only its read side: Plan (diffing a new snapshot
// against what's currently applied) and the bookkeeping a caller uses to
// record what it actually managed to apply (RecordApplied, RecordRevision).
// It deliberately never binds or drains a real listener itself, and Plan
// never mutates the manager's own state — that split is what makes every
// case in the ticket's "unit tests for configuration diffing" list
// something this file's tests can assert on directly, with no mock
// listener or network call anywhere. Turning a Plan into real listener
// changes (Apply) is layered on top in a later chunk, once there is
// somewhere real for one to be carried out against.

import (
	"errors"
	"fmt"
	"sync"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/proxy/config"
)

// Action says what one server ID's listener needs, computed by Plan
// comparing the manager's last-applied configuration against a new
// snapshot. It says what to do, not how — turning an Action into a real
// bind, drain, or in-place credential swap is Apply's job.
type Action int

const (
	// ActionAdd: newly enabled, no listener yet — bind one.
	ActionAdd Action = iota + 1
	// ActionUpdateInPlace: same listen port, something else about the
	// upstream (host, port, user, password) or the name changed — apply to
	// new connections only; existing sessions keep the config they
	// connected with (see the ticket's "Immutable per-session
	// configuration" requirement).
	ActionUpdateInPlace
	// ActionMovePort: the listen port itself changed — bind the new port
	// first, and only begin draining the old listener once the new one is
	// confirmed up; preserve the old listener if the new port can't bind.
	ActionMovePort
	// ActionRemove: disabled or deleted from the snapshot — stop accepting
	// new connections, let existing sessions drain, then remove.
	ActionRemove
)

func (a Action) String() string {
	switch a {
	case ActionAdd:
		return "add"
	case ActionUpdateInPlace:
		return "update-in-place"
	case ActionMovePort:
		return "move-port"
	case ActionRemove:
		return "remove"
	default:
		return fmt.Sprintf("Action(%d)", int(a))
	}
}

// PlanItem is one server ID's worth of work a diff produced.
//
// Desired is the zero value for ActionRemove — there is no new
// configuration to apply, only Previous (what's currently running) matters
// for tearing it down. Previous is the zero value for ActionAdd — there is
// nothing currently running yet to compare against.
type PlanItem struct {
	ServerID int
	Action   Action
	Desired  audit.Server
	Previous audit.Server
}

// ErrInvalidSnapshot is returned by Plan when snap is nil or fails
// validation. It is a sentinel (wrapped with more detail via %w) so a
// caller can recognise "this snapshot failed validation" specifically —
// per the ticket's failure-safety requirements, that case must leave the
// last known-good configuration alone, same as every other Plan failure.
var ErrInvalidSnapshot = errors.New("proxy config snapshot failed validation")

// Manager tracks the proxy's currently-applied live configuration and
// computes what needs to change when a new snapshot arrives.
type Manager struct {
	mu       sync.Mutex
	revision int
	applied  map[int]audit.Server // server ID -> last successfully applied config
}

// NewManager returns an empty Manager: no revision seen yet, nothing
// applied. The first Plan against it proposes ActionAdd for every enabled
// server in whatever snapshot it's given.
func NewManager() *Manager {
	return &Manager{applied: make(map[int]audit.Server)}
}

// AppliedRevision returns the highest snapshot revision this manager has
// fully reconciled — see RecordRevision for what "fully" means.
func (m *Manager) AppliedRevision() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revision
}

// AppliedServers returns a copy of what this manager currently believes is
// running, keyed by server ID — safe to range over without racing a
// concurrent RecordApplied.
func (m *Manager) AppliedServers() map[int]audit.Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int]audit.Server, len(m.applied))
	for id, s := range m.applied {
		out[id] = s
	}
	return out
}

// Plan compares this manager's currently-applied configuration against
// every server in snap — enabled or not, since a disabled or deleted server
// that currently has a listener still needs an ActionRemove — and returns
// one PlanItem per server ID that needs something done. An unchanged server
// produces no item at all, so a caller ranges over exactly the work there
// is rather than filtering out a pile of no-op entries.
//
// Plan is read-only: it never touches m's own state (see RecordApplied /
// RecordRevision) and never touches a real listener.
//
// Only the enabled servers in snap are validated (via config.ValidateServers, the
// same check BuildProxies already applies to a static config file) — a
// disabled server never gets a listener, so a blank field or a port
// collision on its side can't cause an operational problem, and validating
// it anyway would reject snapshots for no operational reason. A validation
// failure, or a nil snap, returns (nil, error wrapping ErrInvalidSnapshot);
// since Plan never mutates m, the caller's last known-good configuration is
// already untouched by construction — there is nothing extra it needs to
// do to "preserve" it beyond simply not calling RecordApplied/RecordRevision
// for a Plan call that failed.
func (m *Manager) Plan(snap *config.ConfigSnapshot) ([]PlanItem, error) {
	if snap == nil {
		return nil, fmt.Errorf("%w: nil snapshot", ErrInvalidSnapshot)
	}

	var enabled []audit.Server
	for _, s := range snap.Servers {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	if err := config.ValidateServers(enabled); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSnapshot, err)
	}

	current := m.AppliedServers()

	desiredByID := make(map[int]audit.Server, len(snap.Servers))
	for _, s := range snap.Servers {
		desiredByID[s.ID] = s
	}

	var items []PlanItem

	for id, desired := range desiredByID {
		prev, exists := current[id]
		switch {
		case !desired.Enabled:
			if exists {
				items = append(items, PlanItem{ServerID: id, Action: ActionRemove, Previous: prev})
			}
			// else: disabled and was never applied — nothing to do.
		case !exists:
			items = append(items, PlanItem{ServerID: id, Action: ActionAdd, Desired: desired})
		case prev == desired:
			// Unchanged — no item.
		case prev.ListenPort != desired.ListenPort:
			items = append(items, PlanItem{ServerID: id, Action: ActionMovePort, Desired: desired, Previous: prev})
		default:
			items = append(items, PlanItem{ServerID: id, Action: ActionUpdateInPlace, Desired: desired, Previous: prev})
		}
	}

	// Anything currently applied but entirely absent from this snapshot —
	// not merely disabled, actually deleted — needs the same treatment as a
	// disabled server: stop it and remove it from the manager.
	for id, prev := range current {
		if _, stillPresent := desiredByID[id]; !stillPresent {
			items = append(items, PlanItem{ServerID: id, Action: ActionRemove, Previous: prev})
		}
	}

	return items, nil
}

// RecordApplied updates the manager's view of what's currently running for
// one server ID, based on a PlanItem that was actually carried out
// successfully.
//
// Call it once per PlanItem that Apply (a later chunk) finished
// successfully — a bind that succeeded, a drain that completed — and never
// for one that failed. A failed item must leave the manager's existing view
// of that server untouched, which is exactly what simply not calling this
// for it achieves: per the ticket, "One server in a snapshot cannot be
// applied" must not disturb the others, and it doesn't, because each
// server's applied state only ever changes via its own successful call
// here.
func (m *Manager) RecordApplied(item PlanItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if item.Action == ActionRemove {
		delete(m.applied, item.ServerID)
		return
	}
	m.applied[item.ServerID] = item.Desired
}

// RecordRevision marks snapshotRevision as the highest revision this
// manager has fully reconciled — every PlanItem Plan produced for it was
// applied successfully.
//
// Do not call this after a partially-failed reconciliation: the ticket
// requires the stored revision to advance only once reconciliation as a
// whole succeeded, so a later poll of the same still-partially-applied
// revision keeps retrying the parts that failed rather than being silently
// treated as "already seen, nothing to do". Individual servers that did
// succeed keep whatever RecordApplied already recorded for them regardless
// — this only gates the revision number, not per-server state.
func (m *Manager) RecordRevision(snapshotRevision int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revision = snapshotRevision
}
