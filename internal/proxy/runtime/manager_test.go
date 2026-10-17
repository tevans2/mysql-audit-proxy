package runtime

// Unit tests for Manager.Plan — the reconciliation manager's
// snapshot-diffing logic (see the package comment on manager.go). These are
// pure in-memory tests: no real listener, no network, no goroutine. Every
// case here is drawn directly from the ticket's "Add unit tests for
// configuration diffing" list.

import (
	"errors"
	"reflect"
	"testing"

	"lustre/internal/audit"
	"lustre/internal/proxy/config"
)

func serverA() audit.Server {
	return audit.Server{ID: 1, Name: "db-a", ListenPort: 3307, UpstreamHost: "db-a", UpstreamPort: 3306, UpstreamUser: "root", UpstreamPass: "rootpass", Enabled: true}
}

func serverB() audit.Server {
	return audit.Server{ID: 2, Name: "db-b", ListenPort: 3308, UpstreamHost: "db-b", UpstreamPort: 3306, UpstreamUser: "root", UpstreamPass: "rootpass-b", Enabled: true}
}

// applyAll runs every item in a plan through m.RecordApplied, simulating a
// fully successful reconciliation, and records the snapshot's revision.
// Tests that need a manager already "warmed up" to some baseline use this
// rather than reaching into Manager's unexported fields.
func applyAll(t *testing.T, m *Manager, snap *config.ConfigSnapshot) []PlanItem {
	t.Helper()
	items, err := m.Plan(snap)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, item := range items {
		m.RecordApplied(item)
	}
	m.RecordRevision(snap.Revision)
	return items
}

func findItem(t *testing.T, items []PlanItem, serverID int) PlanItem {
	t.Helper()
	for _, item := range items {
		if item.ServerID == serverID {
			return item
		}
	}
	t.Fatalf("no plan item for server ID %d in %+v", serverID, items)
	return PlanItem{}
}

// TestManager_PlanAddsNewEnabledServer is the "Add listener" case: a fresh
// manager, given a snapshot with one enabled server it has never seen,
// proposes ActionAdd.
func TestManager_PlanAddsNewEnabledServer(t *testing.T) {
	m := NewManager()
	snap := &config.ConfigSnapshot{Revision: 1, Servers: []audit.Server{serverA()}}

	items, err := m.Plan(snap)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("Plan returned %d items, want 1: %+v", len(items), items)
	}
	item := items[0]
	if item.ServerID != 1 || item.Action != ActionAdd {
		t.Errorf("item = %+v, want {ServerID: 1, Action: ActionAdd}", item)
	}
	if item.Desired != serverA() {
		t.Errorf("item.Desired = %+v, want %+v", item.Desired, serverA())
	}
}

// TestManager_PlanUpdatesUpstreamOnSamePort is the "Update upstream on the
// same port" case: host/user/password/name can change without the listen
// port changing, and that must come back as ActionUpdateInPlace, not
// ActionMovePort.
func TestManager_PlanUpdatesUpstreamOnSamePort(t *testing.T) {
	m := NewManager()
	applyAll(t, m, &config.ConfigSnapshot{Revision: 1, Servers: []audit.Server{serverA()}})

	changed := serverA()
	changed.UpstreamHost = "db-a-replica"
	changed.UpstreamPass = "new-password"

	items, err := m.Plan(&config.ConfigSnapshot{Revision: 2, Servers: []audit.Server{changed}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	item := findItem(t, items, 1)
	if item.Action != ActionUpdateInPlace {
		t.Errorf("Action = %v, want ActionUpdateInPlace", item.Action)
	}
	if item.Desired != changed {
		t.Errorf("Desired = %+v, want %+v", item.Desired, changed)
	}
	if item.Previous != serverA() {
		t.Errorf("Previous = %+v, want the original config", item.Previous)
	}
}

// TestManager_PlanMovesListenerToNewPort is the "Move listener to a new
// port" case: when ListenPort itself changes, that must come back as
// ActionMovePort specifically — Apply (a later chunk) treats this
// differently from an in-place update (bind the new port first, only then
// drain the old one).
func TestManager_PlanMovesListenerToNewPort(t *testing.T) {
	m := NewManager()
	applyAll(t, m, &config.ConfigSnapshot{Revision: 1, Servers: []audit.Server{serverA()}})

	moved := serverA()
	moved.ListenPort = 4000

	items, err := m.Plan(&config.ConfigSnapshot{Revision: 2, Servers: []audit.Server{moved}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	item := findItem(t, items, 1)
	if item.Action != ActionMovePort {
		t.Errorf("Action = %v, want ActionMovePort", item.Action)
	}
	if item.Desired.ListenPort != 4000 {
		t.Errorf("Desired.ListenPort = %d, want 4000", item.Desired.ListenPort)
	}
}

// TestManager_PlanRemovesDisabledServer is the "Disable listener" case: a
// server that's still present in the snapshot but now has Enabled=false
// must produce ActionRemove.
func TestManager_PlanRemovesDisabledServer(t *testing.T) {
	m := NewManager()
	applyAll(t, m, &config.ConfigSnapshot{Revision: 1, Servers: []audit.Server{serverA()}})

	disabled := serverA()
	disabled.Enabled = false

	items, err := m.Plan(&config.ConfigSnapshot{Revision: 2, Servers: []audit.Server{disabled}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	item := findItem(t, items, 1)
	if item.Action != ActionRemove {
		t.Errorf("Action = %v, want ActionRemove", item.Action)
	}
	if item.Previous != serverA() {
		t.Errorf("Previous = %+v, want the original applied config", item.Previous)
	}
}

// TestManager_PlanRemovesDeletedServer is the "Delete listener" case: a
// server absent from the snapshot entirely (not just disabled) must also
// produce ActionRemove — deletion and disabling are handled identically,
// per the ticket grouping them under one requirement.
func TestManager_PlanRemovesDeletedServer(t *testing.T) {
	m := NewManager()
	applyAll(t, m, &config.ConfigSnapshot{Revision: 1, Servers: []audit.Server{serverA(), serverB()}})

	// serverB is simply not in this snapshot at all.
	items, err := m.Plan(&config.ConfigSnapshot{Revision: 2, Servers: []audit.Server{serverA()}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("Plan returned %d items, want 1 (only server B's removal): %+v", len(items), items)
	}
	item := findItem(t, items, 2)
	if item.Action != ActionRemove {
		t.Errorf("Action = %v, want ActionRemove", item.Action)
	}
}

// TestManager_PlanReturnsNoItemsForUnchangedSnapshot is the "Unchanged
// snapshot" case: re-planning the exact configuration already applied must
// produce zero items, not a pile of no-op entries.
func TestManager_PlanReturnsNoItemsForUnchangedSnapshot(t *testing.T) {
	m := NewManager()
	snap := &config.ConfigSnapshot{Revision: 1, Servers: []audit.Server{serverA(), serverB()}}
	applyAll(t, m, snap)

	items, err := m.Plan(&config.ConfigSnapshot{Revision: 2, Servers: []audit.Server{serverA(), serverB()}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("Plan on an unchanged configuration returned %d items, want 0: %+v", len(items), items)
	}
}

// TestManager_PlanRejectsInvalidSnapshotAndLeavesStateUntouched is the
// "Invalid snapshot" case, combined with the failure-safety requirement
// that a validation failure must not disturb the manager's existing state.
func TestManager_PlanRejectsInvalidSnapshotAndLeavesStateUntouched(t *testing.T) {
	m := NewManager()
	applyAll(t, m, &config.ConfigSnapshot{Revision: 1, Servers: []audit.Server{serverA()}})
	before := m.AppliedServers()
	beforeRevision := m.AppliedRevision()

	invalid := serverB()
	invalid.UpstreamHost = "" // config.ValidateServers requires this

	_, err := m.Plan(&config.ConfigSnapshot{Revision: 2, Servers: []audit.Server{serverA(), invalid}})
	if err == nil {
		t.Fatal("Plan on a snapshot with a missing upstream_host returned no error")
	}
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Errorf("err = %v, want errors.Is(err, ErrInvalidSnapshot)", err)
	}

	if !reflect.DeepEqual(m.AppliedServers(), before) {
		t.Errorf("AppliedServers changed after a failed Plan: got %+v, want %+v", m.AppliedServers(), before)
	}
	if m.AppliedRevision() != beforeRevision {
		t.Errorf("AppliedRevision changed after a failed Plan: got %d, want %d", m.AppliedRevision(), beforeRevision)
	}
}

// TestManager_PlanIsReadOnly confirms Plan never mutates the manager, no
// matter what it returns — calling it repeatedly with nothing in between
// must keep producing the identical plan.
func TestManager_PlanIsReadOnly(t *testing.T) {
	m := NewManager()
	snap := &config.ConfigSnapshot{Revision: 1, Servers: []audit.Server{serverA()}}

	first, err := m.Plan(snap)
	if err != nil {
		t.Fatalf("first Plan: %v", err)
	}
	second, err := m.Plan(snap)
	if err != nil {
		t.Fatalf("second Plan: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two Plan calls with nothing applied in between produced different results: %+v vs %+v", first, second)
	}
	if len(m.AppliedServers()) != 0 {
		t.Errorf("Plan alone (no RecordApplied) changed AppliedServers: %+v", m.AppliedServers())
	}
}

// TestManager_PartiallyFailedReconciliationKeepsFailedItemPending is the
// direct test for the ticket's "One server in a snapshot cannot be applied"
// failure-safety requirement, combined with "Only update the stored applied
// revision after successful reconciliation": if a caller applies server A's
// item but not server B's (simulating B's bind failing) and never calls
// RecordRevision, the next Plan against the same snapshot must still show A
// as unchanged and must still propose B again — the failure didn't get
// silently forgotten, and the success wasn't rolled back either.
func TestManager_PartiallyFailedReconciliationKeepsFailedItemPending(t *testing.T) {
	m := NewManager()
	snap := &config.ConfigSnapshot{Revision: 1, Servers: []audit.Server{serverA(), serverB()}}

	items, err := m.Plan(snap)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// Apply only server A's item — simulate server B's bind failing, so its
	// item is deliberately never recorded, and RecordRevision is never
	// called for this snapshot at all (reconciliation as a whole did not
	// succeed).
	m.RecordApplied(findItem(t, items, 1))

	if m.AppliedRevision() != 0 {
		t.Fatalf("AppliedRevision = %d after a partial failure, want 0 (unset)", m.AppliedRevision())
	}

	retry, err := m.Plan(snap)
	if err != nil {
		t.Fatalf("retry Plan: %v", err)
	}
	if len(retry) != 1 {
		t.Fatalf("retry Plan returned %d items, want 1 (only B, still pending): %+v", len(retry), retry)
	}
	retryItem := findItem(t, retry, 2)
	if retryItem.Action != ActionAdd {
		t.Errorf("retry Action for server B = %v, want ActionAdd (still never successfully applied)", retryItem.Action)
	}
}
