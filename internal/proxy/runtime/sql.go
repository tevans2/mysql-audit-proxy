package runtime

import (
	"strings"
)

// classifyStmtType returns a best-effort statement type — the first keyword
// of the query after any leading comments, uppercased (e.g. "SELECT",
// "INSERT"). Returns "" for an empty or comment-only query.
//
// Drivers and tools tag their statements with a leading comment
// (/* ApplicationName=DBeaver ... */, /* mysql-connector-j ... */). Without
// skipping it, every such statement was typed "/*".
func classifyStmtType(query string) string {
	trimmed := skipLeadingComments(query)
	if trimmed == "" {
		return ""
	}
	end := strings.IndexAny(trimmed, " \t\r\n(")
	if end == -1 {
		return strings.ToUpper(trimmed)
	}
	return strings.ToUpper(trimmed[:end])
}

// skipLeadingComments returns query without the whitespace and comments that
// precede its first token: /* ... */, "-- " to end of line, and # to end of
// line. It is for classifying only; the text that is stored is never altered.
//
// An unterminated /* swallows the rest of the query, as MySQL would, so the
// result is "". A /*! ... */ or /*+ ... */ comment is left in place: MySQL
// executes the first as SQL, and the second is an optimizer hint, so neither
// is just a note.
func skipLeadingComments(query string) string {
	s := query
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "/*!"), strings.HasPrefix(s, "/*+"):
			return s
		case strings.HasPrefix(s, "/*"):
			end := strings.Index(s[2:], "*/")
			if end == -1 {
				return ""
			}
			s = s[2+end+2:]
		case strings.HasPrefix(s, "#"):
			s = skipLine(s)
		case strings.HasPrefix(s, "--") && (len(s) == 2 || strings.ContainsRune(" \t\r\n", rune(s[2]))):
			s = skipLine(s)
		default:
			return s
		}
	}
}

// skipLine returns s after the end of its first line, or "" if it has none.
func skipLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i != -1 {
		return s[i+1:]
	}
	return ""
}

// extractUseDB reports whether query is a "USE <db>" statement and, if so,
// returns the database name. MySQL clients typically send USE as literal SQL
// text over COM_QUERY (reaching HandleQuery), not as the COM_INIT_DB protocol
// command (which would reach UseDB instead) — so HandleQuery has to watch for
// this itself to keep currentDB accurate.
func extractUseDB(query string) (string, bool) {
	trimmed := strings.TrimSpace(query)
	const prefix = "use "
	if len(trimmed) <= len(prefix) || !strings.EqualFold(trimmed[:len(prefix)], prefix) {
		return "", false
	}
	dbName := strings.TrimSpace(trimmed[len(prefix):])
	dbName = strings.TrimSuffix(dbName, ";")
	dbName = strings.Trim(dbName, "`")
	return dbName, dbName != ""
}
