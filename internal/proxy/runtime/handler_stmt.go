package runtime

import (
	"fmt"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/stmt"

	"lustre/internal/audit"
)

// HandleStmtPrepare forwards a COM_STMT_PREPARE to the upstream and returns
// the parameter/column counts go-mysql needs to answer the client, plus a
// context it hands straight back to HandleStmtExecute/HandleStmtClose for
// this same statement.
//
// The context is the address of the upstream *client.Stmt's own embedded
// *stmt.PreparedStmt, not the *client.Stmt itself. That exact type matters:
// right after this returns, server.Conn's COM_STMT_PREPARE handling does
// `st.Context.(*stmt.PreparedStmt)` and, only on a match, copies
// RawParamFields/RawColumnFields onto the response it writes back to the
// client — the real upstream column/parameter metadata (names, types,
// nullability) rather than the library's generic "?" placeholder fields,
// which is what a client doing type- or name-based binding (JDBC,
// DBeaver, …) actually needs. clientStmt.Prepare already populated those
// Raw*Fields from the upstream's own PREPARE response, so returning
// &clientStmt.PreparedStmt is enough to make that copy happen — no manual
// translation needed.
//
// That leaves recovering the *client.Stmt itself at execute/close time: the
// context passed back there is this same *stmt.PreparedStmt pointer, but
// stmt.PreparedStmt doesn't reference its containing client.Stmt, so
// preparedStmts (on proxyHandler) is the map back from one to the other. Not
// server.Conn's own per-session statement-ID → *Stmt table — that already
// exists and is exactly why this handler needs no such table itself for
// the query text (see HandleStmtExecute) — just for this one pointer.
//
// A failed PREPARE is audited here, on its own error path, rather than left
// to HandleStmtExecute: a rejected PREPARE (bad table/column name, syntax
// error, missing privilege) never reaches EXECUTE at all — go-mysql doesn't
// hand back a statement ID for it, so there is nothing for the client to
// execute or close. Without this, someone probing for table/column names via
// COM_STMT_PREPARE (it still reaches the upstream and gets a real error, e.g.
// 1146 for an unknown table, before this function returns) would do so
// without leaving any audit trail — the exact gap a query-based probe
// wouldn't have, since a failed plain query is always audited by HandleQuery.
func (h *proxyHandler) HandleStmtPrepare(query string) (params int, columns int, context any, err error) {
	h.upstreamMu.Lock()
	h.commandStartedAt.Store(time.Now().UnixNano())
	clientStmt, prepErr := h.upstream.Prepare(query)
	h.commandStartedAt.Store(0)
	h.upstreamMu.Unlock()
	if prepErr != nil {
		prepErr = h.classifyErr(prepErr)
		h.sink.Emit(audit.Event{
			ConnID:   h.connID,
			Kind:     audit.KindStatement,
			DB:       h.currentDB,
			SQL:      query,
			StmtType: classifyStmtType(query),
			At:       time.Now().UTC().Format(eventTimeFormat),
			Err:      prepErr.Error(),
		})
		return 0, 0, nil, prepErr
	}

	pctx := &clientStmt.PreparedStmt
	if h.preparedStmts == nil {
		h.preparedStmts = make(map[*stmt.PreparedStmt]*client.Stmt)
	}
	h.preparedStmts[pctx] = clientStmt

	return clientStmt.ParamNum(), clientStmt.ColumnNum(), pctx, nil
}

// HandleStmtExecute forwards a COM_STMT_EXECUTE to the upstream prepared
// statement matching context and audits it with the prepared SQL text.
//
// query is exactly the string originally passed to HandleStmtPrepare — go-
// mysql remembers it per statement ID and hands it back here — so the audit
// event records the real SQL, not "executed statement 3": the whole point
// of this ticket. It does not include the bound parameter values, only the
// statement text with its placeholders, same as HandleQuery only ever sees
// literal SQL and never separates "the query" from "the data" either.
//
// Known limitation: a client executing a prepared statement that fails
// upstream (duplicate key, deadlock, lock timeout, …) sees ER_UNKNOWN_ERROR
// (1105) instead of the real code (1062, 1213, 1205, …), even though this
// method returns the *mysql.MyError from clientStmt.Execute unwrapped, the
// same way HandleQuery does for a plain query (see the comment there). The
// difference is where the wrapping happens: HandleQuery's caller in go-mysql
// (server/command.go's COM_QUERY case) returns our error as-is, but
// COM_STMT_EXECUTE's caller (server/stmt.go's handleStmtExecute, unexported —
// not the Handler method of the same name) always does
// `return nil, errors.Trace(err)` around whatever HandleStmtExecute returns,
// before go-mysql's writeError ever sees it. errors.Trace only adds a stack
// frame — the wrapped error still Unwraps to our original *mysql.MyError —
// but writeError (server/resp.go) checks with a plain type assertion,
// `e.(*mysql.MyError)`, not errors.As, so the wrap defeats it and it falls
// back to ER_UNKNOWN_ERROR with the original message text (which is why the
// client still sees "1062" inside the message, just not as the numeric
// code). This is entirely inside go-mysql v1.16.0's own COM_STMT_EXECUTE
// dispatch; there is no hook here to intervene before it wraps our return
// value, so it cannot be fixed from this handler. See README.md's Transparent
// Results note for the client-facing implication.
func (h *proxyHandler) HandleStmtExecute(context any, query string, args []any) (*mysql.Result, error) {
	pctx, ok := context.(*stmt.PreparedStmt)
	if !ok || pctx == nil {
		return nil, fmt.Errorf("invalid prepared statement context")
	}
	clientStmt, ok := h.preparedStmts[pctx]
	if !ok {
		return nil, fmt.Errorf("no upstream statement for this prepared statement")
	}

	h.upstreamMu.Lock()
	h.commandStartedAt.Store(time.Now().UnixNano())
	result, err := clientStmt.Execute(args...)
	h.commandStartedAt.Store(0)
	h.upstreamMu.Unlock()
	err = h.classifyErr(err)

	var errStr string
	if err != nil {
		errStr = err.Error()
	}

	h.sink.Emit(audit.Event{
		ConnID:   h.connID,
		Kind:     audit.KindStatement,
		DB:       h.currentDB,
		SQL:      query,
		StmtType: classifyStmtType(query),
		At:       time.Now().UTC().Format(eventTimeFormat),
		Err:      errStr,
	})

	return result, err
}

// HandleStmtClose forwards a COM_STMT_CLOSE to the upstream prepared
// statement matching context, freeing it there too — a prepared statement
// the client closes but the upstream never hears about would sit there for
// the life of the upstream connection. There is nothing to audit here: the
// ticket's "done when" is met by the execute-time event above, and a close
// carries no SQL or result of its own to record. COM_STMT_CLOSE has no
// response in the MySQL protocol either way, so the return value only ever
// reports a transport failure, never something written back to the client.
func (h *proxyHandler) HandleStmtClose(context any) error {
	pctx, ok := context.(*stmt.PreparedStmt)
	if !ok || pctx == nil {
		return nil
	}
	clientStmt, ok := h.preparedStmts[pctx]
	if !ok {
		return nil
	}
	delete(h.preparedStmts, pctx)

	h.upstreamMu.Lock()
	h.commandStartedAt.Store(time.Now().UnixNano())
	err := clientStmt.Close()
	h.commandStartedAt.Store(0)
	h.upstreamMu.Unlock()
	return h.classifyErr(err)
}
