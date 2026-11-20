package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
)

// cutoff for these tests: anything before 2026-09-15 is expired.
var testCutoff = time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

const (
	old    = "2026-08-01T10:00:00Z"
	recent = "2026-10-01T10:00:00Z"
)

func mustStore(t *testing.T) *Store {
	t.Helper()
	s, err := InitDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func write(t *testing.T, s *Store, events ...audit.Event) {
	t.Helper()
	if err := s.WriteBatch(events); err != nil {
		t.Fatal(err)
	}
}

func connect(id, at string) audit.Event {
	return audit.Event{ConnID: id, Kind: audit.KindConnect, At: at, User: "alice", Upstream: "db-a", AuthResult: audit.AuthOK}
}

func disconnect(id, at string) audit.Event {
	return audit.Event{ConnID: id, Kind: audit.KindDisconnect, At: at}
}

func stmt(id, at, sqlText string) audit.Event {
	return audit.Event{ConnID: id, Kind: audit.KindStatement, At: at, SQL: sqlText, StmtType: "SELECT"}
}

func connIDs(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	conns, _, err := s.GetConnections(Filter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		got[c.ConnID] = true
	}
	return got
}

func TestExpiredStatementsAreOnlyTheOldOnes(t *testing.T) {
	s := mustStore(t)
	write(t, s,
		connect("c1", old),
		stmt("c1", old, "SELECT 'old'"),
		stmt("c1", recent, "SELECT 'recent'"),
	)

	got, err := s.ExpiredStatements(testCutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SQL != "SELECT 'old'" {
		t.Fatalf("expired statements = %+v, want only the old one", got)
	}
}

func TestExpiredStatementsRespectsLimit(t *testing.T) {
	s := mustStore(t)
	write(t, s, connect("c1", old))
	for i := 0; i < 5; i++ {
		write(t, s, stmt("c1", old, fmt.Sprintf("SELECT %d", i)))
	}
	got, err := s.ExpiredStatements(testCutoff, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want the limit of 3", len(got))
	}
}

func TestRecordWithoutATimestampIsNeverExpired(t *testing.T) {
	s := mustStore(t)
	write(t, s, connect("c1", recent))
	// An event with no time stores an empty timestamp: it cannot be dated.
	write(t, s, stmt("c1", "", "SELECT 'undated'"))

	got, err := s.ExpiredStatements(testCutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("an undated statement was reported expired: %+v", got)
	}
}

func TestDeleteStatementsDeletesExactlyThoseIDs(t *testing.T) {
	s := mustStore(t)
	write(t, s, connect("c1", old), stmt("c1", old, "SELECT 1"), stmt("c1", old, "SELECT 2"), stmt("c1", old, "SELECT 3"))

	rows, err := s.ExpiredStatements(testCutoff, 100)
	if err != nil || len(rows) != 3 {
		t.Fatalf("setup: %v, %d rows", err, len(rows))
	}
	n, err := s.DeleteStatements([]int64{rows[0].ID, rows[2].ID})
	if err != nil || n != 2 {
		t.Fatalf("DeleteStatements = %d, %v; want 2, nil", n, err)
	}
	left, _ := s.ExpiredStatements(testCutoff, 100)
	if len(left) != 1 || left[0].SQL != "SELECT 2" {
		t.Fatalf("left = %+v, want only SELECT 2", left)
	}
}

func TestDeleteStatementsHandlesMoreIDsThanOneBatch(t *testing.T) {
	s := mustStore(t)
	write(t, s, connect("c1", old))
	events := make([]audit.Event, 0, 1300)
	for i := 0; i < 1300; i++ {
		events = append(events, stmt("c1", old, fmt.Sprintf("SELECT %d", i)))
	}
	write(t, s, events...)

	rows, err := s.ExpiredStatements(testCutoff, 5000)
	if err != nil || len(rows) != 1300 {
		t.Fatalf("setup: %v, %d rows", err, len(rows))
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	n, err := s.DeleteStatements(ids)
	if err != nil || n != 1300 {
		t.Fatalf("DeleteStatements = %d, %v; want 1300, nil", n, err)
	}
}

// The rules for which connections expire, one case per kind of connection.
func TestWhichConnectionsExpire(t *testing.T) {
	s := mustStore(t)
	write(t, s,
		// Finished long ago, no statements left: expires.
		connect("finished-old", old), disconnect("finished-old", old),
		// A failed login long ago: expires.
		audit.Event{ConnID: "failed-old", Kind: audit.KindAuthFail, At: old, User: "mallory", AuthResult: audit.AuthFail, Err: "denied"},
		// Never recorded a disconnect and nothing since: expires on connect time.
		connect("never-closed-old", old),
		// Connected long ago but disconnected recently: its last activity is recent.
		connect("long-session", old), disconnect("long-session", recent),
		// Recent.
		connect("recent", recent),
		// Connected long ago, still has a recent statement: kept whatever else.
		connect("old-with-recent-stmt", old), stmt("old-with-recent-stmt", recent, "SELECT 1"),
		// Only a disconnect arrived (the connect was lost): dated by the disconnect.
		disconnect("disconnect-only-old", old),
	)

	got, err := s.ExpiredConnections(testCutoff, 100)
	if err != nil {
		t.Fatal(err)
	}
	expired := map[string]bool{}
	for _, c := range got {
		expired[c.ConnID] = true
	}
	for _, id := range []string{"finished-old", "failed-old", "never-closed-old", "disconnect-only-old"} {
		if !expired[id] {
			t.Errorf("%s should have expired", id)
		}
	}
	for _, id := range []string{"long-session", "recent", "old-with-recent-stmt"} {
		if expired[id] {
			t.Errorf("%s must not expire", id)
		}
	}
}

// The counts feed the startup report and the dry run, so they must agree with
// what a purge would then remove, not just with what is expired right now.
func TestCountsMatchWhatAPurgeRemoves(t *testing.T) {
	s := mustStore(t)
	write(t, s,
		// Old session whose statements are all old: connection and 2 statements go.
		connect("gone", old), disconnect("gone", old), stmt("gone", old, "SELECT 1"), stmt("gone", old, "SELECT 2"),
		// Old session with one old and one recent statement: 1 statement goes, connection stays.
		connect("mixed", old), disconnect("mixed", old), stmt("mixed", old, "SELECT 3"), stmt("mixed", recent, "SELECT 4"),
		// Old session with an undated statement: the statement cannot be dated, so the connection stays.
		connect("undated", old), disconnect("undated", old), stmt("undated", "", "SELECT 5"),
		// Old failed login: goes.
		audit.Event{ConnID: "failed", Kind: audit.KindAuthFail, At: old, User: "mallory", AuthResult: audit.AuthFail},
		// Recent: stays.
		connect("recent", recent), stmt("recent", recent, "SELECT 6"),
	)

	stmts, err := s.CountExpiredStatements(testCutoff)
	if err != nil {
		t.Fatal(err)
	}
	conns, err := s.CountExpiredConnections(testCutoff)
	if err != nil {
		t.Fatal(err)
	}
	if stmts != 3 || conns != 2 {
		t.Fatalf("counted %d statements and %d connections, want 3 and 2", stmts, conns)
	}

	// Counting must not have deleted anything.
	if again, _ := s.CountExpiredStatements(testCutoff); again != 3 {
		t.Fatalf("a count changed the data: %d statements expired now", again)
	}

	// Now do what a purge does and check the counts were right.
	rows, _ := s.ExpiredStatements(testCutoff, 100)
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	if n, err := s.DeleteStatements(ids); err != nil || n != stmts {
		t.Fatalf("deleted %d statements (%v), counted %d", n, err, stmts)
	}
	list, _ := s.ExpiredConnections(testCutoff, 100)
	connList := make([]string, len(list))
	for i, c := range list {
		connList[i] = c.ConnID
	}
	if n, err := s.DeleteExpiredConnections(testCutoff, connList); err != nil || n != conns {
		t.Fatalf("deleted %d connections (%v), counted %d", n, err, conns)
	}
}

func TestConnectionWithStatementsDoesNotExpireUntilTheyAreGone(t *testing.T) {
	s := mustStore(t)
	write(t, s, connect("c1", old), disconnect("c1", old), stmt("c1", old, "SELECT 1"))

	got, _ := s.ExpiredConnections(testCutoff, 100)
	if len(got) != 0 {
		t.Fatalf("connection reported expired while it still has a statement: %+v", got)
	}

	rows, _ := s.ExpiredStatements(testCutoff, 100)
	if _, err := s.DeleteStatements([]int64{rows[0].ID}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ExpiredConnections(testCutoff, 100)
	if len(got) != 1 {
		t.Fatalf("connection should expire once its statements are gone, got %+v", got)
	}
}

func TestDeleteExpiredConnectionsKeepsOneThatWokeUp(t *testing.T) {
	s := mustStore(t)
	write(t, s, connect("c1", old), disconnect("c1", old), connect("c2", old), disconnect("c2", old))

	listed, _ := s.ExpiredConnections(testCutoff, 100)
	if len(listed) != 2 {
		t.Fatalf("setup: %d listed", len(listed))
	}

	// Between the read and the delete, c1 gets a late statement and c2 a late reconnect time.
	write(t, s, stmt("c1", recent, "SELECT late"))
	write(t, s, disconnect("c2", recent))

	n, err := s.DeleteExpiredConnections(testCutoff, []string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("deleted %d connections that were no longer expired", n)
	}
	if got := connIDs(t, s); !got["c1"] || !got["c2"] {
		t.Fatalf("connections after delete = %v, want both kept", got)
	}
}

func TestDeleteExpiredConnectionsHandlesMoreThanOneBatch(t *testing.T) {
	s := mustStore(t)
	events := make([]audit.Event, 0, 1200)
	ids := make([]string, 0, 1200)
	for i := 0; i < 1200; i++ {
		id := fmt.Sprintf("c%04d", i)
		ids = append(ids, id)
		events = append(events, audit.Event{ConnID: id, Kind: audit.KindAuthFail, At: old, User: "u", AuthResult: audit.AuthFail})
	}
	write(t, s, events...)

	n, err := s.DeleteExpiredConnections(testCutoff, ids)
	if err != nil || n != 1200 {
		t.Fatalf("DeleteExpiredConnections = %d, %v; want 1200, nil", n, err)
	}
}

// Without the indexes a purge scans every row. This checks they exist, and that
// the expiry queries use them, for instance failing if a query and the index
// expression drift apart.
func TestRetentionQueriesUseTheirIndexes(t *testing.T) {
	s := mustStore(t)

	for _, name := range []string{"idx_statements_ts", "idx_connections_last"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("index %s does not exist: InitDB must create it", name)
		}
	}

	plan := func(query string, args ...any) string {
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			b.WriteString(detail + "\n")
		}
		return b.String()
	}

	got := plan(expiredStatementsSQL, "x", 10)
	if !strings.Contains(got, "idx_statements_ts") {
		t.Errorf("statement expiry query does not use idx_statements_ts:\n%s", got)
	}

	got = plan(expiredConnectionsSQL, "x", 10)
	if !strings.Contains(got, "idx_connections_last") {
		t.Errorf("connection expiry query does not use idx_connections_last:\n%s", got)
	}

	got = plan(countExpiredStatementsSQL, "x")
	if !strings.Contains(got, "idx_statements_ts") {
		t.Errorf("statement count query does not use idx_statements_ts:\n%s", got)
	}

	got = plan(countExpiredConnectionsSQL, "x", "x")
	if !strings.Contains(got, "idx_connections_last") {
		t.Errorf("connection count query does not use idx_connections_last:\n%s", got)
	}
}
