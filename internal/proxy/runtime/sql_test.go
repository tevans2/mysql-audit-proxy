package runtime

import "testing"

func TestClassifyStmtType(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"plain select", "SELECT 1", "SELECT"},
		{"lowercase", "select * from t", "SELECT"},
		{"leading whitespace", "  \n\tUPDATE t SET a=1", "UPDATE"},
		{"keyword then paren", "INSERT(a) VALUES (1)", "INSERT"},
		{"empty", "", ""},
		{"whitespace only", " \t\n", ""},

		// What DBeaver and Connector/J actually send.
		{"DBeaver tag", "/* ApplicationName=DBeaver 26.2.0 - SQLEditor <demo-2.sql> */ SELECT id, name, email FROM sales.customers LIMIT 0, 200", "SELECT"},
		{"DBeaver tag on SET", "/* ApplicationName=DBeaver 26.2.0 - Main */ SET autocommit=1", "SET"},
		{"Connector/J tag, no space", "/* mysql-connector-j-8.2.0 (Revision: 06a1f724497fd81c6a659131fda822c9e5085b6c) */SELECT @@session.auto_increment_increment AS auto_increment_increment", "SELECT"},
		{"DBeaver metadata SHOW", "/* ApplicationName=DBeaver 26.2.0 - Metadata */ show databases", "SHOW"},

		{"several comments", "/* a */ /* b */\n-- c\n# d\nDELETE FROM t", "DELETE"},
		{"line comment", "-- note\nSELECT 1", "SELECT"},
		{"hash comment", "# note\nSELECT 1", "SELECT"},
		{"dash dash then end", "--", ""},
		{"dash dash needs a space", "--1 SELECT", "--1"},
		{"minus is not a comment", "-1", "-1"},
		{"comment-only", "/* only a comment */", ""},
		{"unterminated block comment", "/* never closed SELECT 1", ""},
		{"line comment without newline", "-- just a note", ""},
		{"comment inside is not leading", "SELECT /* x */ 1", "SELECT"},

		// Kept as written: MySQL runs /*! ... */, and /*+ ... */ is a hint.
		{"executable comment", "/*!40101 SET NAMES utf8 */", "/*!40101"},
		{"hint comment", "/*+ SET_VAR(sort_buffer_size=16M) */ SELECT 1", "/*+"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStmtType(tc.query); got != tc.want {
				t.Errorf("classifyStmtType(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestSkipLeadingCommentsLeavesTheRestAlone(t *testing.T) {
	const q = "/* tag */ SELECT 'a /* not a comment */ b' -- trailing"
	if got, want := skipLeadingComments(q), "SELECT 'a /* not a comment */ b' -- trailing"; got != want {
		t.Errorf("skipLeadingComments(%q) = %q, want %q", q, got, want)
	}
}
