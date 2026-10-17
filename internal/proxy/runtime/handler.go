package runtime

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/go-mysql-org/go-mysql/stmt"

	"lustre/internal/audit"
)

// eventTimeFormat is how audit.Event.At is rendered.
//
// RFC3339Nano rather than RFC3339: the store de-duplicates statements on
// (conn_id, ts, sql_text), so whole-second timestamps would make it discard
// repeats of the same query within one second as if they were sink retries.
const eventTimeFormat = time.RFC3339Nano

// ---------------------------------------------------------------------------
// proxyHandler — implements go-mysql server.Handler by forwarding queries to
// the upstream MySQL server.
// ---------------------------------------------------------------------------

// proxyHandler relays the client's commands to the upstream MySQL server.
//
// upstream is a single connection, not a pool, so the client's session state
// (selected database, transactions, session variables) maps one-to-one onto an
// upstream session.
type proxyHandler struct {
	server.EmptyHandler
	upstream *client.Conn

	// upstreamMu serialises every use of upstream. The command-loop goroutine
	// (UseDB/HandleQuery) and the idle health-check goroutine (watchUpstream)
	// both drive the same underlying MySQL connection, and the protocol
	// assumes one command in flight at a time — without this, a ping racing a
	// real query would interleave reads/writes on the same socket and corrupt
	// both.
	upstreamMu sync.Mutex

	sink   audit.Sink
	connID string

	// currentDB is the schema most recently selected via UseDB, if any.
	// Reported on statement events per the data-model contract.
	currentDB string

	// upstreamDead means the upstream connection is gone and the session
	// cannot recover. Set from either the command-loop goroutine (a failed
	// query/UseDB) or the health-check goroutine (a failed idle ping), so it
	// is an atomic rather than a plain bool.
	upstreamDead atomic.Bool

	// commandStartedAt is the UnixNano start time of whatever HandleQuery or
	// UseDB call currently holds upstreamMu, or 0 when idle. It lets the
	// watcher — which never blocks waiting for the lock — read back how long
	// the current command has actually been running without taking the lock
	// itself, so it can tell a genuinely wedged upstream from ordinary query
	// traffic contending for the same tick.
	//
	// Must be stamped after Lock() and cleared before Unlock(), not around
	// the lock acquisition itself: mutex wait time is contention, not
	// upstream I/O time, and counting it here would make a query that simply
	// queued behind the watcher's own ping look like it's already over
	// budget the moment it starts.
	commandStartedAt atomic.Int64

	// authenticated is false for the entire handshake and flips true right
	// as auditingAuthHandler.OnAuthSuccess starts applying the deferred
	// database selection (applyPendingDB). While false, UseDB is being
	// called from inside the handshake itself (readDb, before credentials
	// are checked) and must not touch the upstream — see UseDB and
	// applyPendingDB. Never touched concurrently: nothing but this session's
	// own single handshake goroutine calls UseDB or applyPendingDB before
	// the handshake finishes, and watchUpstream only starts afterwards.
	authenticated bool

	// pendingDB is the database the client asked for as part of the
	// handshake (CLIENT_CONNECT_WITH_DB), recorded by UseDB while
	// authenticated is still false, and applied for real by applyPendingDB
	// once it's safe to. Empty if the client didn't request one.
	pendingDB string

	// preparedStmts maps the *stmt.PreparedStmt context handed back to
	// HandleStmtExecute/HandleStmtClose to the upstream *client.Stmt it
	// belongs to. See HandleStmtPrepare for why the context has to be that
	// exact type rather than the *client.Stmt itself. Only ever touched from
	// the single command-loop goroutine that runs this session's
	// HandleStmtPrepare/Execute/Close calls (MySQL's protocol is one command
	// in flight at a time per connection), so it needs no mutex of its own —
	// unlike upstream, watchUpstream never reaches into it.
	preparedStmts map[*stmt.PreparedStmt]*client.Stmt
}

// isUpstreamLost reports whether err represents a dead transport rather than
// the upstream rejecting a statement. A *mysql.MyError is a normal SQL error
// (bad syntax, missing table, …); the client sees it and the session
// continues. Anything else — read/write failures, EOF, timeouts — means the
// connection itself is gone and the session cannot recover.
func isUpstreamLost(err error) bool {
	var myErr *mysql.MyError
	return err != nil && !errors.As(err, &myErr)
}

// classifyErr flags transport failures, which end the session, and returns
// the error unchanged.
func (h *proxyHandler) classifyErr(err error) error {
	if isUpstreamLost(err) {
		h.upstreamDead.Store(true)
	}
	return err
}

// UseDB forwards COM_INIT_DB to the upstream — except during the handshake
// itself, when it instead just records dbName and always reports success.
//
// go-mysql calls this same method two ways: once from inside the handshake
// (readDb, part of readHandshakeResponse, for CLIENT_CONNECT_WITH_DB — a
// client's "connect straight to this database"), and later, post-handshake,
// for an explicit COM_INIT_DB command (a client's own "USE dbname"). The
// first call happens before credentials are ever compared — readDb runs,
// then readPluginName, then finally compareAuthData — so if this actually
// contacted the upstream there, a bad database name would fail the
// handshake on the upstream's "unknown database" error before the password
// is even checked. That derails auditingAuthHandler.OnAuthFailure two ways
// at once: a rejected login with a bogus database gets no credential error
// to audit at all (or the wrong one), and — independently — an
// unauthenticated client can use the database name to probe which databases
// exist upstream, before proving they have any right to ask.
//
// So while authenticated is still false (i.e. this is the handshake-time
// call), defer: remember dbName as pendingDB and return nil unconditionally,
// letting readHandshakeResponse carry on to the real credential check
// regardless of whether the database exists. Once credentials do check out,
// auditingAuthHandler.OnAuthSuccess calls applyPendingDB, which flips
// authenticated to true and calls this method again for real — so the
// upstream is only ever told the requested database once the client has
// already authenticated, and a bad name surfaces as a normal post-auth
// connection failure rather than a pre-auth information leak.
func (h *proxyHandler) UseDB(dbName string) error {
	if !h.authenticated {
		h.pendingDB = dbName
		return nil
	}

	h.upstreamMu.Lock()
	h.commandStartedAt.Store(time.Now().UnixNano())
	err := h.classifyErr(h.upstream.UseDB(dbName))
	h.commandStartedAt.Store(0)
	h.upstreamMu.Unlock()
	if err == nil {
		h.currentDB = dbName
		return nil
	}
	// go-mysql's client wraps the database's answer (errors.Trace), and its
	// server side only recognises a bare *mysql.MyError (writeError uses a type
	// assertion, not errors.As). Hand back the bare one so the client sees the
	// database's own code (1044, 1049, ...) instead of a generic 1105.
	var myErr *mysql.MyError
	if errors.As(err, &myErr) {
		return myErr
	}
	return err
}

// applyPendingDB is called once, from auditingAuthHandler.OnAuthSuccess,
// after credentials have checked out but before the client receives the OK
// packet. It flips authenticated so UseDB starts behaving normally again,
// then — if the client asked for a database as part of the handshake —
// actually forwards it upstream, returning whatever error that produces
// (e.g. "unknown database") so OnAuthSuccess can abort the connection
// cleanly, the same client-facing failure as before this session was
// deferring the check, just now only reachable after authentication.
func (h *proxyHandler) applyPendingDB() error {
	h.authenticated = true
	if h.pendingDB == "" {
		return nil
	}
	return h.UseDB(h.pendingDB)
}

// HandleQuery forwards a COM_QUERY to the upstream and returns its result
// unchanged — Execute already returns the *mysql.Result the server side sends.
//
// Do not wrap the error: server.writeError type asserts on *mysql.MyError
// instead of using errors.As, so wrapping collapses every upstream error code
// to ER_UNKNOWN_ERROR (1105). Do not pass args either, or the statement goes
// through Prepare and the binary protocol instead of being forwarded verbatim.
func (h *proxyHandler) HandleQuery(query string) (*mysql.Result, error) {
	h.upstreamMu.Lock()
	h.commandStartedAt.Store(time.Now().UnixNano())
	result, err := h.upstream.Execute(query)
	h.commandStartedAt.Store(0)
	h.upstreamMu.Unlock()
	err = h.classifyErr(err)

	// A literal "USE dbname" query changes the upstream's selected database
	// (Execute above already forwarded it there), but doesn't go through
	// UseDB — track it here so currentDB stays accurate for later events.
	if err == nil {
		if dbName, ok := extractUseDB(query); ok {
			h.currentDB = dbName
		}
	}

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
