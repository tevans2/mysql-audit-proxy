package store

import (
	"strings"
	"time"
)

/*
 * Retention: the store's half of expiring audit records. It only finds expired
 * rows and deletes the ones it is told to. Deciding what to do with them first
 * (archive, or nothing) and when to run is internal/retention's job, so a
 * deletion here is never a surprise.
 *
 * What expires:
 *
 *   - A statement expires when its own timestamp is older than the cutoff.
 *   - A connection expires when its last activity (its disconnect time, or its
 *     connect time if it never disconnected) is older than the cutoff AND it
 *     has no statements left. A long session that began before the cutoff and
 *     is still running, or that ran a statement after the cutoff, therefore
 *     keeps its connection row, while its older statements are already gone.
 *     A failed login has no statements, so it simply expires with its
 *     timestamp.
 *
 * Statements are deleted before connections, because statements.conn_id
 * references connections (foreign keys are on).
 *
 * A row with no timestamp at all is never expired: it cannot be dated, and
 * deleting audit data we cannot date is worse than keeping it.
 */

// ExpiredStatement is one statements row, with its id, as it was stored.
type ExpiredStatement struct {
	ID       int64   `json:"id"`
	ConnID   string  `json:"conn_id"`
	DBUser   *string `json:"db_user"`
	Upstream *string `json:"upstream"`
	SQL      string  `json:"sql"`
	Ts       string  `json:"ts"`
	StmtType string  `json:"stmt_type"`
	DB       *string `json:"db"`
	Error    *string `json:"error"`
}

// ExpiredConnection is one connections row as it was stored.
type ExpiredConnection struct {
	ConnID         string  `json:"conn_id"`
	ClientAddr     *string `json:"client_addr"`
	ProxyPort      *int    `json:"proxy_port"`
	DBUser         *string `json:"db_user"`
	Upstream       *string `json:"upstream"`
	UpstreamAddr   *string `json:"upstream_addr"`
	AuthResult     *string `json:"auth_result"`
	TsConnect      *string `json:"ts_connect"`
	TsDisconnect   *string `json:"ts_disconnect"`
	AuthFailReason *string `json:"auth_fail_reason"`
}

// deleteBatch is how many ids one DELETE statement names, comfortably below
// SQLite's limit on bound parameters.
const deleteBatch = 500

// cutoffText renders cutoff the way timestamps are stored, so the comparison
// SQLite does on the text is a comparison of times.
func cutoffText(cutoff time.Time) string {
	return cutoff.UTC().Format(tsFormat)
}

/*
 * ExpiredStatements returns up to limit statements older than cutoff, oldest
 * first. It deletes nothing. Ordering by ts is what lets SQLite walk
 * idx_statements_ts and stop after limit rows; ordering by id made it scan the
 * whole table.
 */
func (s *Store) ExpiredStatements(cutoff time.Time, limit int) ([]ExpiredStatement, error) {
	rows, err := s.db.Query(expiredStatementsSQL, cutoffText(cutoff), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ExpiredStatement{}
	for rows.Next() {
		var e ExpiredStatement
		var sqlText, stmtType, connID *string
		if err := rows.Scan(&e.ID, &connID, &e.DBUser, &e.Upstream, &sqlText, &e.Ts, &stmtType, &e.DB, &e.Error); err != nil {
			return nil, err
		}
		if connID != nil {
			e.ConnID = *connID
		}
		if sqlText != nil {
			e.SQL = *sqlText
		}
		if stmtType != nil {
			e.StmtType = *stmtType
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

/*
 * DeleteStatements deletes the statements with these ids and returns how many
 * rows went. One transaction, so a crash leaves either all of them or none.
 */
func (s *Store) DeleteStatements(ids []int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var total int64
	for start := 0; start < len(ids); start += deleteBatch {
		end := min(start+deleteBatch, len(ids))
		args := make([]any, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		res, err := tx.Exec("DELETE FROM statements WHERE id IN ("+placeholders(end-start)+")", args...)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, tx.Commit()
}

/*
 * ExpiredConnections returns up to limit connections whose last activity is
 * older than cutoff and which have no statements. Run it after the expired
 * statements are gone. It deletes nothing.
 */
func (s *Store) ExpiredConnections(cutoff time.Time, limit int) ([]ExpiredConnection, error) {
	rows, err := s.db.Query(expiredConnectionsSQL, cutoffText(cutoff), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ExpiredConnection{}
	for rows.Next() {
		var c ExpiredConnection
		if err := rows.Scan(&c.ConnID, &c.ClientAddr, &c.ProxyPort, &c.DBUser, &c.Upstream,
			&c.UpstreamAddr, &c.AuthResult, &c.TsConnect, &c.TsDisconnect, &c.AuthFailReason); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

/*
 * CountExpiredStatements returns how many statements are older than cutoff. It
 * deletes nothing; it is what the startup report and a dry run are built on.
 */
func (s *Store) CountExpiredStatements(cutoff time.Time) (int64, error) {
	var n int64
	err := s.db.QueryRow(countExpiredStatementsSQL, cutoffText(cutoff)).Scan(&n)
	return n, err
}

/*
 * CountExpiredConnections returns how many connections a purge at cutoff would
 * remove: those whose last activity is older than cutoff and whose statements
 * are all expired too. That is the number after the statements have gone, so
 * it is right before a purge has run as well as after. It deletes nothing.
 */
func (s *Store) CountExpiredConnections(cutoff time.Time) (int64, error) {
	var n int64
	c := cutoffText(cutoff)
	err := s.db.QueryRow(countExpiredConnectionsSQL, c, c).Scan(&n)
	return n, err
}

/*
 * DeleteExpiredConnections deletes the named connections, but only those that
 * are still expired and still have no statements. Re-checking here, rather
 * than trusting the earlier read, means a connection that received a late
 * disconnect or statement between the read and the delete is kept. It returns
 * how many rows went.
 */
func (s *Store) DeleteExpiredConnections(cutoff time.Time, connIDs []string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var total int64
	for start := 0; start < len(connIDs); start += deleteBatch {
		end := min(start+deleteBatch, len(connIDs))
		args := []any{cutoffText(cutoff)}
		for _, id := range connIDs[start:end] {
			args = append(args, id)
		}
		res, err := tx.Exec(`DELETE FROM connections AS c
			WHERE `+lastActivity+` IS NOT NULL AND `+lastActivity+` != '' AND `+lastActivity+` < ?
			  AND NOT EXISTS (SELECT 1 FROM statements s WHERE s.conn_id = c.conn_id)
			  AND c.conn_id IN (`+placeholders(end-start)+`)`, args...)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, tx.Commit()
}

/*
 * lastActivity is the expression a connection's age is judged by. It is also
 * the expression idx_connections_last indexes: SQLite only uses an expression
 * index for the identical text, so change both together.
 */
const lastActivity = "COALESCE(c.ts_disconnect, c.ts_connect)"

/*
 * The queries that find expired rows. Each names its index with INDEXED BY
 * so SQLite cannot choose a full scan instead: its choice depends on table
 * statistics we do not control, and a scan of the whole audit log every hour is
 * exactly what the indexes exist to avoid. If an index is ever missing the query
 * fails loudly ("no such index"), which InitDB prevents by creating both.
 */
const expiredStatementsSQL = `SELECT id, conn_id, db_user, upstream, sql_text, ts, stmt_type, db, error
	FROM statements INDEXED BY idx_statements_ts
	WHERE ts IS NOT NULL AND ts != '' AND ts < ?
	ORDER BY ts, id LIMIT ?`

const expiredConnectionsSQL = `SELECT c.conn_id, c.client_addr, c.proxy_port, c.db_user, c.upstream,
		c.upstream_addr, c.auth_result, c.ts_connect, c.ts_disconnect, c.auth_fail_reason
	FROM connections c INDEXED BY idx_connections_last
	WHERE ` + lastActivity + ` IS NOT NULL AND ` + lastActivity + ` != '' AND ` + lastActivity + ` < ?
	  AND NOT EXISTS (SELECT 1 FROM statements s WHERE s.conn_id = c.conn_id)
	ORDER BY ` + lastActivity + `, c.conn_id LIMIT ?`

const countExpiredStatementsSQL = `SELECT COUNT(*)
	FROM statements INDEXED BY idx_statements_ts
	WHERE ts IS NOT NULL AND ts != '' AND ts < ?`

/*
 * A connection counts as expired when its last activity is old and every one of
 * its statements is itself expired (or it has none). A statement with no
 * timestamp is never expired, so it keeps its connection.
 */
const countExpiredConnectionsSQL = `SELECT COUNT(*)
	FROM connections c INDEXED BY idx_connections_last
	WHERE ` + lastActivity + ` IS NOT NULL AND ` + lastActivity + ` != '' AND ` + lastActivity + ` < ?
	  AND NOT EXISTS (SELECT 1 FROM statements s WHERE s.conn_id = c.conn_id
	                  AND NOT (s.ts IS NOT NULL AND s.ts != '' AND s.ts < ?))`

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
