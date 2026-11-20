package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"

	sqlite3 "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

/*
 * memDBSeq names each in-memory database so concurrent or successive callers
 * do not share one. See InitDB.
 */
var memDBSeq atomic.Int64

type Store struct {
	db *sql.DB
}

// ErrLastAdmin is returned when disabling an admin would leave none active.
var ErrLastAdmin = errors.New("cannot disable the last active admin")

// ErrAdminExists is returned when creating an admin whose username is taken.
var ErrAdminExists = errors.New("admin already exists")

// ErrInvalidKind is returned for a statement kind that is not read, write or
// schema.
var ErrInvalidKind = errors.New("invalid statement kind")

// AdminAction is one row of the admin_actions attribution log.
type AdminAction struct {
	Username string `json:"username"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	Ts       string `json:"ts"`
}

/*
 * IsUniqueViolation reports whether err is a SQLite UNIQUE or PRIMARY KEY
 * constraint failure. It inspects the driver's error code instead of matching
 * on message text.
 */
func IsUniqueViolation(err error) bool {
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.ExtendedCode == sqlite3.ErrConstraintUnique ||
			se.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
	}
	return false
}

// Filter holds the query parameters for listing <connections>
type Filter struct {
	User     string
	Upstream string
	Outcome  string
	From     string
	To       string
	Limit    int
	Offset   int

	// Open keeps only live sessions; see openSessionSQL.
	Open bool
}

// StatementFilter holds the query parameters for listing <statements>
type StatementFilter struct {
	ConnID string
	User   string
	Type   string
	Q      string
	From   string
	To     string
	Limit  int
	Offset int

	// HasError keeps only statements the upstream rejected.
	HasError bool

	// Kind is "read", "write" or "schema"; see stmtKinds. Empty means any.
	Kind string

	Upstream string

	// HideHandshake excludes the session-setup statements clients issue on
	// their own — see handshakeSQLPatterns. It belongs here rather than in
	// the viewer because the filter has to be applied before LIMIT: filtering
	// a page the database already truncated hides rows without making room
	// for any real ones, so a burst of connections can fill the whole page
	// with handshake traffic and leave the viewer showing nothing at all.
	HideHandshake bool
}

// handshakeSQLPatterns match the session-setup statements MySQL clients issue
// automatically on connect — the mysql CLI's version probe, JDBC's
// interrogation of server variables, the character-set and autocommit
// negotiation every driver performs. Under load these outnumber the statements
// a person actually ran, which is what makes the statement list unreadable in
// bulk.
//
// Each one must match the whole statement, lowercased and trimmed: they are
// used as LIKE patterns, but none ends in a wildcard, so a statement that merely
// starts with one (a second clause, a trailing query) is not hidden. (`_` is a
// single-character wildcard in LIKE, so the underscores below match slightly
// more than themselves. Harmless here: no real statement differs from these
// only in those positions.)
//
// Matching is deliberately not whitespace-insensitive. A driver emits these as
// byte-identical constants, so irregular spacing means a person typed it, and
// a statement a person typed should stay visible in an audit view.
//
// The criterion for adding one: it must be read-only session or server
// metadata a client sends on its own, never something a person would author.
// Anything a user could plausibly type stays visible. This only hides rows
// from one view and never deletes anything, but an audit tool that quietly
// omits real statements is worse than a noisy one.
var handshakeSQLPatterns = []string{
	// Exact matches only
	"select @@version_comment limit 1",
	"select database()",
	"show collation",

	// Narrowed hand-typed statements
	"set names utf8mb4",
	"set names utf8mb4 collate utf8mb4_unicode_ci",
	"set autocommit=1",
	"set autocommit=0",
	"select @@session.transaction_isolation",
	"select @@session.tx_isolation",
	"show variables like 'max_allowed_packet'",

	// What DBeaver and MySQL Connector/J send on their own when a connection
	// opens. Connector/J's variable probe is matched in full, not by its
	// prefix: a prefix would let anyone hide a statement by starting it with
	// these words.
	"set character_set_results = null",
	"show engines",
	"show charset",
	"show plugins",
	"show variables like 'lower_case_table_names'",
	"select @@global.character_set_server,@@global.collation_server",
	"select * from information_schema.tables t where t.table_schema = 'information_schema' and t.table_name = 'check_constraints'",
	"select  @@session.auto_increment_increment as auto_increment_increment, @@character_set_client as character_set_client, @@character_set_connection as character_set_connection, @@character_set_results as character_set_results, @@character_set_server as character_set_server, @@collation_server as collation_server, @@collation_connection as collation_connection, @@init_connect as init_connect, @@interactive_timeout as interactive_timeout, @@license as license, @@lower_case_table_names as lower_case_table_names, @@max_allowed_packet as max_allowed_packet, @@net_write_timeout as net_write_timeout, @@performance_schema as performance_schema, @@sql_mode as sql_mode, @@system_time_zone as system_time_zone, @@time_zone as time_zone, @@transaction_isolation as transaction_isolation, @@wait_timeout as wait_timeout",
}

// stmtBodySQL is the statement with one leading tag comment removed, trimmed
// and lowercased, for matching against handshakeSQLPatterns. Clients tag their
// statements (/* ApplicationName=DBeaver … */, /* mysql-connector-j … */), and
// the tag is not part of what the statement is.
//
// Only the first comment goes, and never a /*! (MySQL executes it) or /*+
// (optimizer hint) comment. What is left must still equal a pattern in full, so
// removing the tag cannot make a longer statement look like a short one.
// Whitespace trimmed is spaces, tabs and newlines: some drivers end a
// statement with a newline.
const stmtBodySQL = `lower(trim(CASE
		WHEN trim(COALESCE(sql_text, ''), ' ' || char(9, 10, 13)) LIKE '/*%'
			AND trim(sql_text, ' ' || char(9, 10, 13)) NOT LIKE '/*!%'
			AND trim(sql_text, ' ' || char(9, 10, 13)) NOT LIKE '/*+%'
			AND instr(trim(sql_text, ' ' || char(9, 10, 13)), '*/') > 0
		THEN substr(trim(sql_text, ' ' || char(9, 10, 13)), instr(trim(sql_text, ' ' || char(9, 10, 13)), '*/') + 2)
		ELSE COALESCE(sql_text, '')
	END, ' ' || char(9, 10, 13)))`

/*
 * stmtKinds groups the stored stmt_type values into the three buckets the
 * back-office filters on. stmt_type is only the first keyword of the query
 * (see classifyStmtType in the proxy), so SET, BEGIN, COMMIT, USE, WITH and
 * CALL belong to none of them: they are neither a read, a write nor a schema
 * change on their own, and a kind filter must not claim them.
 *
 * web/stmtkind.js groups statements the same way for the badges, and
 * web/stmtkind.test.mjs reads this map to fail if the two drift apart.
 */
var stmtKinds = map[string][]string{
	"read":   {"SELECT", "SHOW", "EXPLAIN", "DESCRIBE", "DESC"},
	"write":  {"INSERT", "UPDATE", "DELETE", "REPLACE", "LOAD"},
	"schema": {"CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME", "GRANT", "REVOKE"},
}

/*
 * openSessionSQL is what "an open session" means. A failed login is not one:
 * it gets its own conn_id and the proxy never emits a disconnect for it, so its
 * ts_disconnect stays NULL for good and would otherwise be counted as open.
 */
const openSessionSQL = "ts_disconnect IS NULL AND COALESCE(auth_result, '') != 'fail'"

// ValidKind reports whether kind is empty or one of the statement kinds.
func ValidKind(kind string) bool {
	if kind == "" {
		return true
	}
	_, ok := stmtKinds[kind]
	return ok
}

// stmtTypeIn returns "stmt_type IN (?, ?, …)" and its arguments for a kind.
func stmtTypeIn(kind string) (string, []interface{}) {
	types := stmtKinds[kind]
	args := make([]interface{}, len(types))
	for i, t := range types {
		args[i] = t
	}
	return "stmt_type IN (?" + strings.Repeat(", ?", len(types)-1) + ")", args
}

/*
 * InitDB establishes a connection to the SQLite database.
 * It configures durability settings (WAL mode, busy timeouts) for concurrent writes,
 * enforces foreign key constraints, and handles shared memory pools for testing.
 * Finally, it executes the SQL schema to ensure the connections and statements
 * tables, as well as their indexes, exist before the app tries to use them.
 */
func InitDB(filepath string) (*Store, error) {
	dsn := filepath
	if filepath == ":memory:" {
		/*
		 * A plain ":memory:" gives every pooled connection its own blank
		 * database, so the pool has to share one. cache=shared does that, but
		 * an unnamed shared database is process-wide — every InitDB(":memory:")
		 * would hand back the *same* data. Naming it per call keeps callers
		 * (tests especially) isolated from each other.
		 */
		dsn = fmt.Sprintf("file:memdb%d?mode=memory&cache=shared", memDBSeq.Add(1))
	}

	// Enforce foreign keys + some durability
	if strings.Contains(dsn, "?") {
		dsn += "&"
	} else {
		dsn += "?"
	}
	dsn += "_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on"

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	// Force tests to use a single connection for shared memory
	if filepath == ":memory:" {
		db.SetMaxOpenConns(1)
	}

	// Schema
	schema := `
	CREATE TABLE IF NOT EXISTS connections (
		conn_id TEXT PRIMARY KEY,
		client_addr TEXT,
		proxy_port INTEGER,
		db_user TEXT,
		upstream TEXT,
		upstream_addr TEXT,
		auth_result TEXT,
		ts_connect TEXT,
		ts_disconnect TEXT,
		auth_fail_reason TEXT
	);
	
	CREATE TABLE IF NOT EXISTS statements (
		id INTEGER PRIMARY KEY AUTOINCREMENT, 
		conn_id TEXT, 
		db_user TEXT,
		upstream TEXT, 
		sql_text TEXT, 
		ts TEXT,
		stmt_type TEXT,
		db TEXT,
		error TEXT,
		FOREIGN KEY(conn_id) REFERENCES connections(conn_id)
	);
	
	CREATE INDEX IF NOT EXISTS idx_statements_conn_id ON statements(conn_id);

	CREATE TABLE IF NOT EXISTS servers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		listen_port INTEGER UNIQUE NOT NULL,
		upstream_host TEXT NOT NULL,
		upstream_port INTEGER NOT NULL,
		upstream_user TEXT NOT NULL,
		upstream_pass TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS config_metadata (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		revision INTEGER NOT NULL,
		applied_revision INTEGER NOT NULL DEFAULT 0,
		applied_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS admins (
		username TEXT PRIMARY KEY,
		pass_hash TEXT NOT NULL,
		created_at TEXT NOT NULL,
		disabled INTEGER NOT NULL DEFAULT 0,
		last_login_at TEXT
	);

	CREATE TABLE IF NOT EXISTS sessions (
		id_hash TEXT PRIMARY KEY,
		username TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		FOREIGN KEY(username) REFERENCES admins(username)
	);

	CREATE TABLE IF NOT EXISTS admin_actions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL,
		ts TEXT NOT NULL
	);
	`
	if _, err = db.Exec(schema); err != nil {
		return nil, err
	}

	// Seed the initial configuration revision
	if _, err = db.Exec("INSERT OR IGNORE INTO config_metadata (id, revision) VALUES (1, 0)"); err != nil {
		return nil, err
	}

	/*
	 * CREATE TABLE IF NOT EXISTS leaves existing databases untouched, so columns
	 * added after a database file was first created need adding explicitly.
	 * SQLite has no "ADD COLUMN IF NOT EXISTS"; a duplicate column is expected
	 * on every run after the first.
	 */
	for _, alter := range []string{
		`ALTER TABLE servers ADD COLUMN tls TEXT NOT NULL DEFAULT 'verify'`,
		`ALTER TABLE servers ADD COLUMN tls_ca_file TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE connections ADD COLUMN upstream_addr TEXT`,
		`ALTER TABLE connections ADD COLUMN auth_fail_reason TEXT`,
		`ALTER TABLE config_metadata ADD COLUMN applied_revision INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE config_metadata ADD COLUMN applied_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE admins ADD COLUMN last_login_at TEXT`,
	} {
		if _, err := db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return nil, err
		}
	}

	/*
	 * De-duplication key for statements. The sink retries batches it did not
	 * get a 2xx for, so the same statement can legitimately arrive more than
	 * once; without this the audit log over-reports how many queries ran.
	 *
	 * A unique index rather than a table constraint, so it also applies to
	 * databases created before this existed. Any duplicates already stored
	 * have to go first, or creating the index fails.
	 */
	if _, err := db.Exec(`DELETE FROM statements WHERE id NOT IN (
		SELECT MIN(id) FROM statements GROUP BY conn_id, ts, sql_text
	)`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_statements_dedupe
		ON statements(conn_id, ts, sql_text)`); err != nil {
		return nil, err
	}

	/*
	 * Retention (internal/retention) finds expired rows by age. Without these
	 * every purge would scan the whole table. idx_connections_last indexes the
	 * exact expression retention.go's lastActivity uses, with the table alias
	 * dropped, because an index cannot mention an alias.
	 */
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_statements_ts ON statements(ts)`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_connections_last
		ON connections(COALESCE(ts_disconnect, ts_connect))`); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

/*
 * Close releases the underlying database handle. Callers should defer it, and
 * servers should call it after their listener has drained so an in-flight
 * WriteBatch is not cut off mid-transaction.
 */
func (s *Store) Close() error {
	return s.db.Close()
}

/*
 * tsFormat is RFC3339 with fixed microsecond precision.
 *
 * Width matters: timestamps are stored as text and compared as text by
 * ORDER BY and the from/to filters, and time.RFC3339Nano trims trailing zeros,
 * which would sort "…00.1Z" after "…00.15Z". Sub-second precision matters
 * because it is part of the statement de-duplication key below.
 */
const tsFormat = "2006-01-02T15:04:05.000000Z07:00"

// normalizeTime ensures all dates are formatted as ISO-8601 UTC
func normalizeTime(tStr string) string {
	if tStr == "" {
		return ""
	}
	// Try parsing standard RFC3339 (Parse accepts fractional seconds too)
	if t, err := time.Parse(time.RFC3339, tStr); err == nil {
		return t.UTC().Format(tsFormat)
	}
	// Fallback for the old test format
	if t, err := time.Parse("2006-01-02", tStr); err == nil {
		return t.UTC().Format(tsFormat)
	}
	return tStr
}

/*
 * WriteBatch takes an array of incoming events and inserts them into the database.
 * It uses an idempotent UPSERT for connections to safely handle retries and out-of-order
 * disconnect/auth_fail events, and inserts statements with the new schema fields.
 */
func (s *Store) WriteBatch(events []audit.Event) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, e := range events {
		atUTC := normalizeTime(e.At)
		var execErr error

		if e.Kind == "connect" || e.Kind == "disconnect" || e.Kind == "auth_fail" {
			var tsConnect, tsDisconnect *string
			if e.Kind == "connect" || e.Kind == "auth_fail" {
				tsConnect = &atUTC
			}
			if e.Kind == "disconnect" {
				tsDisconnect = &atUTC
			}

			// Capture the error from the Upsert
			//
			// auth_fail_reason: only an auth_fail event ever carries e.Err (connect
			// and disconnect never populate audit.Event.Err), so NULLIF(?, '') keeps
			// this column NULL for those instead of overwriting a real reason with
			// an empty string on a later connect/disconnect upsert for the same
			// conn_id — the same COALESCE(NULLIF(...), ...) pattern already used for
			// every other column here that legitimately empty events must not clobber.
			_, execErr = tx.Exec(`INSERT INTO connections (
				conn_id, client_addr, proxy_port, db_user, upstream, upstream_addr, auth_result, ts_connect, ts_disconnect, auth_fail_reason
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(conn_id) DO UPDATE SET
				client_addr = COALESCE(NULLIF(excluded.client_addr, ''), connections.client_addr),
				proxy_port = COALESCE(NULLIF(excluded.proxy_port, 0), connections.proxy_port),
				db_user = COALESCE(NULLIF(excluded.db_user, ''), connections.db_user),
				upstream = COALESCE(NULLIF(excluded.upstream, ''), connections.upstream),
				upstream_addr = COALESCE(NULLIF(excluded.upstream_addr, ''), connections.upstream_addr),
				auth_result = COALESCE(NULLIF(excluded.auth_result, ''), connections.auth_result),
				ts_connect = COALESCE(excluded.ts_connect, connections.ts_connect),
				ts_disconnect = COALESCE(excluded.ts_disconnect, connections.ts_disconnect),
				auth_fail_reason = COALESCE(NULLIF(excluded.auth_fail_reason, ''), connections.auth_fail_reason)`,
				e.ConnID, e.ClientAddr, e.ProxyPort, e.User, e.Upstream, e.UpstreamAddr, e.AuthResult, tsConnect, tsDisconnect, e.Err)

		} else if e.Kind == "statement" {
			/*
			 * Capture the error from the statement insert
			 * DO NOTHING on conflict: a retried batch must not duplicate rows.
			 * See the idx_statements_dedupe index in InitDB.
			 */
			_, execErr = tx.Exec(`INSERT INTO statements (conn_id, db_user, upstream, sql_text, ts, stmt_type, db, error)
				VALUES (?, (SELECT db_user FROM connections WHERE conn_id=?), (SELECT upstream FROM connections WHERE conn_id=?), ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))
				ON CONFLICT(conn_id, ts, sql_text) DO NOTHING`,
				e.ConnID, e.ConnID, e.ConnID, e.SQL, atUTC, e.StmtType, e.DB, e.Err)
		}

		// If any insert/update fails, return the error immediately so the transaction rolls back
		if execErr != nil {
			return execErr
		}
	}
	return tx.Commit()
}

/*
 * GetConnections queries the database using dynamic filters.
 * It selects all connection columns and uses a LEFT JOIN to compute the statement_count.
 * Returns the paginated results and the total unpaginated row count.
 */
func (s *Store) GetConnections(f Filter) ([]audit.Connection, int, error) {
	if f.Limit <= 0 {
		f.Limit = 50 // Default limit per contract
	}
	if f.Limit > 500 {
		f.Limit = 500 // Max limit per contract
	}

	args := []interface{}{}
	where := " WHERE 1=1"

	if f.User != "" {
		where += " AND c.db_user = ?"
		args = append(args, f.User)
	}
	if f.Upstream != "" {
		where += " AND c.upstream = ?"
		args = append(args, f.Upstream)
	}
	if f.Outcome != "" {
		where += " AND c.auth_result = ?"
		args = append(args, f.Outcome)
	}
	if f.From != "" {
		where += " AND c.ts_connect >= ?"
		args = append(args, normalizeTime(f.From))
	}
	if f.To != "" {
		where += " AND c.ts_connect <= ?"
		args = append(args, normalizeTime(f.To))
	}
	if f.Open {
		where += " AND " + openSessionSQL
	}

	// Calculate total BEFORE pagination
	var total int
	countQuery := "SELECT COUNT(DISTINCT c.conn_id) FROM connections c LEFT JOIN statements s ON c.conn_id = s.conn_id" + where
	if err := s.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `
		SELECT
			c.conn_id, c.db_user, c.client_addr, c.proxy_port, c.upstream,
			c.upstream_addr, c.auth_result, c.ts_connect, c.ts_disconnect,
			COUNT(s.id) as statement_count, c.auth_fail_reason
		FROM connections c
		LEFT JOIN statements s ON c.conn_id = s.conn_id` + where +
		` GROUP BY c.conn_id ORDER BY c.ts_connect DESC LIMIT ? OFFSET ?`

	// Appending
	args = append(args, f.Limit, f.Offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	/*
	 * Non-nil so an empty result encodes as [] rather than null: the UI iterates
	 * the field directly, and `for (const c of null)` is a TypeError.
	 */
	conns := []audit.Connection{}
	for rows.Next() {
		var c audit.Connection
		if err := rows.Scan(
			&c.ConnID, &c.DBUser, &c.ClientAddr, &c.ProxyPort, &c.Upstream,
			&c.UpstreamAddr, &c.AuthResult, &c.TsConnect, &c.TsDisconnect, &c.StatementCount,
			&c.AuthFailReason,
		); err != nil {
			return nil, 0, err
		}
		conns = append(conns, c)
	}

	/*
	 * rows.Next returns false both at the end of the result set and on error,
	 * so without this a truncated read looks like a complete one.
	 */
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return conns, total, nil
}

/*
 * GetConnection fetches a single connection and its associated statements.
 */
func (s *Store) GetConnection(connID string) (*audit.Connection, []audit.Statement, error) {
	query := `
		SELECT
			c.conn_id, c.db_user, c.client_addr, c.proxy_port, c.upstream,
			c.upstream_addr, c.auth_result, c.ts_connect, c.ts_disconnect,
			COUNT(s.id) as statement_count, c.auth_fail_reason
		FROM connections c
		LEFT JOIN statements s ON c.conn_id = s.conn_id
		WHERE c.conn_id = ?
		GROUP BY c.conn_id
	`

	var c audit.Connection
	err := s.db.QueryRow(query, connID).Scan(
		&c.ConnID, &c.DBUser, &c.ClientAddr, &c.ProxyPort, &c.Upstream,
		&c.UpstreamAddr, &c.AuthResult, &c.TsConnect, &c.TsDisconnect, &c.StatementCount,
		&c.AuthFailReason,
	)

	if err == sql.ErrNoRows {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}

	// Fetch statements for this connection
	stmts, _, err := s.GetStatements(StatementFilter{ConnID: connID, Limit: 500})
	if err != nil {
		return nil, nil, err
	}

	return &c, stmts, nil
}

/*
 * GetStatements queries the database for statements using dynamic filters and text search.
 */
func (s *Store) GetStatements(f StatementFilter) ([]audit.Statement, int, error) {
	if f.Limit <= 0 {
		f.Limit = 50 // min
	}
	if f.Limit > 500 {
		f.Limit = 500 // max
	}

	args := []interface{}{}
	where := " WHERE 1=1"

	if f.ConnID != "" {
		where += " AND conn_id = ?"
		args = append(args, f.ConnID)
	}
	if f.User != "" {
		where += " AND db_user = ?"
		args = append(args, f.User)
	}
	if f.Type != "" {
		where += " AND stmt_type = ?"
		args = append(args, f.Type)
	}
	if f.HasError {
		where += " AND error IS NOT NULL"
	}
	if f.Upstream != "" {
		where += " AND upstream = ?"
		args = append(args, f.Upstream)
	}
	if f.Kind != "" {
		if !ValidKind(f.Kind) {
			return nil, 0, ErrInvalidKind
		}
		clause, kindArgs := stmtTypeIn(f.Kind)
		where += " AND " + clause
		args = append(args, kindArgs...)
	}
	if f.Q != "" {
		where += " AND sql_text LIKE ? ESCAPE '\\'"
		escapedQ := strings.ReplaceAll(f.Q, "\\", "\\\\")
		escapedQ = strings.ReplaceAll(escapedQ, "%", "\\%")
		escapedQ = strings.ReplaceAll(escapedQ, "_", "\\_")
		args = append(args, "%"+escapedQ+"%")
	}
	if f.From != "" {
		where += " AND ts >= ?"
		args = append(args, normalizeTime(f.From))
	}
	if f.To != "" {
		where += " AND ts <= ?"
		args = append(args, normalizeTime(f.To))
	}
	if f.HideHandshake {
		// COALESCE because sql_text is nullable and `NULL NOT LIKE x` is NULL,
		// which would drop a row with no SQL rather than keep it.
		//
		// This cannot use an index — lower(trim(...)) is not the indexed
		// expression — so it is a scan of whatever the preceding filters left.
		// Fine at the sizes this store is built for; revisit alongside the
		// Postgres decision if it stops being.
		for _, pattern := range handshakeSQLPatterns {
			where += " AND " + stmtBodySQL + " NOT LIKE ?"
			args = append(args, pattern)
		}
	}

	// Calculate total BEFORE pagination — and, because the handshake predicate
	// is part of `where`, the total the viewer displays is the filtered one.
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM statements"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT id, conn_id, ts, db_user, upstream, db, sql_text, stmt_type, error FROM statements` +
		where + ` ORDER BY ts DESC LIMIT ? OFFSET ?`
	args = append(args, f.Limit, f.Offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	// Non-nil for the same reason as conns above.
	stmts := []audit.Statement{}
	for rows.Next() {
		var st audit.Statement
		var dbStr, errStr sql.NullString
		if err := rows.Scan(&st.ID, &st.ConnID, &st.Ts, &st.DBUser, &st.Upstream, &dbStr, &st.SQL, &st.StmtType, &errStr); err != nil {
			return nil, 0, err
		}

		// only assign the pointers if was actually valid
		if dbStr.Valid {
			st.DB = &dbStr.String
		}
		if errStr.Valid {
			st.Error = &errStr.String
		}

		stmts = append(stmts, st)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return stmts, total, nil
}

/*
 * GetServers retrieves all proxy server configurations from the database.
 */
func (s *Store) GetServers() ([]audit.Server, error) {
	query := `SELECT id, name, listen_port, upstream_host, upstream_port, upstream_user, upstream_pass, enabled, tls, tls_ca_file FROM servers ORDER BY listen_port ASC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	servers := []audit.Server{}
	for rows.Next() {
		var srv audit.Server
		if err := rows.Scan(&srv.ID, &srv.Name, &srv.ListenPort, &srv.UpstreamHost, &srv.UpstreamPort, &srv.UpstreamUser, &srv.UpstreamPass, &srv.Enabled, &srv.TLS, &srv.TLSCAFile); err != nil {
			return nil, err
		}
		servers = append(servers, srv)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	return servers, nil
}

// GetRevision fetches the current configuration revision.
func (s *Store) GetRevision() (int, error) {
	var rev int
	err := s.db.QueryRow("SELECT revision FROM config_metadata WHERE id = 1").Scan(&rev)
	return rev, err
}

// GetAppliedRevision fetches the revision the proxy last reported applied, and when.
func (s *Store) GetAppliedRevision() (int, string, error) {
	var rev int
	var at string
	err := s.db.QueryRow("SELECT applied_revision, applied_at FROM config_metadata WHERE id = 1").Scan(&rev, &at)
	return rev, at, err
}

// SetAppliedRevision records what the proxy reports it is running. The revision
// only moves forward, so a restarting proxy reporting 0 doesn't read as nothing applied.
func (s *Store) SetAppliedRevision(rev int) error {
	_, err := s.db.Exec(`
		UPDATE config_metadata
		SET applied_revision = MAX(applied_revision, ?),
		    applied_at = ?
		WHERE id = 1`,
		rev, time.Now().UTC().Format(tsFormat))
	return err
}

// AddServer inserts a new server and increments the revision atomically.
func (s *Store) AddServer(srv audit.Server, encryptedPass string) error {
	mode, modeErr := audit.NormalizeTLS(srv.TLS)
	if modeErr != nil {
		return modeErr
	}
	srv.TLS = mode
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	query := `
		INSERT INTO servers (name, listen_port, upstream_host, upstream_port, upstream_user, upstream_pass, enabled, tls, tls_ca_file)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if _, err := tx.Exec(query, srv.Name, srv.ListenPort, srv.UpstreamHost, srv.UpstreamPort, srv.UpstreamUser, encryptedPass, srv.Enabled, srv.TLS, srv.TLSCAFile); err != nil {
		return err
	}

	if _, err := tx.Exec("UPDATE config_metadata SET revision = revision + 1 WHERE id = 1"); err != nil {
		return err
	}
	return tx.Commit()
}

/*
 * UpdateServer modifies an existing server by ID and increments the revision atomically.
 * If encryptedPass is empty, the existing password is preserved.
 */
func (s *Store) UpdateServer(id int, srv audit.Server, encryptedPass string, tlsUpdates ...TLSUpdate) error {
	tlsMode, caFile, err := serverTLSUpdate(srv, tlsUpdates)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var query string
	var args []interface{}

	if encryptedPass != "" {
		query = `
			UPDATE servers SET name = ?, listen_port = ?, upstream_host = ?, upstream_port = ?, upstream_user = ?, upstream_pass = ?, enabled = ?, tls = COALESCE(?, tls), tls_ca_file = COALESCE(?, tls_ca_file)
			WHERE id = ?
		`
		args = []interface{}{srv.Name, srv.ListenPort, srv.UpstreamHost, srv.UpstreamPort, srv.UpstreamUser, encryptedPass, srv.Enabled, tlsMode, caFile, id}
	} else {
		query = `
			UPDATE servers SET name = ?, listen_port = ?, upstream_host = ?, upstream_port = ?, upstream_user = ?, enabled = ?, tls = COALESCE(?, tls), tls_ca_file = COALESCE(?, tls_ca_file)
			WHERE id = ?
		`
		args = []interface{}{srv.Name, srv.ListenPort, srv.UpstreamHost, srv.UpstreamPort, srv.UpstreamUser, srv.Enabled, tlsMode, caFile, id}
	}

	res, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return sql.ErrNoRows
	}

	if _, err := tx.Exec("UPDATE config_metadata SET revision = revision + 1 WHERE id = 1"); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteServer removes a server by ID and increments the revision atomically.
func (s *Store) DeleteServer(id int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec("DELETE FROM servers WHERE id = ?", id)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return sql.ErrNoRows
	}

	if _, err := tx.Exec("UPDATE config_metadata SET revision = revision + 1 WHERE id = 1"); err != nil {
		return err
	}
	return tx.Commit()
}

/*
 * HasServers reports whether the servers table holds any rows, so a caller can
 * skip preparing a seed it will not be asked for. Advisory only:
 * SeedServersIfEmpty re-checks inside its own transaction, which is where the
 * decision is actually made.
 */
func (s *Store) HasServers() (bool, error) {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM servers").Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

/*
 * SeedServersIfEmpty inserts servers only when the servers table has no rows.
 * The emptiness check, every insert and a single revision bump share one
 * transaction, so a failure part-way leaves the table empty and the next boot
 * retries, rather than leaving a partial table that looks already seeded.
 * Each server's UpstreamPass must already be encrypted. Returns the number of
 * rows inserted (0 if the table was not empty).
 */
func (s *Store) SeedServersIfEmpty(servers []audit.Server) (int, error) {
	if len(servers) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var existing int
	if err := tx.QueryRow("SELECT COUNT(*) FROM servers").Scan(&existing); err != nil {
		return 0, err
	}
	if existing > 0 {
		return 0, nil
	}

	const insert = `
		INSERT INTO servers (name, listen_port, upstream_host, upstream_port, upstream_user, upstream_pass, enabled, tls, tls_ca_file)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for _, srv := range servers {
		mode, err := audit.NormalizeTLS(srv.TLS)
		if err != nil {
			return 0, err
		}
		srv.TLS = mode
		if _, err := tx.Exec(insert, srv.Name, srv.ListenPort, srv.UpstreamHost, srv.UpstreamPort, srv.UpstreamUser, srv.UpstreamPass, srv.Enabled, srv.TLS, srv.TLSCAFile); err != nil {
			return 0, fmt.Errorf("seed server %q: %w", srv.Name, err)
		}
	}
	if _, err := tx.Exec("UPDATE config_metadata SET revision = revision + 1 WHERE id = 1"); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(servers), nil
}

func (s *Store) SeedAdminIfEmpty(defaultUser, defaultHash string) error {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM admins").Scan(&count); err != nil {
		return fmt.Errorf("check admin count: %w", err)
	}
	if count > 0 {
		return nil
	}

	// Validate the seeded hash cost *after* checking if the DB is empty
	cost, err := bcrypt.Cost([]byte(defaultHash))
	if err != nil {
		return fmt.Errorf("invalid bcrypt hash for initial admin: %w", err)
	}
	if cost < bcrypt.MinCost {
		return fmt.Errorf("invalid bcrypt hash for initial admin: cost %d is too low", cost)
	}

	_, err = s.db.Exec(`INSERT INTO admins (username, pass_hash, created_at, disabled) 
		VALUES (?, ?, ?, 0)`,
		defaultUser, defaultHash, time.Now().UTC().Format(time.RFC3339))
	return err
}

/*
 * GetServersAndRevision retrieves all proxy server configurations alongside the current database revision.
 * This runs inside a single transaction to guarantee the revision matches the exact state of the returned servers.
 */
func (s *Store) GetServersAndRevision() ([]audit.Server, int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	var rev int
	if err := tx.QueryRow("SELECT revision FROM config_metadata WHERE id = 1").Scan(&rev); err != nil {
		return nil, 0, err
	}

	query := `SELECT id, name, listen_port, upstream_host, upstream_port, upstream_user, upstream_pass, enabled, tls, tls_ca_file FROM servers ORDER BY listen_port ASC`
	rows, err := tx.Query(query)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	servers := []audit.Server{}
	for rows.Next() {
		var srv audit.Server
		if err := rows.Scan(&srv.ID, &srv.Name, &srv.ListenPort, &srv.UpstreamHost, &srv.UpstreamPort, &srv.UpstreamUser, &srv.UpstreamPass, &srv.Enabled, &srv.TLS, &srv.TLSCAFile); err != nil {
			return nil, 0, err
		}
		servers = append(servers, srv)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Transaction commit is not strictly necessary for a read-only op, but cleanly completes it
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}

	return servers, rev, nil
}

// GetAdmin retrieves the password hash and disabled status for an admin.
func (s *Store) GetAdmin(username string) (string, int, error) {
	var hash string
	var disabled int
	err := s.db.QueryRow("SELECT pass_hash, disabled FROM admins WHERE username = ?", username).Scan(&hash, &disabled)
	return hash, disabled, err
}

// CreateSession stores a new authenticated session token hash.
func (s *Store) CreateSession(tokenHash, username, expiresAt string) error {
	_, err := s.db.Exec("INSERT INTO sessions (id_hash, username, expires_at) VALUES (?, ?, ?)", tokenHash, username, expiresAt)
	return err
}

// GetSession retrieves the session details for a given token hash.
func (s *Store) GetSession(tokenHash string) (string, string, error) {
	var username, expiresAt string
	query := `
		SELECT s.username, s.expires_at 
		FROM sessions s 
		JOIN admins a ON s.username = a.username 
		WHERE s.id_hash = ? AND a.disabled = 0`
	err := s.db.QueryRow(query, tokenHash).Scan(&username, &expiresAt)
	return username, expiresAt, err
}

// DeleteSession removes a specific session by its token hash.
func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE id_hash = ?", tokenHash)
	return err
}

// DeleteExpiredSessions removes all sessions past their expiration date.
func (s *Store) DeleteExpiredSessions(now string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE expires_at < ?", now)
	return err
}

// DeleteSessionsForUser removes all sessions for a specific admin.
func (s *Store) DeleteSessionsForUser(username string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE username = ?", username)
	return err
}

// LogAction records an administrative action for audit purposes.
func (s *Store) LogAction(username, action, target string) error {
	_, err := s.db.Exec("INSERT INTO admin_actions (username, action, target, ts) VALUES (?, ?, ?, ?)",
		username, action, target, time.Now().UTC().Format(time.RFC3339))
	return err
}

// logActionTx writes an attribution row inside an open transaction.
func logActionTx(tx *sql.Tx, actor, action, target string) error {
	_, err := tx.Exec("INSERT INTO admin_actions (username, action, target, ts) VALUES (?, ?, ?, ?)",
		actor, action, target, time.Now().UTC().Format(time.RFC3339))
	return err
}

/*
 * ListActions returns one page of the attribution log, newest first, and the
 * total number of rows. It orders by id rather than ts: ts has one-second
 * resolution, so rows written in the same second tie and a page boundary
 * between them can repeat or skip a row. id is unique and rises with every
 * insert.
 */
func (s *Store) ListActions(limit, offset int) ([]AdminAction, int, error) {
	if limit <= 0 {
		limit = 50
	}
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM admin_actions").Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT username, action, target, ts FROM admin_actions
		ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	// Non-nil so an empty page encodes as [] rather than null.
	out := []AdminAction{}
	for rows.Next() {
		var a AdminAction
		if err := rows.Scan(&a.Username, &a.Action, &a.Target, &a.Ts); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// CreateAdmin adds an administrator without attribution. It is for bootstrap
// and tests; request handlers use CreateAdminAs.
func (s *Store) CreateAdmin(username, passHash string) error {
	_, err := s.db.Exec("INSERT INTO admins (username, pass_hash, created_at, disabled) VALUES (?, ?, ?, 0)",
		username, passHash, time.Now().UTC().Format(time.RFC3339))
	if IsUniqueViolation(err) {
		return ErrAdminExists
	}
	return err
}

/*
 * CreateAdminAs adds an administrator and records who did it, in one
 * transaction: either both rows are written or neither is.
 */
func (s *Store) CreateAdminAs(actor, username, passHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("INSERT INTO admins (username, pass_hash, created_at, disabled) VALUES (?, ?, ?, 0)",
		username, passHash, time.Now().UTC().Format(time.RFC3339)); err != nil {
		if IsUniqueViolation(err) {
			return ErrAdminExists
		}
		return err
	}
	if err := logActionTx(tx, actor, "CREATE_ADMIN", username); err != nil {
		return err
	}
	return tx.Commit()
}

/*
 * DisableAdmin disables target, deletes all of its sessions and records the
 * action, in one transaction. The "at least one other active admin" rule is
 * part of the UPDATE itself, so two admins disabling each other concurrently
 * cannot both succeed. It returns sql.ErrNoRows for an unknown admin and
 * ErrLastAdmin if target is the only active admin. Disabling an already
 * disabled admin is a no-op that still clears sessions.
 */
func (s *Store) DisableAdmin(actor, target string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE admins SET disabled = 1
		WHERE username = ? AND disabled = 0
		AND (SELECT COUNT(*) FROM admins WHERE disabled = 0) > 1`, target)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var disabled int
		err := tx.QueryRow("SELECT disabled FROM admins WHERE username = ?", target).Scan(&disabled)
		if err != nil {
			return err // includes sql.ErrNoRows
		}
		if disabled == 0 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE username = ?", target); err != nil {
		return err
	}
	if err := logActionTx(tx, actor, "DISABLE_ADMIN", target); err != nil {
		return err
	}
	return tx.Commit()
}

/*
 * ChangeAdminPassword replaces target's password hash, deletes all of its
 * sessions and records the action, in one transaction. It returns
 * sql.ErrNoRows for an unknown admin.
 */
func (s *Store) ChangeAdminPassword(actor, target, passHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec("UPDATE admins SET pass_hash = ? WHERE username = ?", passHash, target)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.Exec("DELETE FROM sessions WHERE username = ?", target); err != nil {
		return err
	}
	if err := logActionTx(tx, actor, "CHANGE_PASSWORD", target); err != nil {
		return err
	}
	return tx.Commit()
}

type Admin struct {
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
	Disabled  bool   `json:"disabled"`

	// LastLoginAt is nil for an admin who has never signed in.
	LastLoginAt *string `json:"last_login_at"`
}

// RecordLogin stamps the admin's last successful sign-in.
func (s *Store) RecordLogin(username string) error {
	_, err := s.db.Exec("UPDATE admins SET last_login_at = ? WHERE username = ?",
		time.Now().UTC().Format(time.RFC3339), username)
	return err
}

// ListAdmins returns all administrators, ordered by username.
func (s *Store) ListAdmins() ([]Admin, error) {
	rows, err := s.db.Query("SELECT username, created_at, disabled, last_login_at FROM admins ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		var a Admin
		var disabledInt int
		var lastLogin sql.NullString
		if err := rows.Scan(&a.Username, &a.CreatedAt, &disabledInt, &lastLogin); err != nil {
			return nil, err
		}
		a.Disabled = disabledInt == 1
		if lastLogin.Valid {
			a.LastLoginAt = &lastLogin.String
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetServerName retrieves a server's name by its ID.
func (s *Store) GetServerName(id int) string {
	var name string
	s.db.QueryRow("SELECT name FROM servers WHERE id = ?", id).Scan(&name)
	return name
}

// UpstreamSummary is one upstream's row in a Summary.
type UpstreamSummary struct {
	Upstream     string `json:"upstream"`
	OpenSessions int    `json:"open_sessions"`
	Statements   int    `json:"statements"`
}

// Summary aggregates the audit log for the back-office dashboard.
type Summary struct {
	Sessions        int               `json:"sessions"`
	OpenSessions    int               `json:"open_sessions"`
	FailedLogins    int               `json:"failed_logins"`
	StatementErrors int               `json:"statement_errors"`
	SchemaChanges   int               `json:"schema_changes"`
	ByUpstream      []UpstreamSummary `json:"by_upstream"`
}

/*
 * GetSummary aggregates sessions and statements between from and to (either may
 * be empty). Sessions are windowed by when they connected, statements by when
 * they ran, which is how the list endpoints filter them too.
 *
 * The per-upstream statement count comes from its own GROUP BY over
 * statements. Counting inside the connections GROUP BY would return one
 * arbitrary connection's count for each upstream rather than the total.
 */
func (s *Store) GetSummary(from, to string) (Summary, error) {
	connWhere, connArgs := timeWindow("ts_connect", from, to)
	stmtWhere, stmtArgs := timeWindow("ts", from, to)

	var sum Summary
	err := s.db.QueryRow(`SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN `+openSessionSQL+` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN auth_result = 'fail' THEN 1 ELSE 0 END), 0)
		FROM connections WHERE 1=1`+connWhere, connArgs...).
		Scan(&sum.Sessions, &sum.OpenSessions, &sum.FailedLogins)
	if err != nil {
		return Summary{}, err
	}

	schemaIn, schemaArgs := stmtTypeIn("schema")
	err = s.db.QueryRow(`SELECT
			COALESCE(SUM(CASE WHEN error IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN `+schemaIn+` THEN 1 ELSE 0 END), 0)
		FROM statements WHERE 1=1`+stmtWhere, append(schemaArgs, stmtArgs...)...).
		Scan(&sum.StatementErrors, &sum.SchemaChanges)
	if err != nil {
		return Summary{}, err
	}

	byUpstream := map[string]*UpstreamSummary{}
	entry := func(name string) *UpstreamSummary {
		if u, ok := byUpstream[name]; ok {
			return u
		}
		u := &UpstreamSummary{Upstream: name}
		byUpstream[name] = u
		return u
	}

	rows, err := s.db.Query(`SELECT upstream, COUNT(*) FROM connections
		WHERE `+openSessionSQL+` AND upstream IS NOT NULL AND upstream != ''`+connWhere+`
		GROUP BY upstream`, connArgs...)
	if err != nil {
		return Summary{}, err
	}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			rows.Close()
			return Summary{}, err
		}
		entry(name).OpenSessions = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Summary{}, err
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT upstream, COUNT(*) FROM statements
		WHERE upstream IS NOT NULL AND upstream != ''`+stmtWhere+`
		GROUP BY upstream`, stmtArgs...)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return Summary{}, err
		}
		entry(name).Statements = n
	}
	if err := rows.Err(); err != nil {
		return Summary{}, err
	}

	// Non-nil so no upstreams encodes as [] rather than null.
	sum.ByUpstream = []UpstreamSummary{}
	for _, u := range byUpstream {
		sum.ByUpstream = append(sum.ByUpstream, *u)
	}
	sort.Slice(sum.ByUpstream, func(i, j int) bool {
		return sum.ByUpstream[i].Upstream < sum.ByUpstream[j].Upstream
	})
	return sum, nil
}

// timeWindow returns " AND col >= ? AND col <= ?" for whichever of from and to
// is set, with its arguments.
func timeWindow(col, from, to string) (string, []interface{}) {
	var where string
	var args []interface{}
	if from != "" {
		where += " AND " + col + " >= ?"
		args = append(args, normalizeTime(from))
	}
	if to != "" {
		where += " AND " + col + " <= ?"
		args = append(args, normalizeTime(to))
	}
	return where, args
}
