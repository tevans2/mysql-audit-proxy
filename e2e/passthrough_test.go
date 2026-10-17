package e2e

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"lustre/internal/audit"

	"github.com/go-sql-driver/mysql"
)

// Pass-through authentication (ADR 0004), end to end: a real client, the built
// proxy, the real database, the API and store read back over HTTP.
//
// The proxy holds one account per upstream (proxy_bootstrap, USAGE only) and
// nothing else. Every client logs in as itself, and the database decides.

// mysqlErrNumber returns the MySQL error number inside err, or 0.
func mysqlErrNumber(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// TestPassThroughLoginIsTheRealUser: what the database says about the session
// is what the client logged in as — not the proxy's bootstrap account.
func TestPassThroughLoginIsTheRealUser(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	for _, c := range []struct {
		u          upstream
		user, pass string
	}{
		{a, aliceUser, alicePass(t)},
		{b, bobUser, bobPass(t)},
	} {
		db := s.openThroughProxy(t, c.u.name, c.user, c.pass, "")

		var who string
		if err := db.QueryRow("SELECT USER()").Scan(&who); err != nil {
			t.Fatalf("%s: SELECT USER(): %v", c.u.name, err)
		}
		if !strings.HasPrefix(who, c.user+"@") {
			t.Errorf("%s: SELECT USER() = %q, want it to start with %q", c.u.name, who, c.user+"@")
		}

		var current string
		if err := db.QueryRow("SELECT CURRENT_USER()").Scan(&current); err != nil {
			t.Fatalf("%s: SELECT CURRENT_USER(): %v", c.u.name, err)
		}
		if want := c.user + "@%"; current != want {
			t.Errorf("%s: CURRENT_USER() = %q, want %q (the bootstrap account must not be what the database sees)", c.u.name, current, want)
		}

		conn := s.waitForConnection(t, c.user)
		if conn.Upstream != c.u.name || conn.AuthResult != "ok" {
			t.Errorf("%s: audited connection = upstream %q, auth_result %q; want %q and %q",
				c.user, conn.Upstream, conn.AuthResult, c.u.name, "ok")
		}
	}
}

// TestPassThroughUngrantedTableIsRefusedByTheDatabaseAndAudited: the proxy
// decides nothing about privileges. A table the user has no grant on is refused
// by the database with 1142, and the refusal is in the audit trail.
func TestPassThroughUngrantedTableIsRefusedByTheDatabaseAndAudited(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	db := s.openThroughProxy(t, a.name, aliceUser, alicePass(t), "")

	token := fmt.Sprintf("e2e-nogrant-%d", time.Now().UnixNano())
	var n int
	err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM mysql.user /* %s */", token)).Scan(&n)
	if got := mysqlErrNumber(err); got != 1142 {
		t.Fatalf("ungranted table: got %v (error number %d), want 1142 from the database", err, got)
	}

	// The granted table still works on the same session: the refusal is about
	// the table, not a broken connection.
	if err := db.QueryRow("SELECT COUNT(*) FROM " + a.ownTable).Scan(&n); err != nil {
		t.Errorf("granted table after a refusal: %v", err)
	}

	st := s.waitForStatement(t, token)
	if st.DBUser != aliceUser {
		t.Errorf("refused statement recorded db_user %q, want %q", st.DBUser, aliceUser)
	}
	if st.Error == nil || !strings.Contains(*st.Error, "1142") {
		got := "<nil>"
		if st.Error != nil {
			got = *st.Error
		}
		t.Errorf("refused statement recorded error %s, want the database's 1142", got)
	}
}

// TestPassThroughWrongPasswordIs1045AndAuditedWithTheDatabasesReason: the
// database's own refusal reaches the client unchanged, and the audit log gets an
// auth_fail event carrying the database's reason.
func TestPassThroughWrongPasswordIs1045AndAuditedWithTheDatabasesReason(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	wrong := fmt.Sprintf("wrong-%d", time.Now().UnixNano())
	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/?timeout=10s&tls=skip-verify", aliceUser, wrong, s.proxyAddrs[a.name]))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = db.Ping()
	if got := mysqlErrNumber(err); got != 1045 {
		t.Fatalf("wrong password: got %v (error number %d), want 1045", err, got)
	}

	conn := s.waitForConnection(t, aliceUser)
	if conn.AuthResult != "fail" {
		t.Errorf("auth_result = %q, want %q", conn.AuthResult, "fail")
	}
	if conn.AuthFailReason == nil || !strings.Contains(*conn.AuthFailReason, "1045") {
		got := "<nil>"
		if conn.AuthFailReason != nil {
			got = *conn.AuthFailReason
		}
		t.Errorf("auth_fail reason = %s, want the database's 1045", got)
	}
	if r := conn.AuthFailReason; r != nil {
		if strings.Contains(*r, wrong) {
			t.Errorf("the auth_fail reason contains the password that was tried")
		}
		if strings.Contains(*r, bootstrapUser) {
			t.Errorf("the auth_fail reason names the bootstrap account: %q", *r)
		}
	}
}

// TestPassThroughNoPasswordIsLogged logs in (rightly and wrongly) and then looks
// for every password involved in the proxy's output and in what the API serves.
func TestPassThroughNoPasswordIsLogged(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	good := alicePass(t)
	wrong := fmt.Sprintf("wrong-%d", time.Now().UnixNano())
	boot := bootstrapPass(t, a)

	db := s.openThroughProxy(t, a.name, aliceUser, good, "")
	if err := db.Ping(); err != nil {
		t.Fatalf("good login: %v", err)
	}
	bad, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/?timeout=10s&tls=skip-verify", aliceUser, wrong, s.proxyAddrs[a.name]))
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if err := bad.Ping(); mysqlErrNumber(err) != 1045 {
		t.Fatalf("wrong login: got %v, want 1045", err)
	}

	// Both outcomes have reached the audit store by now, so anything the proxy
	// was going to log about them has been logged.
	s.waitForConnection(t, aliceUser)
	time.Sleep(500 * time.Millisecond)

	var conns, stmts map[string]any
	s.getJSON(t, "/api/connections", &conns)
	s.getJSON(t, "/api/statements", &stmts)

	for _, where := range []struct{ name, text string }{
		{"the proxy's log", s.proxyLog.String()},
		{"GET /api/connections", fmt.Sprint(conns)},
		{"GET /api/statements", fmt.Sprint(stmts)},
	} {
		for _, secret := range []struct{ what, value string }{
			{"alice's password", good},
			{"the wrong password", wrong},
			{"the bootstrap password", boot},
		} {
			if strings.Contains(where.text, secret.value) {
				t.Errorf("%s contains %s", where.name, secret.what)
			}
		}
	}
}

// TestPassThroughLockedAccountIsRefusedAndAudited: a refusal that is not a wrong
// password — here a locked account — still reaches the client as the
// database's own error and is recorded as a failed login with its reason.
func TestPassThroughLockedAccountIsRefusedAndAudited(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	user := fmt.Sprintf("e2e_locked_%d", time.Now().UnixNano()%1_000_000_000)
	pass := fmt.Sprintf("pw-%d", time.Now().UnixNano())
	root := openDirect(t, a, a.user, a.pass)
	defer root.Close()
	if _, err := root.Exec(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s' ACCOUNT LOCK", user, pass)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r := openDirect(t, a, a.user, a.pass)
		defer r.Close()
		_, _ = r.Exec(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", user))
	})

	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/?timeout=10s&tls=skip-verify", user, pass, s.proxyAddrs[a.name]))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = db.Ping()
	if got := mysqlErrNumber(err); got != 3118 {
		t.Fatalf("locked account: got %v (error number %d), want 3118", err, got)
	}

	conn := s.waitForConnection(t, user)
	if conn.AuthResult != "fail" {
		t.Errorf("auth_result = %q, want %q", conn.AuthResult, "fail")
	}
	if conn.AuthFailReason == nil || !strings.Contains(strings.ToLower(*conn.AuthFailReason), "locked") {
		got := "<nil>"
		if conn.AuthFailReason != nil {
			got = *conn.AuthFailReason
		}
		t.Errorf("auth_fail reason = %s, want the database's \"locked\" reason", got)
	}
}

// TestPassThroughDefaultDatabaseProblemsAreNotFailedLogins: naming a database
// the user has no access to, or one that does not exist, is refused by the
// database — but the credentials were fine, so it is not an auth_fail.
func TestPassThroughDefaultDatabaseProblemsAreNotFailedLogins(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	for _, c := range []struct {
		database string
		want     uint16
	}{
		{"mysql", 1044}, // exists, alice has no grant on it
		// MySQL answers 1044, not 1049, for a database that does not exist when
		// the user has no privileges: it must not reveal which databases exist.
		{"no_such_database", 1044},
	} {
		db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/%s?timeout=10s&tls=skip-verify",
			aliceUser, alicePass(t), s.proxyAddrs[a.name], c.database))
		if err != nil {
			t.Fatal(err)
		}
		err = db.Ping()
		db.Close()
		if got := mysqlErrNumber(err); got != c.want {
			t.Errorf("database %q: got %v (error number %d), want %d from the database", c.database, err, got, c.want)
		}
	}

	// A login with the right database still works afterwards.
	good := s.openThroughProxy(t, a.name, aliceUser, alicePass(t), "sales")
	var n int
	if err := good.QueryRow("SELECT COUNT(*) FROM customers").Scan(&n); err != nil {
		t.Errorf("login with a granted default database: %v", err)
	}

	time.Sleep(time.Second) // let any (wrong) event reach the store before looking
	var resp struct {
		Connections []audit.Connection `json:"connections"`
	}
	s.getJSON(t, "/api/connections", &resp)
	for _, c := range resp.Connections {
		if c.AuthResult == "fail" {
			t.Errorf("a database problem was recorded as a failed login: %+v", c)
		}
	}
}

// TestPassThroughUnknownUserIsIndistinguishableFromWrongPassword: a client must
// not be able to tell, from the proxy's answer, whether a username exists. The
// two refusals are compared whole — error number, SQLSTATE and message — with
// only the name the client typed normalised away.
func TestPassThroughUnknownUserIsIndistinguishableFromWrongPassword(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	wrong := fmt.Sprintf("wrong-%d", time.Now().UnixNano())
	unknown := fmt.Sprintf("e2e_nosuchuser_%d", time.Now().UnixNano()%1_000_000_000)

	refusal := func(user string) *mysql.MySQLError {
		t.Helper()
		err := s.pingAs(t, a.name, user, wrong)
		var me *mysql.MySQLError
		if !errors.As(err, &me) {
			t.Fatalf("%s: got %v, want a MySQL error", user, err)
		}
		return me
	}
	knownErr := refusal(aliceUser)
	unknownErr := refusal(unknown)

	if knownErr.Number != 1045 || unknownErr.Number != 1045 {
		t.Fatalf("error numbers: wrong password %d, unknown user %d, want 1045 for both", knownErr.Number, unknownErr.Number)
	}
	if knownErr.SQLState != unknownErr.SQLState {
		t.Errorf("SQLSTATE differs: wrong password %v, unknown user %v", knownErr.SQLState, unknownErr.SQLState)
	}
	normalise := func(msg, user string) string { return strings.ReplaceAll(msg, "'"+user+"'", "'<user>'") }
	if k, u := normalise(knownErr.Message, aliceUser), normalise(unknownErr.Message, unknown); k != u {
		t.Errorf("messages differ, so the username's existence leaks:\n  wrong password: %q\n  unknown user:   %q", k, u)
	}

	// The audit trail must not tell them apart either.
	for _, user := range []string{aliceUser, unknown} {
		conn := s.waitForConnection(t, user)
		if conn.AuthResult != "fail" {
			t.Errorf("%s: auth_result = %q, want %q", user, conn.AuthResult, "fail")
		}
		if conn.AuthFailReason == nil || !strings.Contains(*conn.AuthFailReason, "1045") {
			t.Errorf("%s: auth_fail reason = %v, want the database's 1045", user, conn.AuthFailReason)
		}
	}
}

// TestPassThroughRevokedGrantTakesEffectOnTheNextSessionWithoutAProxyChange:
// the database is the only authority. Revoking a privilege directly on it
// changes what the same user can do through the proxy, with nothing reloaded
// or restarted, and the refusal is the database's own 1142.
func TestPassThroughRevokedGrantTakesEffectOnTheNextSessionWithoutAProxyChange(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	const query = "SELECT name FROM lustre_test.upstream_marker"
	user, pass := createTempUser(t, a, "e2e_revoke", "GRANT SELECT ON lustre_test.upstream_marker TO %s")

	first := s.openThroughProxy(t, a.name, user, pass, "")
	var name string
	if err := first.QueryRow(query).Scan(&name); err != nil {
		t.Fatalf("before the revoke: %v", err)
	}

	root := openDirect(t, a, a.user, a.pass)
	defer root.Close()
	if _, err := root.Exec(fmt.Sprintf("REVOKE SELECT ON lustre_test.upstream_marker FROM '%s'@'%%'", user)); err != nil {
		t.Fatalf("revoking: %v", err)
	}

	// A new session through the same, unchanged proxy.
	second := s.openThroughProxy(t, a.name, user, pass, "")
	err := second.QueryRow(query).Scan(&name)
	if got := mysqlErrNumber(err); got != 1142 {
		t.Fatalf("after the revoke: got %v (error number %d), want 1142 from the database", err, got)
	}

	deadline := time.Now().Add(auditTimeout)
	for {
		var resp struct {
			Statements []audit.Statement `json:"statements"`
		}
		s.getJSON(t, "/api/statements", &resp)
		for _, got := range resp.Statements {
			if got.DBUser == user && got.Error != nil && strings.Contains(*got.Error, "1142") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refused statement for %s never appeared in the audit log with its 1142", user)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestPassThroughBadBootstrapCredentialIsAGenericErrorAndNotAFailedLogin: when
// the proxy's own account is the problem, the client gets a generic error that
// names neither the account nor its password, and no auth_fail is recorded: the
// client's credentials were never tested, so a failed login would be a false
// security event.
func TestPassThroughBadBootstrapCredentialIsAGenericErrorAndNotAFailedLogin(t *testing.T) {
	a, b := upstreams(t)
	badBoot := fmt.Sprintf("not-the-bootstrap-password-%d", time.Now().UnixNano())
	s := startStackWith(t, stackOpts{
		sinkBatchSize:     1,
		sinkFlushInterval: 200 * time.Millisecond,
		bootstrapPass:     badBoot,
	}, a, b)

	err := s.pingAs(t, a.name, aliceUser, alicePass(t))
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		t.Fatalf("login with a bad bootstrap credential: got %v, want a MySQL error", err)
	}
	if me.Number == 1045 {
		t.Errorf("error number 1045: the client was told its own password was wrong, but it was never tested")
	}
	for _, leak := range []string{bootstrapUser, badBoot, bootstrapPass(t, a)} {
		if strings.Contains(me.Message, leak) {
			t.Errorf("the client error contains %q: %q", leak, me.Message)
		}
	}

	// The operator, not the client, learns which account failed.
	if !strings.Contains(s.proxyLog.String(), bootstrapUser) {
		t.Errorf("the proxy log never names the bootstrap account, so the operator cannot tell what to fix")
	}

	// Let the sink flush, then look for an auth_fail that must not be there.
	time.Sleep(1 * time.Second)
	var resp struct {
		Connections []audit.Connection `json:"connections"`
	}
	s.getJSON(t, "/api/connections", &resp)
	for _, c := range resp.Connections {
		if c.AuthResult == "fail" {
			t.Errorf("auth_fail recorded for %q although the proxy, not the client, was at fault", c.DBUser)
		}
	}
}
