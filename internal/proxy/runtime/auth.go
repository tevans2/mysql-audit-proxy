package runtime

import (
	"net"
	"strconv"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/google/uuid"

	"lustre/internal/audit"
	"lustre/internal/passthrough"
)

// authLoginTimeout bounds one client's whole login: the proxy's connection to
// the database, every packet exchanged with the database, and every packet
// exchanged with the client while the database's challenge is relayed.
const authLoginTimeout = 10 * time.Second

// loginTimeout is LoginTimeout, or authLoginTimeout if it is unset.
func (p *Proxy) loginTimeout() time.Duration {
	if p.LoginTimeout > 0 {
		return p.LoginTimeout
	}
	return authLoginTimeout
}

// auditingAuthHandler is the server.AuthenticationHandler for one client
// connection. The credential side is passthrough.Credentials, which holds no
// users and no passwords: the database decides every login (see
// internal/passthrough). This type adds the two things the proxy needs around
// that: an auth_fail event when the database rejects a login (OnAuthFailure),
// and applying the session's requested default database once the login has
// succeeded (OnAuthSuccess).
//
// One of these is built per connection, because OnAuthSuccess needs this
// session's own provider and proxyHandler — see newAuditingAuthHandler.
type auditingAuthHandler struct {
	passthrough.Credentials

	sink         audit.Sink
	upstreamName string
	upstreamAddr string

	// provider holds this session's upstream connection once Authenticate has
	// logged the client in. It keeps ownership of it, so the connection is
	// closed whichever way the session ends — including a failure in
	// OnAuthSuccess, which used to leak it.
	provider *passthrough.Provider

	// handler is this session's proxyHandler — the same one passed to
	// server.NewCustomizedConn alongside this auditingAuthHandler.
	handler *proxyHandler
}

// OnAuthFailure is go-mysql's hook for *any* error that aborts the handshake,
// not only a rejected login: server.Conn.handshake() calls it whenever
// readHandshakeResponse returns an error at all. Three situations reach it,
// and only the first is a failed login that belongs in the audit log:
//
//   - The database rejected the login: wrong password, a user it does not
//     know, a locked account, a host the account may not connect from.
//     passthrough.Provider returns the database's own error unwrapped, so it
//     arrives here as a *mysql.MyError with the database's code and reason,
//     which is what is recorded (see passthrough.LoginRefusal).
//   - The proxy could not log anyone in at all: the bootstrap account failed,
//     the database was unreachable, TLS could not be established, or the
//     client was refused for not using TLS. These carry other error codes and
//     are deliberately not auth_fail events (LoginRefusal excludes them). They
//     are not a user's failed login, and recording a proxy misconfiguration as
//     one would put false security events in the audit log (the Provider logs
//     the detail for the operator instead).
//   - A handshake that never produced a username: a bare TCP connect-then-close
//     (a health check), a non-MySQL client, or a malformed packet.
//     conn.GetUser() is empty, and auditing it would be a failed-login record
//     for no one.
//
// What separates the first from the rest is the error code, not the wrapped
// sentinel: server.Conn.handshake() rewrites go-mysql's own rejections into a
// fresh *mysql.MyError before calling this hook, so there is nothing left to
// match on but the code. The proxy's own errors use 1105 and 3159, which
// passthrough.LoginRefusal sets aside; every other code is the database's.
//
// The connection is closed by the caller regardless of what this does, so
// there is no session to correlate a later disconnect event with — each
// genuine failed login gets its own conn_id, the same way a rejected attempt
// never gets to open a session in the first place.
func (h *auditingAuthHandler) OnAuthFailure(conn *server.Conn, authErr error) {
	user := conn.GetUser()
	myErr, isRefusal := passthrough.LoginRefusal(authErr)
	if user == "" || !isRefusal {
		return
	}

	var proxyPort int
	if _, portStr, err := net.SplitHostPort(conn.LocalAddr().String()); err == nil {
		proxyPort, _ = strconv.Atoi(portStr)
	}

	h.sink.Emit(audit.Event{
		ConnID:       uuid.NewString(),
		Kind:         audit.KindAuthFail,
		User:         user,
		ClientAddr:   conn.RemoteAddr().String(),
		ProxyPort:    proxyPort,
		Upstream:     h.upstreamName,
		UpstreamAddr: h.upstreamAddr,
		AuthResult:   audit.AuthFail,
		// The database's own words, e.g. "Access denied for user 'alice'@'…'
		// (using password: YES)". The host in it is the proxy's address as the
		// database saw it, not the client's: the database only ever meets the
		// proxy (the audit event's ClientAddr is the real client).
		Err: myErr.Error(),
		At:  time.Now().UTC().Format(eventTimeFormat),
	})
}

// OnAuthSuccess is go-mysql's hook for a completed handshake, called after the
// database has accepted the login but before the client receives the OK
// packet — the last point at which returning an error still aborts the
// connection cleanly instead of sending a spurious OK.
//
// It gives the session's proxyHandler the upstream connection the Provider
// just logged in as the client's user, then selects the default database the
// client asked for, if any. proxyHandler.UseDB deliberately does not contact
// the database during the handshake (go-mysql calls it before the login is
// checked); it only records the name. That keeps the database name from being
// usable by an unauthenticated client to probe which databases exist, and
// lets a rejected login with a bogus database name still be audited as the
// credential rejection it is. Applying it here means a name that does not
// exist fails the connection, but only for someone who has already logged in.
//
// If applying it fails, the error reaches the client via c.writeError inside
// handshake(), and it is not audited: it is not a credential rejection, and
// OnAuthFailure is never called for an OnAuthSuccess error. The upstream
// connection is closed by the Provider's owner (handleClient) on the way out.
func (h *auditingAuthHandler) OnAuthSuccess(conn *server.Conn) error {
	upstreamConn := h.provider.Conn()
	if upstreamConn == nil {
		// Unreachable while the Provider is the authentication provider.
		return &mysql.MyError{Code: mysql.ER_UNKNOWN_ERROR, State: "HY000", Message: "The proxy could not complete authentication with the database"}
	}
	h.handler.upstream = upstreamConn
	return h.handler.applyPendingDB()
}

// newAuditingAuthHandler builds the authentication handler for one session,
// so a failed login is audited (auditingAuthHandler.OnAuthFailure) and the
// session's default database is applied only once the login has succeeded
// (auditingAuthHandler.OnAuthSuccess). provider and handler are that
// session's own, the same values passed to the handshake server and to
// server.NewCustomizedConn.
//
// upstreamName and upstreamAddr come from the caller's own captured
// upstreamConfig (see handleClient) rather than being read from p directly,
// so an auth failure logged mid-handshake reports the upstream this session
// is actually using, not whatever Proxy's fields happen to hold if an
// UpdateUpstream landed in between.
func (p *Proxy) newAuditingAuthHandler(provider *passthrough.Provider, handler *proxyHandler, upstreamName, upstreamAddr string) *auditingAuthHandler {
	return &auditingAuthHandler{
		sink:         p.Sink,
		upstreamName: upstreamName,
		upstreamAddr: upstreamAddr,
		provider:     provider,
		handler:      handler,
	}
}
