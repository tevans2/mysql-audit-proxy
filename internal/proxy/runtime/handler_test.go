package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"lustre/internal/audit"
)

// TestProxyQueryRelay verifies that a SELECT through the proxy returns the
// correct result from the upstream server.
func TestProxyQueryRelay(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Connecting through proxy")
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	t.Log("Step 2: Executing SELECT 1")
	var result int
	if err := db.QueryRow("SELECT 1").Scan(&result); err != nil {
		t.Fatalf("SELECT 1 through proxy: %v", err)
	}
	if result != 1 {
		t.Errorf("SELECT 1 = %d, want 1", result)
	}
}

// TestProxyStatementEvent verifies that a query through the proxy emits a
// statement event with the correct conn_id, SQL text, and statement type.
func TestProxyStatementEvent(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Connecting through proxy")
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("db.Ping through proxy: %v", err)
	}
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}

	t.Log("Step 2: Executing SELECT 1")
	var result int
	if err := db.QueryRow("SELECT 1").Scan(&result); err != nil {
		t.Fatalf("SELECT 1 through proxy: %v", err)
	}

	t.Log("Step 3: Waiting for statement event")
	if !sink.waitForEvent(audit.KindStatement, 5*time.Second) {
		t.Fatal("timed out waiting for statement event")
	}

	events := sink.getEvents()
	var connectEvent, stmtEvent *audit.Event
	for i := range events {
		switch events[i].Kind {
		case audit.KindConnect:
			connectEvent = &events[i]
		case audit.KindStatement:
			stmtEvent = &events[i]
		}
	}
	if connectEvent == nil {
		t.Fatal("no connect event found")
	}
	if stmtEvent == nil {
		t.Fatal("no statement event found")
	}

	if stmtEvent.ConnID != connectEvent.ConnID {
		t.Errorf("statement event conn_id = %q, want %q (should match the session's connect event)",
			stmtEvent.ConnID, connectEvent.ConnID)
	}
	if stmtEvent.SQL != "SELECT 1" {
		t.Errorf("statement event sql = %q, want %q", stmtEvent.SQL, "SELECT 1")
	}
	if stmtEvent.StmtType != "SELECT" {
		t.Errorf("statement event stmt_type = %q, want %q", stmtEvent.StmtType, "SELECT")
	}
}

// TestProxyStatementTypeClassification runs a few different statement kinds
// through the proxy and checks each gets the correct best-effort stmt_type.
func TestProxyStatementTypeClassification(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/demo?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("db.Ping through proxy: %v", err)
	}

	t.Log("Step 1: Executing DDL (DROP/CREATE TABLE)")
	if _, err := db.Exec("DROP TABLE IF EXISTS stmt_type_check"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE stmt_type_check (id INT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { db.Exec("DROP TABLE IF EXISTS stmt_type_check") })

	t.Log("Step 2: Executing DML (INSERT/UPDATE)")
	if _, err := db.Exec("INSERT INTO stmt_type_check (id) VALUES (1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec("UPDATE stmt_type_check SET id = 2"); err != nil {
		t.Fatalf("update: %v", err)
	}

	t.Log("Step 3: Waiting for statement events to classify")
	// Wait for at least 4 statement events (CREATE, INSERT, UPDATE — DROP
	// TABLE IF EXISTS may or may not fire depending on whether it existed).
	deadline := time.Now().Add(5 * time.Second)
	var stmtEvents []audit.Event
	for time.Now().Before(deadline) {
		stmtEvents = nil
		for _, e := range sink.getEvents() {
			if e.Kind == audit.KindStatement {
				stmtEvents = append(stmtEvents, e)
			}
		}
		if len(stmtEvents) >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	got := map[string]bool{}
	for _, e := range stmtEvents {
		got[e.StmtType] = true
	}

	for _, want := range []string{"CREATE", "INSERT", "UPDATE"} {
		if !got[want] {
			t.Errorf("no statement event with stmt_type=%q found among: %+v", want, stmtEvents)
		}
	}
}

// TestProxyStatementDB verifies that a statement event's db field reflects
// the database selected via USE, populated from proxyHandler.currentDB.
func TestProxyStatementDB(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	// Connect with no default database, then switch via USE — this exercises
	// proxyHandler.UseDB rather than relying on the DSN's database segment.
	t.Log("Step 1: Connecting with no default DB")
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	clientConn, err := db.Conn(ctx) // pin ONE proxy session for the whole test
	if err != nil {
		t.Fatalf("pin client connection: %v", err)
	}
	defer clientConn.Close()

	t.Log("Step 2: Switching DB via USE demo")
	if _, err := clientConn.ExecContext(ctx, "USE demo"); err != nil {
		t.Fatalf("USE demo: %v", err)
	}
	if _, err := clientConn.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}

	t.Log("Step 3: Waiting for statement event")
	if !sink.waitForEvent(audit.KindStatement, 5*time.Second) {
		t.Fatal("timed out waiting for statement event")
	}

	var found bool
	for _, e := range sink.getEvents() {
		if e.Kind == audit.KindStatement && e.SQL == "SELECT 1" {
			found = true
			if e.DB != "demo" {
				t.Errorf("statement event db = %q, want %q", e.DB, "demo")
			}
		}
	}
	if !found {
		t.Fatal("no statement event for SELECT 1 found")
	}
}

func TestProxyFailingQuery(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/demo?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	clientConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin client connection: %v", err)
	}
	defer clientConn.Close()

	t.Log("Step 1: Executing failing query")
	_, err = clientConn.ExecContext(ctx, "SELECT * FROM non_existent_table_999")
	if err == nil {
		t.Fatal("expected error for non-existent table, got nil")
	}

	t.Log("Step 2: Waiting for statement event")
	if !sink.waitForEvent(audit.KindStatement, 5*time.Second) {
		t.Fatal("timed out waiting for statement event")
	}

	var found bool
	for _, e := range sink.getEvents() {
		if e.Kind == audit.KindStatement && e.SQL == "SELECT * FROM non_existent_table_999" {
			found = true
			if !strings.Contains(e.Err, "1146") {
				t.Errorf("expected Err to contain 1146, got %q", e.Err)
			}
		}
	}
	if !found {
		t.Fatal("no statement event for failing query found")
	}
}

func TestProxyIdenticalQueries(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&tls=skip-verify", upstreamUser, upstreamPass, proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	clientConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin client connection: %v", err)
	}
	defer clientConn.Close()

	t.Log("Step 1: Executing identical queries")
	query := "SELECT 2"
	for i := 0; i < 2; i++ {
		if _, err := clientConn.ExecContext(ctx, query); err != nil {
			t.Fatalf("query %d failed: %v", i, err)
		}
	}

	t.Log("Step 2: Waiting for multiple statement events")
	deadline := time.Now().Add(5 * time.Second)
	var count int
	for time.Now().Before(deadline) {
		count = 0
		for _, e := range sink.getEvents() {
			if e.Kind == audit.KindStatement && e.SQL == query {
				count++
			}
		}
		if count >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if count < 2 {
		t.Errorf("expected 2 events for identical queries, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// 2. Writes report zero affected rows / zero insert id.
// ---------------------------------------------------------------------------

// TestVerify_WriteReportsAffectedRows inserts through the proxy and checks the
// reported counts against what the upstream actually holds.
func TestVerify_WriteReportsAffectedRows(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)
	admin := upstreamAdmin(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN(upstreamUser, upstreamPass, proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if _, err := admin.Exec("DROP TABLE IF EXISTS verify_writes"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := admin.Exec("CREATE TABLE verify_writes (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(16))"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { admin.Exec("DROP TABLE IF EXISTS verify_writes") })

	t.Log("Step 1: Executing INSERT through proxy")
	res, err := db.Exec("INSERT INTO verify_writes (v) VALUES ('a')")
	if err != nil {
		t.Fatalf("INSERT through proxy: %v", err)
	}
	affected, _ := res.RowsAffected()
	insertID, _ := res.LastInsertId()

	t.Log("Step 2: Verifying row count via direct admin connection")
	var upstreamRows int
	if err := admin.QueryRow("SELECT COUNT(*) FROM verify_writes").Scan(&upstreamRows); err != nil {
		t.Fatalf("count upstream rows: %v", err)
	}

	t.Logf("proxy reported affected=%d insertId=%d; upstream actually holds %d row(s)",
		affected, insertID, upstreamRows)

	if upstreamRows != 1 {
		t.Fatalf("upstream row count = %d, want 1 (the relay itself is broken)", upstreamRows)
	}
	if affected != 1 {
		t.Errorf("RowsAffected() = %d, want 1", affected)
	}
	if insertID == 0 {
		t.Errorf("LastInsertId() = 0, want non-zero")
	}
}

// ---------------------------------------------------------------------------
// 3. A result set truncated mid-stream is returned as a success.
// ---------------------------------------------------------------------------

// TestVerify_TruncatedResultSetIsAnError kills the upstream mid-SELECT. The
// client should see an error, not a short but "successful" result set.
//
// Timing-sensitive: if it reports all 20 rows the KILL landed too late.
func TestVerify_TruncatedResultSetIsAnError(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)
	admin := upstreamAdmin(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN(upstreamUser, upstreamPass, proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	clientConn, err := db.Conn(ctx) // pin ONE proxy session for the whole test
	if err != nil {
		t.Fatalf("pin client connection: %v", err)
	}
	defer clientConn.Close()

	t.Log("Step 1: Executing SELECT CONNECTION_ID() through proxy")
	var upstreamID uint64
	if err := clientConn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&upstreamID); err != nil {
		t.Fatalf("SELECT CONNECTION_ID(): %v", err)
	}

	// Kill the upstream connection while the result set is still streaming.
	go func() {
		time.Sleep(1 * time.Second)
		t.Logf("Step 3 (Background): Killing upstream connection ID %d", upstreamID)
		admin.Exec(fmt.Sprintf("KILL %d", upstreamID))
	}()

	t.Log("Step 2: Executing streaming SELECT through proxy")
	const want = 20
	rows, err := clientConn.QueryContext(ctx,
		fmt.Sprintf("SELECT SLEEP(0.2) AS s FROM information_schema.columns LIMIT %d", want))
	if err != nil {
		t.Logf("query returned an error, which is the correct behaviour: %v", err)
		return
	}
	got := 0
	for rows.Next() {
		got++
	}
	iterErr := rows.Err()
	rows.Close()

	t.Logf("client received %d of %d rows; client-side error: %v", got, want, iterErr)

	if got < want && iterErr == nil {
		t.Errorf("got %d of %d rows with no error reported — the truncation is invisible to the client", got, want)
	}
}

// ---------------------------------------------------------------------------
// 4. One client session is not pinned to one upstream connection.
// ---------------------------------------------------------------------------

// TestVerify_SessionPinnedToOneUpstreamConn kills the upstream connection, then
// queries again on the same client session. Silently landing on a different
// upstream connection is what makes transactions and session state unsafe.
func TestVerify_SessionPinnedToOneUpstreamConn(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)
	admin := upstreamAdmin(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN(upstreamUser, upstreamPass, proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	clientConn, err := db.Conn(ctx) // one client session for the whole test
	if err != nil {
		t.Fatalf("pin client connection: %v", err)
	}
	defer clientConn.Close()

	t.Log("Step 1: Setting session variable @verify_marker")
	// Session state that only survives on the same upstream connection.
	if _, err := clientConn.ExecContext(ctx, "SET @verify_marker = 42"); err != nil {
		t.Fatalf("SET @verify_marker: %v", err)
	}

	t.Log("Step 2: Getting upstream CONNECTION_ID()")
	var firstID uint64
	if err := clientConn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&firstID); err != nil {
		t.Fatalf("first SELECT CONNECTION_ID(): %v", err)
	}

	t.Logf("Step 3: Killing upstream connection ID %d via admin", firstID)
	if _, err := admin.Exec(fmt.Sprintf("KILL %d", firstID)); err != nil {
		t.Logf("KILL %d returned: %v (continuing)", firstID, err)
	}
	time.Sleep(300 * time.Millisecond)

	t.Log("Step 4: Executing query on supposedly dead session")
	var secondID uint64
	err = clientConn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&secondID)
	if err != nil {
		t.Logf("query after upstream kill failed, which is the correct behaviour: %v", err)
		return
	}

	t.Logf("upstream connection id before kill = %d, after = %d", firstID, secondID)

	var marker sql.NullInt64
	if scanErr := clientConn.QueryRowContext(ctx, "SELECT @verify_marker").Scan(&marker); scanErr == nil {
		t.Logf("@verify_marker after kill: valid=%v value=%d", marker.Valid, marker.Int64)
	}

	if secondID != firstID {
		t.Errorf("client session silently moved from upstream connection %d to %d; "+
			"an open transaction would have been lost with no error", firstID, secondID)
	}
}
