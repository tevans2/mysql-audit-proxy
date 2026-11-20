package e2e

import "testing"

// Drivers and tools tag statements with a leading comment. The audit log must
// still type them by their keyword, and keep the SQL exactly as sent.
func TestCommentedStatementIsTypedByItsKeyword(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	db := s.openThroughProxy(t, a.name, aliceUser, alicePass(t), "")
	const sql = "/* ApplicationName=e2e-tagged */ SELECT name FROM auditproxy_test.upstream_marker"
	if _, err := db.Exec(sql); err != nil {
		t.Fatal(err)
	}

	st := s.waitForStatement(t, "ApplicationName=e2e-tagged")
	if st.StmtType != "SELECT" {
		t.Errorf("stmt_type = %q, want SELECT", st.StmtType)
	}
	if st.SQL != sql {
		t.Errorf("stored SQL = %q, want it exactly as sent: %q", st.SQL, sql)
	}
}
