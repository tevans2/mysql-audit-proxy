// This file is strictly for testing the database logic

package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"lustre/internal/audit"
)

/*
 * TestStoreWriteAndGet verifies that the database insertion and retrieval logic works.
 * It writes a dummy event to an in-memory SQLite database and then pulls it back out
 * to ensure the data was saved and mapped to the Go structs correctly.
 */
func TestStoreWriteAndGet(t *testing.T) {
	// 1. Setup an isolated in-memory database just for this test
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to initialize in-memory database: %v", err)
	}

	// 2. Create a dummy connection event to insert
	events := []audit.Event{
		{
			ConnID:     "test-uuid-1",
			Kind:       "connect",
			At:         "2026-08-10T12:00:00Z",
			User:       "admin",
			Upstream:   "db-main",
			ClientAddr: "192.168.1.1",
			ProxyPort:  3306,
			AuthResult: "success",
		},
	}

	// 3. Execute the WriteBatch function to test database inserts
	err = testStore.WriteBatch(events)
	if err != nil {
		t.Fatalf("WriteBatch failed: %v", err)
	}

	// 4. Execute the GetConnections function to test database reads
	conns, _, err := testStore.GetConnections(Filter{})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}

	// 5. Verify the results are exactly what we expect
	if len(conns) != 1 {
		t.Errorf("Expected exactly 1 connection in the database, got %d", len(conns))
	}

	if len(conns) > 0 && conns[0].ConnID != "test-uuid-1" {
		t.Errorf("Expected the retrieved connection ID to be 'test-uuid-1', got '%s'", conns[0].ConnID)
	}
}

/*
 * TestUpstreamAddrRoundTrip verifies that upstream_addr survives ingest and read.
 * The proxy sends both fields per the data-model contract (§Confirmed decisions #4):
 * upstream is the configured server name, upstream_addr is what was really connected to.
 */
func TestUpstreamAddrRoundTrip(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to initialize in-memory database: %v", err)
	}

	err = testStore.WriteBatch([]audit.Event{
		{
			ConnID: "conn-addr-1", Kind: "connect", At: "2026-08-12T12:00:00Z",
			User: "alice", Upstream: "db-a", UpstreamAddr: "demo-mysql:3306",
		},
	})
	if err != nil {
		t.Fatalf("WriteBatch failed: %v", err)
	}

	conns, _, err := testStore.GetConnections(Filter{})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("Expected 1 connection, got %d", len(conns))
	}
	if conns[0].Upstream != "db-a" {
		t.Errorf("upstream = %q, want %q", conns[0].Upstream, "db-a")
	}
	if conns[0].UpstreamAddr != "demo-mysql:3306" {
		t.Errorf("upstream_addr = %q, want %q", conns[0].UpstreamAddr, "demo-mysql:3306")
	}
}

/*
 * TestAuthFailReasonRoundTrip verifies an auth_fail event's Err text actually
 * reaches GetConnections, not just the ingest request body. Per code review:
 * WriteBatch's INSERT for the connections table used to omit the column
 * entirely — the reason arrived over the wire and was then silently
 * discarded, despite the README and docs/design/data-model.md describing it
 * as forwarded to the API. "a failed login produces an auth_fail event with
 * the attempted username and reason" is the original ticket's "done when",
 * and a reason that never reaches a reader of the audit log doesn't satisfy
 * it, whatever the ingest payload contained.
 */
func TestAuthFailReasonRoundTrip(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to initialize in-memory database: %v", err)
	}

	const reason = `Error 1045 (28000): Access denied for user 'demo'@'127.0.0.1' (using password: YES)`
	err = testStore.WriteBatch([]audit.Event{
		{
			ConnID: "conn-auth-fail-1", Kind: "auth_fail", At: "2026-09-20T12:00:00Z",
			User: "demo", Upstream: "db-a", AuthResult: "fail", Err: reason,
		},
	})
	if err != nil {
		t.Fatalf("WriteBatch failed: %v", err)
	}

	conns, _, err := testStore.GetConnections(Filter{})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("Expected 1 connection, got %d", len(conns))
	}
	if conns[0].AuthFailReason == nil {
		t.Fatal("auth_fail_reason is nil, want the event's Err text")
	}
	if *conns[0].AuthFailReason != reason {
		t.Errorf("auth_fail_reason = %q, want %q", *conns[0].AuthFailReason, reason)
	}

	// GetConnection (singular) runs its own SELECT — cover it too, since a
	// column added only to the list query and not the detail one would leave
	// the connection detail page silently missing the reason.
	single, _, err := testStore.GetConnection("conn-auth-fail-1")
	if err != nil {
		t.Fatalf("GetConnection failed: %v", err)
	}
	if single == nil {
		t.Fatal("GetConnection returned nil for a connection that was just written")
	}
	if single.AuthFailReason == nil || *single.AuthFailReason != reason {
		t.Errorf("GetConnection auth_fail_reason = %v, want %q", single.AuthFailReason, reason)
	}
}

/*
 * TestAuthFailReasonNotClobberedByLaterConnect covers the retry/ordering case
 * the COALESCE(NULLIF(...)) pattern exists for: a connect event for the same
 * conn_id (there shouldn't be one for a real rejected login, since a failed
 * handshake never reaches handleClient's connect-event code — but a retried
 * or out-of-order batch is exactly the scenario the rest of this upsert
 * already defends against for every other column) must not blank out a
 * reason that was already stored, the same way it must not blank out an
 * already-stored client_addr or upstream.
 */
func TestAuthFailReasonNotClobberedByLaterConnect(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to initialize in-memory database: %v", err)
	}

	const reason = "unknown user \"nosuchuser\""
	if err := testStore.WriteBatch([]audit.Event{
		{ConnID: "conn-auth-fail-2", Kind: "auth_fail", At: "2026-09-20T12:00:00Z", User: "nosuchuser", Err: reason},
	}); err != nil {
		t.Fatalf("WriteBatch (auth_fail) failed: %v", err)
	}
	if err := testStore.WriteBatch([]audit.Event{
		{ConnID: "conn-auth-fail-2", Kind: "connect", At: "2026-09-20T12:00:01Z", User: "nosuchuser"},
	}); err != nil {
		t.Fatalf("WriteBatch (connect) failed: %v", err)
	}

	conns, _, err := testStore.GetConnections(Filter{})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("Expected 1 connection, got %d", len(conns))
	}
	if conns[0].AuthFailReason == nil || *conns[0].AuthFailReason != reason {
		t.Errorf("auth_fail_reason after a later connect upsert = %v, want %q", conns[0].AuthFailReason, reason)
	}
}

/*
 * TestDisconnectBeforeConnect covers an out-of-order batch, which a retrying sink
 * can produce. The row is created with no ts_connect, so the read must return it
 * intact with a null ts_connect rather than silently dropping a failed Scan.
 */
func TestDisconnectBeforeConnect(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to initialize in-memory database: %v", err)
	}

	err = testStore.WriteBatch([]audit.Event{
		{ConnID: "orphan-1", Kind: "disconnect", At: "2026-08-12T12:05:00Z"},
	})
	if err != nil {
		t.Fatalf("WriteBatch failed: %v", err)
	}

	conns, _, err := testStore.GetConnections(Filter{})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("Expected 1 connection, got %d", len(conns))
	}
	if conns[0].ConnID != "orphan-1" {
		t.Errorf("conn_id = %q, want %q", conns[0].ConnID, "orphan-1")
	}
	if conns[0].TsConnect != nil {
		t.Errorf("ts_connect = %v, want nil", *conns[0].TsConnect)
	}
	if conns[0].TsDisconnect == nil {
		t.Error("ts_disconnect is nil, want the disconnect timestamp")
	}

	// The connect event arriving later must fill in the gap, not overwrite it.
	err = testStore.WriteBatch([]audit.Event{
		{ConnID: "orphan-1", Kind: "connect", At: "2026-08-12T12:00:00Z", User: "bob"},
	})
	if err != nil {
		t.Fatalf("late connect WriteBatch failed: %v", err)
	}

	conns, _, err = testStore.GetConnections(Filter{})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("Expected the late connect to update the existing row, got %d rows", len(conns))
	}
	if conns[0].TsConnect == nil {
		t.Error("ts_connect is still nil after the connect event arrived")
	}
	if conns[0].DBUser != "bob" {
		t.Errorf("db_user = %q, want %q", conns[0].DBUser, "bob")
	}
}

/*
 * TestStatementRetryIsIdempotent covers the sink re-sending a batch it did not
 * get a 2xx for. Connections upsert on conn_id; statements de-duplicate on
 * (conn_id, ts, sql_text), so a retry must not inflate the audit log.
 */
func TestStatementRetryIsIdempotent(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to initialize in-memory database: %v", err)
	}

	batch := []audit.Event{
		{ConnID: "retry-1", Kind: "connect", At: "2026-08-12T12:00:00Z", User: "alice"},
		{ConnID: "retry-1", Kind: "statement", At: "2026-08-12T12:00:01Z", SQL: "SELECT 1", StmtType: "SELECT"},
	}
	for i := 0; i < 3; i++ {
		if err := testStore.WriteBatch(batch); err != nil {
			t.Fatalf("WriteBatch attempt %d failed: %v", i+1, err)
		}
	}

	conns, _, err := testStore.GetConnections(Filter{})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}
	if len(conns) != 1 {
		t.Errorf("Expected 1 connection after 3 identical batches, got %d", len(conns))
	}
	if len(conns) > 0 && conns[0].StatementCount != 1 {
		t.Errorf("statement_count = %d, want 1", conns[0].StatementCount)
	}

	stmts, _, err := testStore.GetStatements(StatementFilter{ConnID: "retry-1"})
	if err != nil {
		t.Fatalf("GetStatements failed: %v", err)
	}
	if len(stmts) != 1 {
		t.Errorf("Expected 1 statement after 3 identical batches, got %d", len(stmts))
	}
}

/*
 * TestDistinctStatementsAreKept guards the de-duplication key from being too
 * aggressive: the same SQL run twice on one connection at different times is
 * two real events, not a retry.
 */
func TestDistinctStatementsAreKept(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to initialize in-memory database: %v", err)
	}

	err = testStore.WriteBatch([]audit.Event{
		{ConnID: "dup-sql", Kind: "connect", At: "2026-08-12T12:00:00Z", User: "alice"},
		{ConnID: "dup-sql", Kind: "statement", At: "2026-08-12T12:00:01.100000Z", SQL: "SELECT 1"},
		{ConnID: "dup-sql", Kind: "statement", At: "2026-08-12T12:00:01.200000Z", SQL: "SELECT 1"},
	})
	if err != nil {
		t.Fatalf("WriteBatch failed: %v", err)
	}

	stmts, _, err := testStore.GetStatements(StatementFilter{ConnID: "dup-sql"})
	if err != nil {
		t.Fatalf("GetStatements failed: %v", err)
	}
	if len(stmts) != 2 {
		t.Errorf("Expected 2 statements at different timestamps, got %d", len(stmts))
	}
}

/*
 * TestEmptyResultsEncodeAsArray guards the JSON shape of an empty result set.
 * A nil slice marshals to null, and the UI iterates these fields directly, so
 * `for (const c of null)` throws and the page renders blank.
 *
 * The assertion has to be on the encoded JSON: len() is 0 for both a nil slice
 * and an empty one, so a length check would pass against the bug it guards.
 */
func TestEmptyResultsEncodeAsArray(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	conns, total, err := testStore.GetConnections(Filter{})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	body, err := json.Marshal(map[string]any{"connections": conns})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(body), `{"connections":[]}`; got != want {
		t.Errorf("empty connections encoded as %s, want %s", got, want)
	}

	stmts, total, err := testStore.GetStatements(StatementFilter{})
	if err != nil {
		t.Fatalf("GetStatements failed: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	body, err = json.Marshal(map[string]any{"statements": stmts})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(body), `{"statements":[]}`; got != want {
		t.Errorf("empty statements encoded as %s, want %s", got, want)
	}
}

/*
 * TestPaginationTotals verifies that the 'total' returned is the pre-pagination
 * count of all matching rows, while the actual slice length respects the limit.
 */
func TestPaginationTotals(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to initialize in-memory database: %v", err)
	}

	// Seed for 30 distinct connections and statements
	var events []audit.Event
	for i := 0; i < 30; i++ {
		connID := fmt.Sprintf("conn-%d", i)
		events = append(events,
			audit.Event{ConnID: connID, Kind: "connect", At: "2026-08-12T12:00:00Z"},
			audit.Event{ConnID: connID, Kind: "statement", At: "2026-08-12T12:00:01Z", SQL: "SELECT 1"},
		)
	}

	if err := testStore.WriteBatch(events); err != nil {
		t.Fatalf("WriteBatch failed: %v", err)
	}

	// Request limit=10 for connections
	conns, connTotal, err := testStore.GetConnections(Filter{Limit: 10})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}
	if len(conns) != 10 {
		t.Errorf("Expected exactly 10 connections (limit), got %d", len(conns))
	}
	if connTotal != 30 {
		t.Errorf("Expected total connections to be 30 (pre-pagination), got %d", connTotal)
	}

	// Request limit=10 for statements
	stmts, stmtTotal, err := testStore.GetStatements(StatementFilter{Limit: 10})
	if err != nil {
		t.Fatalf("GetStatements failed: %v", err)
	}
	if len(stmts) != 10 {
		t.Errorf("Expected exactly 10 statements (limit), got %d", len(stmts))
	}
	if stmtTotal != 30 {
		t.Errorf("Expected total statements to be 30 (pre-pagination), got %d", stmtTotal)
	}
}

/*
 * TestFromBoundary verifies that the '?from=' filter includes events that occur
 * at the exact boundary instant, checking microsecond precision alignment.
 */
func TestFromBoundary(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("Failed to initialize in-memory database: %v", err)
	}

	boundaryTime := "2026-08-12T12:00:00.000000Z"

	err = testStore.WriteBatch([]audit.Event{
		{ConnID: "bound-1", Kind: "connect", At: boundaryTime},
	})
	if err != nil {
		t.Fatalf("WriteBatch failed: %v", err)
	}

	// Filter with standard 'Z' format, should match the stored microsecond format
	conns, _, err := testStore.GetConnections(Filter{From: "2026-08-12T12:00:00Z"})
	if err != nil {
		t.Fatalf("GetConnections failed: %v", err)
	}

	if len(conns) != 1 {
		t.Fatalf("Expected the boundary filter to include the exact instant, got %d results", len(conns))
	}
}

/*
 * TestStoreRevisioning verifies that mutations (Add, Update, Delete) atomically
 * increment the revision metadata, and that failed mutations rollback safely.
 */
func TestStoreRevisioning(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}

	// 1. Initial revision should be 0
	rev, err := testStore.GetRevision()
	if err != nil {
		t.Fatalf("GetRevision failed: %v", err)
	}
	if rev != 0 {
		t.Errorf("Expected initial revision 0, got %d", rev)
	}

	// 2. AddServer successfully increments revision
	err = testStore.AddServer(audit.Server{
		Name: "db-rev", ListenPort: 3306, UpstreamHost: "host", UpstreamPort: 3306, UpstreamUser: "u", Enabled: true,
	}, "encrypted-pass")
	if err != nil {
		t.Fatalf("AddServer failed: %v", err)
	}

	rev, _ = testStore.GetRevision()
	if rev != 1 {
		t.Errorf("Expected revision 1 after AddServer, got %d", rev)
	}

	// 3. Failed AddServer (duplicate port constraint) does NOT increment revision
	err = testStore.AddServer(audit.Server{
		Name: "db-rev-fail", ListenPort: 3306, UpstreamHost: "host", UpstreamPort: 3306, UpstreamUser: "u", Enabled: true,
	}, "encrypted-pass")
	if err == nil {
		t.Fatalf("Expected unique constraint error for duplicate ListenPort, got nil")
	}

	rev, _ = testStore.GetRevision()
	if rev != 1 {
		t.Errorf("Expected revision 1 to remain unchanged after failed AddServer, got %d", rev)
	}

	// 4. UpdateServer successfully increments revision
	err = testStore.UpdateServer(1, audit.Server{
		Name: "db-rev", ListenPort: 3307, UpstreamHost: "host", UpstreamPort: 3306, UpstreamUser: "u", Enabled: true,
	}, "")
	if err != nil {
		t.Fatalf("UpdateServer failed: %v", err)
	}

	rev, _ = testStore.GetRevision()
	if rev != 2 {
		t.Errorf("Expected revision 2 after UpdateServer, got %d", rev)
	}

	// 5. DeleteServer successfully increments revision
	err = testStore.DeleteServer(1)
	if err != nil {
		t.Fatalf("DeleteServer failed: %v", err)
	}

	rev, _ = testStore.GetRevision()
	if rev != 3 {
		t.Errorf("Expected revision 3 after DeleteServer, got %d", rev)
	}
}

/*
 * TestUpdateAndDeleteServerUnknownIDReturnsErrNoRows guards the store half of the
 * review comment on PUT/DELETE for unknown IDs. The handler only knows to return
 * 404 instead of 500 because these functions specifically surface sql.ErrNoRows
 * (via a RowsAffected() == 0 check) rather than a plain nil error on a no-op
 * mutation; this pins that contract at the store layer directly.
 */
func TestUpdateAndDeleteServerUnknownIDReturnsErrNoRows(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}

	err = testStore.UpdateServer(999, audit.Server{
		Name: "ghost", ListenPort: 3306, UpstreamHost: "host", UpstreamPort: 3306, UpstreamUser: "u", Enabled: true,
	}, "encrypted-pass")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("UpdateServer on unknown ID: got err = %v, want sql.ErrNoRows", err)
	}

	err = testStore.DeleteServer(999)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("DeleteServer on unknown ID: got err = %v, want sql.ErrNoRows", err)
	}

	// Neither no-op mutation should have touched the revision counter.
	rev, _ := testStore.GetRevision()
	if rev != 0 {
		t.Errorf("Expected revision to stay 0 after no-op update/delete on unknown ID, got %d", rev)
	}
}

/*
 * TestGetServersAndRevisionAtomicity ensures servers and revisions are fetched
 * atomically in a single transaction to prevent stale-routing race conditions.
 * Uses a file-backed database to allow multiple pooled connections.
 */
func TestGetServersAndRevisionAtomicity(t *testing.T) {
	dbFile, err := os.CreateTemp("", "lustre-store-*.db")
	if err != nil {
		t.Fatalf("failed to create temp db file: %v", err)
	}
	dbPath := dbFile.Name()
	dbFile.Close()
	defer os.Remove(dbPath)
	defer os.Remove(dbPath + "-wal")
	defer os.Remove(dbPath + "-shm")

	testStore, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	defer testStore.Close()

	const writes = 150
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < writes; i++ {
			srv := audit.Server{
				Name:         fmt.Sprintf("race-%d", i),
				ListenPort:   20000 + i,
				UpstreamHost: "host",
				UpstreamPort: 3306,
				UpstreamUser: "u",
				Enabled:      true,
			}
			if err := testStore.AddServer(srv, "encrypted-pass"); err != nil {
				t.Errorf("AddServer(%d) failed: %v", i, err)
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			servers, rev, err := testStore.GetServersAndRevision()
			if err != nil {
				t.Errorf("GetServersAndRevision failed: %v", err)
				return
			}
			if len(servers) != rev {
				t.Errorf("Inconsistent read: %d servers at revision %d (want equal) — "+
					"GetServersAndRevision is not reading servers and revision atomically",
					len(servers), rev)
				return
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	wg.Wait()

	rev, err := testStore.GetRevision()
	if err != nil {
		t.Fatalf("GetRevision failed: %v", err)
	}
	if rev != writes {
		t.Errorf("Expected final revision %d after %d inserts, got %d", writes, writes, rev)
	}
}

/*
 * TestTimestampPrecisionRoundTrip ensures that the 6-digit fractional second precision
 * is preserved exactly when written to and read from the database, preventing
 * go-sqlite3 from trimming trailing zeros.
 */
func TestTimestampPrecisionRoundTrip(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}

	exactTimestamp := "2026-08-13T12:00:00.100000Z"

	err = testStore.WriteBatch([]audit.Event{
		{ConnID: "prec-1", Kind: "connect", At: exactTimestamp, User: "alice"},
		{ConnID: "prec-1", Kind: "statement", At: exactTimestamp, SQL: "SELECT 1"},
	})
	if err != nil {
		t.Fatalf("WriteBatch failed: %v", err)
	}

	// Use GetConnection to fetch by ID directly
	conn, stmts, err := testStore.GetConnection("prec-1")
	if err != nil {
		t.Fatalf("GetConnection failed: %v", err)
	}
	if conn == nil {
		t.Fatalf("Expected connection 'prec-1' to exist")
	}
	if conn.TsConnect == nil || *conn.TsConnect != exactTimestamp {
		var got string
		if conn.TsConnect != nil {
			got = *conn.TsConnect
		}
		t.Errorf("TsConnect = %q, want %q", got, exactTimestamp)
	}

	if len(stmts) != 1 {
		t.Fatalf("Expected 1 statement, got %d", len(stmts))
	}
	if stmts[0].Ts != exactTimestamp {
		t.Errorf("Ts = %q, want %q", stmts[0].Ts, exactTimestamp)
	}
}

func TestSeedServersIfEmpty(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	seed := []audit.Server{
		{Name: "db-a", ListenPort: 3307, UpstreamHost: "a", UpstreamPort: 3306, UpstreamUser: "root", UpstreamPass: "enc-a", Enabled: true},
		{Name: "db-b", ListenPort: 3308, UpstreamHost: "b", UpstreamPort: 3306, UpstreamUser: "root", UpstreamPass: "enc-b", Enabled: true},
	}

	n, err := st.SeedServersIfEmpty(seed)
	if err != nil || n != 2 {
		t.Fatalf("first seed: n=%d err=%v, want 2 rows", n, err)
	}
	if rev, _ := st.GetRevision(); rev != 1 {
		t.Errorf("revision = %d, want 1 (one bump for the whole seed)", rev)
	}

	// Second run is a no-op: no duplicates, no revision bump.
	if n, err := st.SeedServersIfEmpty(seed); err != nil || n != 0 {
		t.Fatalf("second seed: n=%d err=%v, want 0", n, err)
	}
	if got, _ := st.GetServers(); len(got) != 2 {
		t.Errorf("after re-seed have %d servers, want 2", len(got))
	}
	if rev, _ := st.GetRevision(); rev != 1 {
		t.Errorf("revision = %d after no-op seed, want 1", rev)
	}
}

// A failure part-way must leave nothing behind, so the next boot retries.
func TestSeedServersIfEmptyIsAtomic(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	bad := []audit.Server{
		{Name: "db-a", ListenPort: 3307, UpstreamHost: "a", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true},
		{Name: "db-b", ListenPort: 3307, UpstreamHost: "b", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true},
	}
	if _, err := st.SeedServersIfEmpty(bad); err == nil {
		t.Fatal("want an error for the duplicate listen_port")
	}
	if got, _ := st.GetServers(); len(got) != 0 {
		t.Errorf("partial seed left %d rows behind, want 0", len(got))
	}
	if rev, _ := st.GetRevision(); rev != 0 {
		t.Errorf("revision = %d after failed seed, want 0", rev)
	}
}

func TestSeedAdminIfEmpty(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	// Generate a valid bcrypt hash for the test
	validHashBytes, _ := bcrypt.GenerateFromPassword([]byte("testpass"), bcrypt.MinCost)
	validHash := string(validHashBytes)

	// 1. Test that it creates the admin when empty
	err = testStore.SeedAdminIfEmpty("admin", validHash)
	if err != nil {
		t.Fatalf("SeedAdminIfEmpty failed on empty db: %v", err)
	}

	_, disabled, err := testStore.GetAdmin("admin")
	if err != nil {
		t.Errorf("Expected 1 admin after seeding, got error: %v", err)
	}
	if disabled != 0 {
		t.Errorf("Expected seeded admin to be active, got disabled=%d", disabled)
	}

	// 2. Test that it does NOT overwrite an existing admin
	testStore.CreateAdmin("existing-admin", validHash)

	err = testStore.SeedAdminIfEmpty("new-admin", validHash)
	if err != nil {
		t.Fatalf("SeedAdminIfEmpty failed on populated db: %v", err)
	}

	_, _, err = testStore.GetAdmin("new-admin")
	if err == nil {
		t.Errorf("Expected 0 new admins after seeding a populated db, but new-admin was found")
	}
}

func TestGetStatementsEscapesWildcards(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	err = testStore.WriteBatch([]audit.Event{
		{ConnID: "1", Kind: "connect", At: "2026-10-12T11:59:00Z"},
		{ConnID: "2", Kind: "connect", At: "2026-10-12T12:00:30Z"},
		{ConnID: "1", Kind: "statement", SQL: "SELECT 100% FROM sales", At: "2026-10-12T12:00:00Z"},
		{ConnID: "2", Kind: "statement", SQL: "SELECT * FROM sales", At: "2026-10-12T12:01:00Z"},
		{ConnID: "3", Kind: "connect", At: "2026-10-12T12:02:00Z"},
		{ConnID: "3", Kind: "statement", SQL: "SELECT user_id FROM users", At: "2026-10-12T12:03:00Z"},
		{ConnID: "4", Kind: "connect", At: "2026-10-12T12:04:00Z"},
		{ConnID: "4", Kind: "statement", SQL: "path\\to\\file", At: "2026-10-12T12:05:00Z"},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// 1. Searching for "%" should ONLY return the literal %, not act as a wildcard
	stmts, _, err := testStore.GetStatements(StatementFilter{Q: "%"})
	if err != nil {
		t.Fatalf("GetStatements: %v", err)
	}
	if len(stmts) != 1 || stmts[0].ConnID != "1" {
		t.Errorf("Expected 1 result containing literal '%%', got %d", len(stmts))
	}

	// 2. Searching for "_" should ONLY return the literal _, not act as a wildcard
	stmts, _, err = testStore.GetStatements(StatementFilter{Q: "_"})
	if err != nil {
		t.Fatalf("GetStatements: %v", err)
	}
	if len(stmts) != 1 || stmts[0].ConnID != "3" {
		t.Errorf("Expected 1 result containing literal '_', got %d", len(stmts))
	}

	// 3. Searching for "\" should ONLY return the literal \
	stmts, _, err = testStore.GetStatements(StatementFilter{Q: "\\"})
	if err != nil {
		t.Fatalf("GetStatements: %v", err)
	}
	if len(stmts) != 1 || stmts[0].ConnID != "4" {
		t.Errorf("Expected 1 result containing literal '\\', got %d", len(stmts))
	}
}

func TestGetConnectionsOutcomeFilter(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	err = testStore.WriteBatch([]audit.Event{
		{ConnID: "ok-conn", Kind: "connect", AuthResult: "ok", At: "2026-10-12T12:00:00Z"},
		{ConnID: "fail-conn", Kind: "auth_fail", AuthResult: "fail", At: "2026-10-12T12:01:00Z"},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	conns, total, err := testStore.GetConnections(Filter{Outcome: "fail"})
	if err != nil {
		t.Fatalf("GetConnections: %v", err)
	}

	if total != 1 {
		t.Errorf("Expected total 1, got %d", total)
	}
	if len(conns) != 1 || conns[0].ConnID != "fail-conn" {
		t.Errorf("Expected 1 result with ConnID 'fail-conn', got %d", len(conns))
	}
}

/*
 * TestHideHandshakeFiltersBeforePagination is the point of doing this in SQL
 * rather than in the viewer.
 *
 * The store is filled with more handshake statements than fit in one page,
 * followed by the two statements a person actually ran. A viewer that fetched
 * a page and filtered it client-side would receive handshake rows only, drop
 * all of them, and render nothing — the bug this replaces. Filtering in the
 * query means the page is spent on real statements instead.
 */
func TestHideHandshakeFiltersBeforePagination(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	batch := []audit.Event{{ConnID: "bulk-1", Kind: "connect", At: "2026-09-24T09:00:00.000000Z", User: "alice"}}
	// 60 handshake statements: more than the page size used below, so they
	// would fill it entirely.
	for i := 0; i < 60; i++ {
		batch = append(batch, audit.Event{
			ConnID:   "bulk-1",
			Kind:     "statement",
			At:       fmt.Sprintf("2026-09-24T09:00:%02d.000000Z", i),
			SQL:      "SELECT @@version_comment LIMIT 1",
			StmtType: "SELECT",
		})
	}
	batch = append(batch,
		audit.Event{ConnID: "bulk-1", Kind: "statement", At: "2026-09-24T09:02:00.000000Z", SQL: "SELECT * FROM sales.customers", StmtType: "SELECT"},
		audit.Event{ConnID: "bulk-1", Kind: "statement", At: "2026-09-24T09:02:01.000000Z", SQL: "DELETE FROM sales.orders WHERE id = 7", StmtType: "DELETE"},
	)
	if err := testStore.WriteBatch(batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// Unfiltered, a 50-row page is entirely handshake noise.
	noisy, noisyTotal, err := testStore.GetStatements(StatementFilter{Limit: 50})
	if err != nil {
		t.Fatalf("GetStatements: %v", err)
	}
	if noisyTotal != 62 {
		t.Errorf("unfiltered total = %d, want 62", noisyTotal)
	}
	if len(noisy) != 50 {
		t.Fatalf("unfiltered page = %d rows, want 50", len(noisy))
	}

	stmts, total, err := testStore.GetStatements(StatementFilter{Limit: 50, HideHandshake: true})
	if err != nil {
		t.Fatalf("GetStatements(HideHandshake): %v", err)
	}
	if total != 2 {
		t.Errorf("filtered total = %d, want 2 — the total must reflect the filter, or the viewer's count is a lie", total)
	}
	if len(stmts) != 2 {
		t.Fatalf("filtered page = %d rows, want 2", len(stmts))
	}
	for _, s := range stmts {
		if strings.Contains(strings.ToLower(s.SQL), "@@version_comment") {
			t.Errorf("handshake statement survived the filter: %q", s.SQL)
		}
	}
}

/*
 * TestHideHandshakeMatchesRealClientTraffic pins the patterns against the
 * statements drivers actually send, and — more importantly — against the ones
 * they don't. A filter on an audit view that swallows a user's own statement
 * is worse than the noise it removes, so the negative cases are the ones worth
 * keeping.
 */

/*
 * TestHideHandshakeMatchesRealClientTraffic pins the patterns against the
 * statements drivers actually send, and — more importantly — against the ones
 * they don't. A filter on an audit view that swallows a user's own statement
 * is worse than the noise it removes, so the negative cases are the ones worth
 * keeping.
 */
func TestHideHandshakeMatchesRealClientTraffic(t *testing.T) {
	hidden := []string{
		"SELECT @@version_comment LIMIT 1",
		"SET NAMES utf8mb4",
		"SET NAMES utf8mb4 COLLATE utf8mb4_unicode_ci",
		"SET autocommit=1",
		"SELECT DATABASE()",
		"SELECT @@session.transaction_isolation",
		"SHOW VARIABLES LIKE 'max_allowed_packet'",
		"SHOW COLLATION",
	}
	kept := []string{
		"SELECT * FROM sales.customers",
		"SELECT name FROM lustre_test.upstream_marker",
		"DELETE FROM sales.orders WHERE id = 7",
		"SHOW TABLES",
		"SELECT version_comment FROM audit_notes",
		"select  @@version_comment  limit 1",
		"UPDATE hr.employees SET salary = 1 WHERE id = 2",

		// Multi-clause statement disguised as a handshake must NOT be hidden
		"select @@version_comment limit 1; drop table users",
	}

	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	batch := []audit.Event{{ConnID: "mix-1", Kind: "connect", At: "2026-09-24T10:00:00.000000Z", User: "alice"}}
	for i, sql := range append(append([]string{}, hidden...), kept...) {
		batch = append(batch, audit.Event{
			ConnID:   "mix-1",
			Kind:     "statement",
			At:       fmt.Sprintf("2026-09-24T10:%02d:00.000000Z", i),
			SQL:      sql,
			StmtType: "SELECT",
		})
	}
	if err := testStore.WriteBatch(batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	stmts, total, err := testStore.GetStatements(StatementFilter{Limit: 500, HideHandshake: true})
	if err != nil {
		t.Fatalf("GetStatements: %v", err)
	}
	if total != len(kept) {
		t.Errorf("total = %d, want %d", total, len(kept))
	}

	survived := map[string]bool{}
	for _, s := range stmts {
		survived[s.SQL] = true
	}
	for _, sql := range kept {
		if !survived[sql] {
			t.Errorf("user statement was hidden: %q", sql)
		}
	}
	for _, sql := range hidden {
		if survived[sql] {
			t.Errorf("handshake statement was not hidden: %q", sql)
		}
	}
}

// SetAppliedRevision only moves forward: a proxy that restarts reports 0
// before its first reconcile, and letting that through would make the
// back-office claim nothing is applied while the listeners are serving.
func TestSetAppliedRevisionIsForwardOnly(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}

	rev, at, err := testStore.GetAppliedRevision()
	if err != nil {
		t.Fatalf("GetAppliedRevision failed: %v", err)
	}
	if rev != 0 || at != "" {
		t.Errorf("before any report: applied_revision = %d, applied_at = %q; want 0 and empty", rev, at)
	}

	if err := testStore.SetAppliedRevision(5); err != nil {
		t.Fatalf("SetAppliedRevision(5) failed: %v", err)
	}
	if err := testStore.SetAppliedRevision(3); err != nil {
		t.Fatalf("SetAppliedRevision(3) failed: %v", err)
	}

	rev, _, err = testStore.GetAppliedRevision()
	if err != nil {
		t.Fatalf("GetAppliedRevision failed: %v", err)
	}
	if rev != 5 {
		t.Errorf("applied_revision = %d after reporting 3 behind 5, want 5", rev)
	}
}

// The timestamp advances even when the revision repeats. That is what the
// heartbeat relies on: an idle proxy re-reports the same number to show it is
// still running, and the viewer reads the timestamp, not the number.
func TestSetAppliedRevisionUpdatesTimestampOnRepeat(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}

	if err := testStore.SetAppliedRevision(4); err != nil {
		t.Fatalf("first SetAppliedRevision failed: %v", err)
	}
	_, firstAt, err := testStore.GetAppliedRevision()
	if err != nil {
		t.Fatalf("GetAppliedRevision failed: %v", err)
	}
	if firstAt == "" {
		t.Fatal("applied_at is empty after a report")
	}

	// tsFormat keeps microseconds, so a short sleep is enough to tell two
	// writes apart.
	time.Sleep(2 * time.Millisecond)

	if err := testStore.SetAppliedRevision(4); err != nil {
		t.Fatalf("second SetAppliedRevision failed: %v", err)
	}
	rev, secondAt, err := testStore.GetAppliedRevision()
	if err != nil {
		t.Fatalf("GetAppliedRevision failed: %v", err)
	}
	if rev != 4 {
		t.Errorf("applied_revision = %d after repeating 4, want 4", rev)
	}
	if secondAt == firstAt {
		t.Errorf("applied_at did not change on a repeated revision: %q", secondAt)
	}
}

/*
 * TestListActionsPaginatesNewestFirst walks the whole log page by page and
 * checks that every row appears exactly once. The rows are all written within
 * the same second, so a page boundary ordered by ts could repeat or skip one;
 * ordering by id cannot.
 */
func TestListActionsPaginatesNewestFirst(t *testing.T) {
	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	for i := 1; i <= 25; i++ {
		if err := testStore.LogAction("admin", "TEST_ACTION", fmt.Sprintf("Target-%d", i)); err != nil {
			t.Fatalf("LogAction %d: %v", i, err)
		}
	}

	var seen []string
	for offset := 0; ; offset += 10 {
		page, total, err := testStore.ListActions(10, offset)
		if err != nil {
			t.Fatalf("ListActions offset %d: %v", offset, err)
		}
		if total != 25 {
			t.Fatalf("total = %d, want 25 on every page", total)
		}
		if len(page) == 0 {
			break
		}
		for _, a := range page {
			seen = append(seen, a.Target)
		}
	}

	if len(seen) != 25 {
		t.Fatalf("walked %d rows, want 25: %v", len(seen), seen)
	}
	for i, target := range seen {
		if want := fmt.Sprintf("Target-%d", 25-i); target != want {
			t.Fatalf("row %d = %s, want %s (newest first, none repeated or skipped)", i, target, want)
		}
	}

	// A page past the end is empty, not an error, and still reports the total.
	page, total, err := testStore.ListActions(10, 100)
	if err != nil || len(page) != 0 || total != 25 {
		t.Errorf("past the end: %d rows, total %d, err %v", len(page), total, err)
	}
}

// seedFilterFixture stores two sessions on two upstreams and one statement of
// each type the kind filter has to place, plus two failures. Only c-a has
// disconnected.
func seedFilterFixture(t *testing.T, st *Store) {
	t.Helper()
	stmt := func(conn, typ, sql, errText string, sec int) audit.Event {
		return audit.Event{
			ConnID: conn, Kind: "statement", StmtType: typ, SQL: sql, Err: errText,
			At: fmt.Sprintf("2026-10-12T12:00:%02d.000000Z", sec),
		}
	}
	events := []audit.Event{
		{ConnID: "c-a", Kind: "connect", User: "alice", Upstream: "db-a", AuthResult: "ok", At: "2026-10-12T11:59:00.000000Z"},
		{ConnID: "c-b", Kind: "connect", User: "bob", Upstream: "db-b", AuthResult: "ok", At: "2026-10-12T11:59:30.000000Z"},
		stmt("c-a", "SELECT", "SELECT 1", "", 1),
		stmt("c-a", "SHOW", "SHOW TABLES", "", 2),
		stmt("c-a", "INSERT", "INSERT INTO t VALUES (1)", "", 3),
		stmt("c-a", "UPDATE", "UPDATE t SET a = 2", "duplicate key", 4),
		stmt("c-a", "CREATE", "CREATE TABLE t2 (a int)", "", 5),
		stmt("c-b", "DROP", "DROP TABLE t2", "", 6),
		stmt("c-b", "GRANT", "GRANT SELECT ON t TO bob", "", 7),
		stmt("c-b", "SELECT", "SELECT * FROM missing", "no such table", 8),
		// Neither a read, a write nor a schema change on their own.
		stmt("c-b", "SET", "SET NAMES utf8mb4", "", 9),
		stmt("c-b", "BEGIN", "BEGIN", "", 10),
		stmt("c-b", "COMMIT", "COMMIT", "", 11),
		stmt("c-b", "WITH", "WITH x AS (SELECT 1) SELECT * FROM x", "", 12),
		stmt("c-b", "CALL", "CALL refresh()", "", 13),
		{ConnID: "c-a", Kind: "disconnect", At: "2026-10-12T12:30:00.000000Z"},
	}
	if err := st.WriteBatch(events); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
}

func TestStatementKindFilter(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	seedFilterFixture(t, st)

	for kind, want := range map[string]int{
		"read":   3, // SELECT, SHOW, SELECT (failed)
		"write":  2, // INSERT, UPDATE
		"schema": 3, // CREATE, DROP, GRANT — not SET/BEGIN/COMMIT/WITH/CALL
	} {
		_, total, err := st.GetStatements(StatementFilter{Kind: kind})
		if err != nil {
			t.Fatalf("kind=%s: %v", kind, err)
		}
		if total != want {
			t.Errorf("kind=%s: total = %d, want %d", kind, total, want)
		}
	}

	if _, _, err := st.GetStatements(StatementFilter{Kind: "bogus"}); !errors.Is(err, ErrInvalidKind) {
		t.Errorf("unknown kind: err = %v, want ErrInvalidKind", err)
	}
}

func TestStatementHasErrorAndUpstreamFilters(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	seedFilterFixture(t, st)

	stmts, total, err := st.GetStatements(StatementFilter{HasError: true})
	if err != nil {
		t.Fatalf("has_error: %v", err)
	}
	if total != 2 || len(stmts) != 2 {
		t.Errorf("has_error: total %d, returned %d, want 2 and 2", total, len(stmts))
	}
	for _, s := range stmts {
		if s.Error == nil {
			t.Errorf("has_error returned a statement with no error: %q", s.SQL)
		}
	}

	_, total, err = st.GetStatements(StatementFilter{Upstream: "db-a"})
	if err != nil {
		t.Fatalf("upstream: %v", err)
	}
	if total != 5 {
		t.Errorf("upstream=db-a: total = %d, want 5", total)
	}

	// Filters combine: a failed statement on db-b.
	_, total, err = st.GetStatements(StatementFilter{Upstream: "db-b", HasError: true, Kind: "read"})
	if err != nil {
		t.Fatalf("combined: %v", err)
	}
	if total != 1 {
		t.Errorf("db-b + has_error + read: total = %d, want 1", total)
	}
}

func TestConnectionsOpenFilter(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	seedFilterFixture(t, st)

	conns, total, err := st.GetConnections(Filter{Open: true})
	if err != nil {
		t.Fatalf("GetConnections: %v", err)
	}
	if total != 1 || len(conns) != 1 || conns[0].ConnID != "c-b" {
		t.Errorf("open=1: total %d, conns %+v, want only c-b", total, conns)
	}

	_, total, err = st.GetConnections(Filter{})
	if err != nil || total != 2 {
		t.Errorf("without open: total %d err %v, want 2", total, err)
	}
}

// A failed login never receives a disconnect, so ts_disconnect stays NULL; it
// must not show up as an open session.
func TestConnectionsOpenFilterExcludesFailedLogins(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	err = st.WriteBatch([]audit.Event{
		{ConnID: "live", Kind: "connect", User: "alice", Upstream: "db-a", AuthResult: "ok", At: "2026-10-12T12:00:00.000000Z"},
		{ConnID: "failed", Kind: "auth_fail", User: "mallory", Upstream: "db-a", AuthResult: "fail", At: "2026-10-12T12:00:01.000000Z"},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	conns, total, err := st.GetConnections(Filter{Open: true})
	if err != nil {
		t.Fatalf("GetConnections: %v", err)
	}
	if total != 1 || len(conns) != 1 || conns[0].ConnID != "live" {
		t.Errorf("open=1 returned %d rows (total %d); want only the live session", len(conns), total)
	}
}

func TestRecordLoginAndListAdmins(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	for _, name := range []string{"alice", "bob"} {
		if err := st.CreateAdmin(name, "hash"); err != nil {
			t.Fatalf("CreateAdmin %s: %v", name, err)
		}
	}
	if err := st.RecordLogin("alice"); err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}

	admins, err := st.ListAdmins()
	if err != nil {
		t.Fatalf("ListAdmins: %v", err)
	}
	if len(admins) != 2 {
		t.Fatalf("got %d admins, want 2", len(admins))
	}
	if admins[0].Username != "alice" || admins[0].LastLoginAt == nil {
		t.Errorf("alice should have a last login, got %+v", admins[0])
	} else if _, err := time.Parse(time.RFC3339, *admins[0].LastLoginAt); err != nil {
		t.Errorf("last_login_at %q is not RFC 3339: %v", *admins[0].LastLoginAt, err)
	}
	if admins[1].LastLoginAt != nil {
		t.Errorf("bob never signed in, last_login_at = %q, want null", *admins[1].LastLoginAt)
	}
}

// An audit.db created before last_login_at existed must still open, and its
// admins must list with a null last login.
func TestLastLoginMigration(t *testing.T) {
	path := t.TempDir() + "/old.db"
	old, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, err = old.Exec(`CREATE TABLE admins (
		username TEXT PRIMARY KEY,
		pass_hash TEXT NOT NULL,
		created_at TEXT NOT NULL,
		disabled INTEGER NOT NULL DEFAULT 0
	);
	INSERT INTO admins (username, pass_hash, created_at) VALUES ('legacy', 'hash', '2026-09-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	old.Close()

	st, err := InitDB(path)
	if err != nil {
		t.Fatalf("InitDB on an old database: %v", err)
	}
	if _, err := InitDB(path); err != nil {
		t.Fatalf("InitDB is not repeatable: %v", err)
	}
	admins, err := st.ListAdmins()
	if err != nil || len(admins) != 1 || admins[0].LastLoginAt != nil {
		t.Fatalf("ListAdmins = %+v, %v; want one admin with no last login", admins, err)
	}
	if err := st.RecordLogin("legacy"); err != nil {
		t.Fatalf("RecordLogin: %v", err)
	}
}

func seedSummaryFixture(t *testing.T, st *Store) {
	t.Helper()
	stmt := func(conn, typ, sql, errText, at string) audit.Event {
		return audit.Event{ConnID: conn, Kind: "statement", StmtType: typ, SQL: sql, Err: errText, At: at}
	}
	err := st.WriteBatch([]audit.Event{
		// Two sessions on db-a with different statement counts (3 and 1): a
		// per-upstream count taken inside the connections GROUP BY would report
		// one of those instead of 4.
		{ConnID: "a-1", Kind: "connect", User: "alice", Upstream: "db-a", AuthResult: "ok", At: "2026-10-12T10:00:00.000000Z"},
		{ConnID: "a-2", Kind: "connect", User: "bob", Upstream: "db-a", AuthResult: "ok", At: "2026-10-12T10:05:00.000000Z"},
		{ConnID: "b-1", Kind: "connect", User: "carol", Upstream: "db-b", AuthResult: "ok", At: "2026-10-12T10:10:00.000000Z"},
		// A failed login: never disconnects, and is not an open session.
		{ConnID: "f-1", Kind: "auth_fail", User: "mallory", Upstream: "db-b", AuthResult: "fail", Err: "denied", At: "2026-10-12T10:15:00.000000Z"},

		stmt("a-1", "SELECT", "SELECT 1", "", "2026-10-12T10:01:00.000000Z"),
		stmt("a-1", "CREATE", "CREATE TABLE t (a int)", "", "2026-10-12T10:02:00.000000Z"),
		stmt("a-1", "SET", "SET NAMES utf8mb4", "", "2026-10-12T10:03:00.000000Z"),
		stmt("a-2", "DROP", "DROP TABLE t", "unknown table", "2026-10-12T10:06:00.000000Z"),
		stmt("b-1", "SELECT", "SELECT 2", "", "2026-10-12T10:11:00.000000Z"),
		stmt("b-1", "BEGIN", "BEGIN", "", "2026-10-12T10:12:00.000000Z"),
		stmt("b-1", "WITH", "WITH x AS (SELECT 1) SELECT * FROM x", "", "2026-10-12T10:13:00.000000Z"),

		{ConnID: "a-2", Kind: "disconnect", At: "2026-10-12T10:20:00.000000Z"},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
}

func TestGetSummary(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	seedSummaryFixture(t, st)

	got, err := st.GetSummary("", "")
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	want := Summary{
		Sessions:        4,
		OpenSessions:    2, // a-1 and b-1; a-2 disconnected, f-1 failed to log in
		FailedLogins:    1,
		StatementErrors: 1,
		SchemaChanges:   2, // CREATE and DROP — not SET, BEGIN or WITH
		ByUpstream: []UpstreamSummary{
			{Upstream: "db-a", OpenSessions: 1, Statements: 4},
			{Upstream: "db-b", OpenSessions: 1, Statements: 3},
		},
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("summary =\n %+v\nwant\n %+v", got, want)
	}
}

func TestGetSummaryTimeWindow(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	seedSummaryFixture(t, st)

	// Only db-b's session connected from 10:10, and only its statements ran.
	got, err := st.GetSummary("2026-10-12T10:10:00Z", "2026-10-12T11:00:00Z")
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}
	if got.Sessions != 2 || got.FailedLogins != 1 || got.SchemaChanges != 0 || got.StatementErrors != 0 {
		t.Errorf("windowed summary = %+v", got)
	}
	if len(got.ByUpstream) != 1 || got.ByUpstream[0] != (UpstreamSummary{Upstream: "db-b", OpenSessions: 1, Statements: 3}) {
		t.Errorf("windowed by_upstream = %+v", got.ByUpstream)
	}
}

// An empty store reports zeros and an empty list, not nulls or an error:
// SUM over no rows is NULL in SQL.
func TestGetSummaryEmpty(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	got, err := st.GetSummary("", "")
	if err != nil {
		t.Fatalf("GetSummary on an empty store: %v", err)
	}
	if got.Sessions != 0 || got.OpenSessions != 0 || got.StatementErrors != 0 || got.ByUpstream == nil || len(got.ByUpstream) != 0 {
		t.Errorf("empty summary = %+v", got)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"by_upstream":[]`) {
		t.Errorf("by_upstream should encode as [], got %s", b)
	}
}

// The verbs the UI badges as read or write must be the ones the kind filter
// returns, and the session-control verbs must be in no kind at all.
func TestStatementKindMembership(t *testing.T) {
	st, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	types := []string{"DESCRIBE", "DESC", "LOAD", "USE", "SET", "BEGIN", "COMMIT", "WITH", "CALL"}
	events := []audit.Event{{ConnID: "c", Kind: "connect", User: "u", Upstream: "db-a", AuthResult: "ok", At: "2026-10-12T12:00:00.000000Z"}}
	for i, typ := range types {
		events = append(events, audit.Event{
			ConnID: "c", Kind: "statement", StmtType: typ, SQL: typ + " x",
			At: fmt.Sprintf("2026-10-12T12:00:%02d.000000Z", i+1),
		})
	}
	if err := st.WriteBatch(events); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	for kind, want := range map[string][]string{
		"read":   {"DESCRIBE", "DESC"},
		"write":  {"LOAD"},
		"schema": nil, // USE, SET, BEGIN, COMMIT, WITH and CALL belong to no kind
	} {
		stmts, _, err := st.GetStatements(StatementFilter{Kind: kind})
		if err != nil {
			t.Fatalf("kind=%s: %v", kind, err)
		}
		var got []string
		for _, s := range stmts {
			got = append(got, s.StmtType)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("kind=%s returned %v, want %v", kind, got, want)
		}
	}
}

// Connector/J's connect-time variable probe exactly as it reaches the audit
// log, including its tag, the two spaces after SELECT, and the trailing newline.
const connectorJProbe = "/* mysql-connector-j-8.2.0 (Revision: 06a1f724497fd81c6a659131fda822c9e5085b6c) */SELECT  @@session.auto_increment_increment AS auto_increment_increment, @@character_set_client AS character_set_client, @@character_set_connection AS character_set_connection, @@character_set_results AS character_set_results, @@character_set_server AS character_set_server, @@collation_server AS collation_server, @@collation_connection AS collation_connection, @@init_connect AS init_connect, @@interactive_timeout AS interactive_timeout, @@license AS license, @@lower_case_table_names AS lower_case_table_names, @@max_allowed_packet AS max_allowed_packet, @@net_write_timeout AS net_write_timeout, @@performance_schema AS performance_schema, @@sql_mode AS sql_mode, @@system_time_zone AS system_time_zone, @@time_zone AS time_zone, @@transaction_isolation AS transaction_isolation, @@wait_timeout AS wait_timeout\n"

/*
 * TestHideHandshakeIgnoresClientTags checks what DBeaver and Connector/J
 * actually send: the handshake statements arrive behind a tag comment, and the
 * tag must not stop them being recognised. It also pins the other direction.
 * Removing the tag must never make a longer or hand-written statement match.
 */
func TestHideHandshakeIgnoresClientTags(t *testing.T) {
	hidden := []string{
		"/* ApplicationName=DBeaver 26.2.0 - Metadata */ SET autocommit=1",
		"/* ApplicationName=DBeaver 26.2.0 - SQLEditor <Script.sql> */ SHOW ENGINES",
		"SET character_set_results = NULL\n",
		"SHOW CHARSET",
		"SHOW PLUGINS",
		"SHOW VARIABLES LIKE 'lower_case_table_names'",
		"SELECT @@GLOBAL.character_set_server,@@GLOBAL.collation_server",
		"SELECT * FROM information_schema.TABLES t WHERE t.TABLE_SCHEMA = 'information_schema' AND t.TABLE_NAME = 'CHECK_CONSTRAINTS'",
		connectorJProbe,
	}
	kept := []string{
		// Not session setup: a person could have typed these.
		"/* ApplicationName=DBeaver 26.2.0 - SQLEditor */ SELECT * FROM sales.customers",
		"/* ApplicationName=DBeaver 26.2.0 - Metadata */ SHOW FULL TABLES FROM sales",
		"show databases",

		// A tag in front of something longer must not borrow its way in.
		"/* tag */ select database(); drop table users",
		"/* tag */ select database() union select password from users",
		connectorJProbe + " , (SELECT 1)",
		"/* tag */ " + "SELECT  @@session.auto_increment_increment AS auto_increment_increment, (SELECT secret FROM hr.salaries LIMIT 1)",

		// /*! is executed by MySQL and /*+ is a hint: neither is a tag.
		"/*!50000 select database() */",
		"/*+ MAX_EXECUTION_TIME(1) */ select database()",

		// Only one tag is removed, and an unterminated or lone comment is no tag.
		"/* a */ /* b */ select database()",
		"/* select database()",
		"/* only a comment */",
	}

	testStore, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	batch := []audit.Event{{ConnID: "tag-1", Kind: "connect", At: "2026-10-16T10:00:00.000000Z", User: "alice"}}
	for i, sql := range append(append([]string{}, hidden...), kept...) {
		batch = append(batch, audit.Event{
			ConnID: "tag-1", Kind: "statement",
			At:  fmt.Sprintf("2026-10-16T10:%02d:00.000000Z", i),
			SQL: sql, StmtType: "SELECT",
		})
	}
	if err := testStore.WriteBatch(batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	stmts, total, err := testStore.GetStatements(StatementFilter{Limit: 500, HideHandshake: true})
	if err != nil {
		t.Fatalf("GetStatements: %v", err)
	}
	if total != len(kept) {
		t.Errorf("total = %d, want %d", total, len(kept))
	}
	survived := map[string]bool{}
	for _, s := range stmts {
		survived[s.SQL] = true
	}
	for _, sql := range kept {
		if !survived[sql] {
			t.Errorf("hidden but must stay visible: %q", sql)
		}
	}
	for _, sql := range hidden {
		if survived[sql] {
			t.Errorf("still visible but is client handshake noise: %q", sql)
		}
	}
}
