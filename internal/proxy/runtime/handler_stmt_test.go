package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	sqldriver "github.com/go-sql-driver/mysql"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
)

// Prepared statements — HandleStmtPrepare/Execute/Close in handler_stmt.go. The
// point of the ticket: a client using server-side prepared statements can
// connect and query through the proxy, and the audit log shows the real SQL,
// not "executed statement <id>".
// ---------------------------------------------------------------------------

// TestVerify_PreparedStatementExecutesAndAudits verifies a server-side
// prepared statement works end to end through the proxy (prepare, bind
// params, execute, get the right answer back) and that the audit trail
// records the prepared SQL text, not a placeholder.
//
// go-sql-driver/mysql uses real server-side prepared statements (COM_STMT_
// PREPARE/EXECUTE) for db.Prepare by default — the DSN here doesn't set
// interpolateParams, which is what would turn this into plain text queries
// instead and defeat the point of the test.
func TestVerify_PreparedStatementExecutesAndAudits(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

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

	const query = "SELECT ? + ?"
	stmt, err := clientConn.PrepareContext(ctx, query)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer stmt.Close()

	var sum int
	if err := stmt.QueryRowContext(ctx, 2, 3).Scan(&sum); err != nil {
		t.Fatalf("QueryRow(2, 3): %v", err)
	}
	if sum != 5 {
		t.Errorf("2 + 3 through a prepared statement = %d, want 5", sum)
	}

	if !sink.waitForEvent(audit.KindStatement, 5*time.Second) {
		t.Fatal("timed out waiting for statement event")
	}

	for _, e := range sink.getEvents() {
		if e.Kind != audit.KindStatement {
			continue
		}
		if e.SQL != query {
			t.Errorf("statement event SQL = %q, want %q (the prepared text, not a resolved query or a statement id)", e.SQL, query)
		}
		if e.StmtType != "SELECT" {
			t.Errorf("statement event StmtType = %q, want %q", e.StmtType, "SELECT")
		}
		return
	}
	t.Fatal("no statement event found after waitForEvent reported one")
}

// TestVerify_PreparedStatementReusedAcrossExecutions verifies a statement
// prepared once and executed several times with different arguments audits
// the same prepared SQL text every time — proving the event records the
// statement as prepared, not whatever a single execution's arguments happen
// to look like.
func TestVerify_PreparedStatementReusedAcrossExecutions(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

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

	const query = "SELECT ? + ?"
	stmt, err := clientConn.PrepareContext(ctx, query)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer stmt.Close()

	want := map[int]bool{7: true, 30: true}
	for _, args := range [][2]int{{3, 4}, {10, 20}} {
		var sum int
		if err := stmt.QueryRowContext(ctx, args[0], args[1]).Scan(&sum); err != nil {
			t.Fatalf("QueryRow(%d, %d): %v", args[0], args[1], err)
		}
		if !want[sum] {
			t.Errorf("QueryRow(%d, %d) = %d, want one of 7 or 30", args[0], args[1], sum)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	var statementEvents int
	for time.Now().Before(deadline) && statementEvents < 2 {
		statementEvents = 0
		for _, e := range sink.getEvents() {
			if e.Kind != audit.KindStatement {
				continue
			}
			statementEvents++
			if e.SQL != query {
				t.Errorf("statement event SQL = %q, want %q", e.SQL, query)
			}
		}
		if statementEvents < 2 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if statementEvents < 2 {
		t.Fatalf("got %d statement events for 2 executions, want 2", statementEvents)
	}
}

// TestVerify_PreparedStatementCloseFreesUpstream verifies closing a
// prepared statement doesn't break the session it belongs to — the upstream
// *client.Stmt behind it is torn down, but the shared upstream connection
// (and any later plain query on it) keeps working.
func TestVerify_PreparedStatementCloseFreesUpstream(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

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

	stmt, err := clientConn.PrepareContext(ctx, "SELECT ? + ?")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var sum int
	if err := stmt.QueryRowContext(ctx, 1, 1).Scan(&sum); err != nil {
		t.Fatalf("QueryRow(1, 1): %v", err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The session (and its shared upstream connection) must still work after
	// closing one prepared statement on it.
	var one int
	if err := clientConn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("plain query after closing a prepared statement failed: %v", err)
	}
	if one != 1 {
		t.Errorf("SELECT 1 after closing a prepared statement = %d, want 1", one)
	}
}

// TestVerify_PreparedStatementFailureIsAudited verifies a prepared statement
// that fails at EXECUTE, not PREPARE — the statement is valid, only this
// particular argument isn't — still produces a statement event carrying the
// error, the same way a failing plain query does in HandleQuery. Silently
// dropping a failed prepared-statement execute from the audit trail would be
// worse than the "executed statement 3" placeholder this ticket exists to
// fix: it would be no record at all.
func TestVerify_PreparedStatementFailureIsAudited(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)
	admin := upstreamAdmin(t, upstreamAddr, upstreamUser, upstreamPass)

	if _, err := admin.Exec("DROP TABLE IF EXISTS verify_stmt_fail"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := admin.Exec("CREATE TABLE verify_stmt_fail (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { admin.Exec("DROP TABLE IF EXISTS verify_stmt_fail") })

	db, err := sql.Open("mysql", proxyDSN(upstreamUser, upstreamPass, proxyAddr))
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

	const query = "INSERT INTO verify_stmt_fail (id) VALUES (?)"
	stmt, err := clientConn.PrepareContext(ctx, query)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer stmt.Close()

	if _, err := stmt.ExecContext(ctx, 1); err != nil {
		t.Fatalf("first insert (id=1): %v", err)
	}

	t.Log("Step 2: Re-executing with the same primary key — must fail at EXECUTE")
	if _, err := stmt.ExecContext(ctx, 1); err == nil {
		t.Fatal("inserting a duplicate primary key unexpectedly succeeded")
	}

	t.Log("Step 3: Waiting for a statement event carrying the error")
	deadline := time.Now().Add(5 * time.Second)
	var failed bool
	for time.Now().Before(deadline) {
		for _, e := range sink.getEvents() {
			if e.Kind == audit.KindStatement && e.SQL == query && e.Err != "" {
				failed = true
				break
			}
		}
		if failed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !failed {
		t.Fatal("no statement event with a non-empty Err for the failed prepared execute")
	}
}

// TestVerify_PreparedStatementUpstreamLossClosesSession verifies a lost
// upstream connection is caught for a prepared-statement execute the same
// way it already is for a plain query (TestVerify_UpstreamLossClosesClientSession):
// HandleStmtExecute has to run its failure through classifyErr too, or a
// dead upstream behind a prepared statement would leave the client session
// silently open with every execute failing forever instead of being torn
// down.
func TestVerify_PreparedStatementUpstreamLossClosesSession(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)
	admin := upstreamAdmin(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN(upstreamUser, upstreamPass, proxyAddr))
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

	var upstreamID uint64
	if err := clientConn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&upstreamID); err != nil {
		t.Fatalf("SELECT CONNECTION_ID(): %v", err)
	}
	if !sink.waitForEvent(audit.KindConnect, 5*time.Second) {
		t.Fatal("timed out waiting for connect event")
	}

	stmt, err := clientConn.PrepareContext(ctx, "SELECT ? + ?")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer stmt.Close()

	t.Logf("Killing upstream connection ID %d via admin", upstreamID)
	if _, err := admin.Exec(fmt.Sprintf("KILL %d", upstreamID)); err != nil {
		t.Logf("KILL %d returned: %v (continuing)", upstreamID, err)
	}
	time.Sleep(300 * time.Millisecond)

	t.Log("Step: Executing the prepared statement on the killed session")
	if err := stmt.QueryRowContext(ctx, 1, 1).Scan(new(int)); err == nil {
		t.Fatal("prepared statement execute after the upstream kill unexpectedly succeeded")
	}

	// The client closed nothing, so a disconnect can only come from the proxy.
	if !sink.waitForEvent(audit.KindDisconnect, 5*time.Second) {
		t.Error("no disconnect event after the upstream was lost during a prepared statement execute")
	}
}

// TestVerify_FailedPrepareIsAudited verifies a COM_STMT_PREPARE that upstream
// rejects — here, an unknown table — still produces a statement event
// carrying the attempted SQL and the error. A rejected PREPARE never reaches
// EXECUTE (go-mysql doesn't hand back a statement ID for it), so without this
// the query text alone would leave no audit trail — someone could probe for
// table or column names via prepare and never show up in the log, unlike the
// same probe run as a plain query, which HandleQuery always audits.
func TestVerify_FailedPrepareIsAudited(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	db, err := sql.Open("mysql", proxyDSN(upstreamUser, upstreamPass, proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	const query = "SELECT secret FROM verify_stmt_prepare_no_such_table WHERE id = ?"

	t.Log("Step 1: Preparing a statement against a table that doesn't exist")
	if _, err := db.PrepareContext(ctx, query); err == nil {
		t.Fatal("preparing a statement against a nonexistent table unexpectedly succeeded")
	}

	t.Log("Step 2: Waiting for a statement event carrying the attempted SQL and the error")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range sink.getEvents() {
			if e.Kind == audit.KindStatement && e.SQL == query && e.Err != "" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no statement event with the attempted SQL and a non-empty Err for the failed prepare")
}

// TestVerify_PreparedStatementErrorCodeBecomesGeneric pins a known,
// currently-unfixable go-mysql v1.16.0 limitation raised in code review: a
// prepared statement that fails at EXECUTE loses its real MySQL error code.
//
// The proxy returns the upstream's own *mysql.MyError from HandleStmtExecute
// unwrapped — same as HandleQuery does for a plain query — but unlike
// HandleQuery's caller, COM_STMT_EXECUTE's dispatch inside go-mysql
// (server/stmt.go, not this package) always wraps whatever HandleStmtExecute
// returns with errors.Trace before its own writeError sees it, and writeError
// checks the error's type with a plain assertion rather than errors.As, so
// the wrap defeats it. See the comment on HandleStmtExecute in handler_stmt.go for
// the full explanation and why it cannot be fixed from this handler.
//
// This test intentionally asserts the *broken* behaviour (code 1105, not the
// real 1062) rather than skipping the scenario: if a future go-mysql upgrade
// fixes writeError to use errors.As, this test starts failing, which is
// exactly the signal to go remove this limitation from README.md and this
// comment. Contrast with TestVerify_PreparedStatementFailureIsAudited, which
// covers the same duplicate-key scenario but only checks that *some* error
// is audited — it doesn't care about the code, so it stays green either way.
func TestVerify_PreparedStatementErrorCodeBecomesGeneric(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)
	admin := upstreamAdmin(t, upstreamAddr, upstreamUser, upstreamPass)

	if _, err := admin.Exec("DROP TABLE IF EXISTS verify_stmt_error_code"); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := admin.Exec("CREATE TABLE verify_stmt_error_code (id INT PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { admin.Exec("DROP TABLE IF EXISTS verify_stmt_error_code") })

	db, err := sql.Open("mysql", proxyDSN(upstreamUser, upstreamPass, proxyAddr))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	t.Log("Step 1: Triggering the same duplicate-key failure via a plain query — real code (1062) preserved")
	if _, err := db.ExecContext(ctx, "INSERT INTO verify_stmt_error_code (id) VALUES (1)"); err != nil {
		t.Fatalf("first insert (id=1): %v", err)
	}
	_, plainErr := db.ExecContext(ctx, "INSERT INTO verify_stmt_error_code (id) VALUES (1)")
	if plainErr == nil {
		t.Fatal("duplicate insert via plain query unexpectedly succeeded")
	}
	var plainMySQLErr *sqldriver.MySQLError
	if !errors.As(plainErr, &plainMySQLErr) {
		t.Fatalf("plain query error is not a *mysql.MySQLError: %v", plainErr)
	}
	if plainMySQLErr.Number != 1062 {
		t.Fatalf("plain query duplicate-key error code = %d, want 1062 (ER_DUP_ENTRY) — "+
			"if this changed, the upstream MySQL version's error codes moved, not this proxy",
			plainMySQLErr.Number)
	}

	t.Log("Step 2: Triggering the identical failure via a prepared statement — real code lost to go-mysql")
	stmt, err := db.PrepareContext(ctx, "INSERT INTO verify_stmt_error_code (id) VALUES (?)")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer stmt.Close()

	_, preparedErr := stmt.ExecContext(ctx, 1)
	if preparedErr == nil {
		t.Fatal("duplicate insert via prepared statement unexpectedly succeeded")
	}
	var preparedMySQLErr *sqldriver.MySQLError
	if !errors.As(preparedErr, &preparedMySQLErr) {
		t.Fatalf("prepared statement error is not a *mysql.MySQLError: %v", preparedErr)
	}
	if preparedMySQLErr.Number != 1105 {
		t.Fatalf("prepared statement duplicate-key error code = %d, want the known-limitation "+
			"value 1105 (ER_UNKNOWN_ERROR) — if go-mysql now preserves 1062 here, the limitation "+
			"documented on HandleStmtExecute and in README.md is fixed and should be removed",
			preparedMySQLErr.Number)
	}
}
