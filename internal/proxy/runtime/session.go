package runtime

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/passthrough"
)

// ActiveSessions returns the number of client sessions this proxy is
// currently handling.
func (p *Proxy) ActiveSessions() int64 {
	return p.activeSessions.Load()
}

// UpdateUpstream atomically swaps this proxy's upstream connection details,
// keeping the TLS mode and CA file it already has but rebuilding the TLS
// configuration for the (possibly new) address. It affects only sessions
// accepted after it returns — see upstreamConfig's comment for why an
// already-running session is unaffected — and it never touches Proxy's own
// plain Upstream*/TLS* fields, which after Run has started are stale by
// design (upstream is the only thing handleClient consults).
//
// It returns an error, and changes nothing, if the new configuration cannot
// be built. To change the TLS settings themselves use UpdateUpstreamTLS.
func (p *Proxy) UpdateUpstream(addr, name, user, pass string) error {
	mode, caFile := p.TLS, p.TLSCAFile
	if cur := p.upstream.Load(); cur != nil {
		mode, caFile = cur.TLSMode, cur.TLSCAFile
	}
	return p.UpdateUpstreamTLS(addr, name, user, pass, mode, caFile)
}

// trackConn registers clientConn so CloseActiveSessions can find and close
// it later. Called from Run's accept loop, once per accepted connection.
func (p *Proxy) trackConn(c net.Conn) {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	if p.conns == nil {
		p.conns = make(map[net.Conn]struct{})
	}
	p.conns[c] = struct{}{}
}

// untrackConn removes clientConn from the registry trackConn added it to.
// Called once handleClient returns for that connection, regardless of how
// it ended (client-initiated close, upstream failure, or a forced close
// from CloseActiveSessions).
func (p *Proxy) untrackConn(c net.Conn) {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	delete(p.conns, c)
}

// CloseActiveSessions forcibly closes every client connection this proxy is
// currently tracking. It is the forceful half of a drain: WaitSessions (or
// Drain, below) is how a caller waits for sessions to end on their own;
// this is what a caller reaches for once a drain timeout has elapsed and
// some sessions are still open.
func (p *Proxy) CloseActiveSessions() {
	p.connsMu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.connsMu.Unlock()

	for _, c := range conns {
		c.Close()
	}
}

// forceCloseGracePeriod bounds how long Drain waits for sessions to actually
// unwind (handleClient returning, its deferred audit/disconnect logic
// finishing) after CloseActiveSessions has forced their connections closed.
// A forced close should unblock a session almost immediately, so this only
// exists to keep Drain from being able to block forever if something is
// stuck — it is not itself a configurable drain timeout.
const forceCloseGracePeriod = 5 * time.Second

// Drain stops accepting new work on already-open sessions gracefully: it
// waits up to timeout for every currently-tracked session to finish on its
// own, and if timeout elapses while sessions are still open, forcibly
// closes them via CloseActiveSessions and waits (bounded by
// forceCloseGracePeriod) for that to actually take effect.
//
// Drain does not itself stop new connections from being accepted — pair it
// with cancelling this proxy's own Run context first (see the ticket's
// "stop accepting new connections independently of existing sessions"
// requirement), which is exactly what the reconciler's remove/move-port
// handling does.
func (p *Proxy) Drain(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := p.WaitSessions(ctx); err == nil {
		return
	}

	p.CloseActiveSessions()

	forceCtx, forceCancel := context.WithTimeout(context.Background(), forceCloseGracePeriod)
	defer forceCancel()
	p.WaitSessions(forceCtx)
}

// WaitSessions blocks until every session Run has ever accepted has
// finished, or ctx is done first, whichever happens first. It does not stop
// new sessions from starting — pair it with a cancelled Run (no more
// accepts) if that's the intent, the same separation the reconciliation
// manager needs between "stop taking new connections" and "wait for the
// existing ones to drain".
func (p *Proxy) WaitSessions(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.sessionWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleClient processes a single client session: handshake, relay, cleanup.
func (p *Proxy) handleClient(clientConn net.Conn) {
	clientAddr := clientConn.RemoteAddr().String()
	proxyAddr := clientConn.LocalAddr().String()

	// Capture this session's upstream configuration exactly once, right now,
	// and use only this local copy for the rest of the function. A
	// concurrent UpdateUpstream swaps p.upstream to point at a new
	// upstreamConfig value — it never mutates the one cfg already points
	// to — so this snapshot stays exactly what it was at accept time no
	// matter what happens to the proxy's live configuration afterwards. See
	// upstreamConfig's comment for the full reasoning.
	cfg := p.upstream.Load()

	// One conn_id per session — attach it to both the connect and
	// disconnect events so they can be correlated.
	connID := uuid.NewString()
	handler := &proxyHandler{
		sink:   p.Sink,
		connID: connID,
	}

	// The upstream connection is opened during the client's handshake, not
	// before it: which database user to log in as is only known once the
	// client sends its handshake response. The provider opens it as the
	// bootstrap account, switches it to the client's own identity with
	// COM_CHANGE_USER, and relays the database's challenge to the client — so
	// the database, not the proxy, decides whether the login is valid.
	//
	// The provider owns that connection until the session ends, and this
	// deferred Close closes it however the session ends: a login the database
	// refused, a default database that does not exist (applyPendingDB failing
	// in OnAuthSuccess), the client going away, or a normal disconnect. It is
	// registered before the watcher's cleanup below, so it runs after it: the
	// watcher must be stopped before the connection it pings is closed.
	//
	// IOTimeout bounds every read on the upstream connection (Execute, UseDB,
	// Ping) so a wedged-but-not-closed upstream can't block a command forever
	// — see config.DefaultUpstreamReadTimeout. It also caps max single-query
	// duration; that trade-off is why it's configurable rather than fixed.
	tlsCfg := cfg.TLS
	if tlsCfg == nil {
		tlsCfg = p.upstreamTLS
	}
	loginTimeout := p.loginTimeout()
	provider := passthrough.NewProvider(passthrough.Config{
		Addr:              cfg.Addr,
		BootstrapUser:     cfg.User,
		BootstrapPassword: cfg.Pass,
		TLS:               tlsCfg,
		Timeout:           loginTimeout,
		IOTimeout:         p.UpstreamReadTimeout,
		Logf: func(format string, args ...any) {
			p.Logger.Printf("server %q: "+format, append([]any{cfg.Name}, args...)...)
		},
	})
	defer provider.Close()

	// Perform the MySQL handshake with the client. A server is built for this
	// connection alone, around the shared key material, because go-mysql keeps
	// the authentication provider on the server object.
	//
	// The whole handshake has a deadline, so a client that connects and says
	// nothing cannot hold a goroutine, and whatever it has opened, for ever.
	// It is lifted once the client is in.
	authHandler := p.newAuditingAuthHandler(provider, handler, cfg.Name, cfg.Addr)
	svr := newHandshakeServerFor(p.hsRSAKey, p.hsTLS, provider)
	_ = clientConn.SetDeadline(time.Now().Add(loginTimeout))
	conn, err := svr.NewCustomizedConn(clientConn, authHandler, handler)
	if err != nil {
		p.Logger.Printf("handshake error for %s: %v", clientAddr, err)
		clientConn.Close()
		return
	}
	_ = clientConn.SetDeadline(time.Time{})

	// Capture the username that the client presented during the handshake.
	username := conn.GetUser()

	// Watch the upstream for as long as this session lives, so a client that
	// goes idle right as the upstream disappears is still noticed rather than
	// left open indefinitely. The deferred close+Wait below stops it and
	// blocks until it has actually exited whenever the session ends by any
	// route, so it never outlives the connections it uses — closing upstream
	// while the watcher might still be mid-Ping on it would be its own race.
	stopWatch := make(chan struct{})
	var watcherWg sync.WaitGroup
	watcherWg.Add(1)
	go func() {
		defer watcherWg.Done()
		handler.watchUpstream(clientConn, stopWatch, p.UpstreamCommandTimeout)
	}()
	defer func() {
		close(stopWatch)
		watcherWg.Wait()
	}()

	// proxy_port is the numeric listen port, not the full "host:port" string.
	_, portStr, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		p.Logger.Printf("could not parse proxy port from %q: %v", proxyAddr, err)
	}
	proxyPort, err := strconv.Atoi(portStr)
	if err != nil {
		p.Logger.Printf("could not convert proxy port %q to int: %v", portStr, err)
	}

	// Emit the connect event. Reaching this point means the handshake succeeded,
	// so auth_result is "ok" — the contract documents the field and the UI shows
	// a badge for it, so leaving it unset renders blank on every connection.
	p.Sink.Emit(audit.Event{
		ConnID:       connID,
		Kind:         audit.KindConnect,
		User:         username,
		ClientAddr:   clientAddr,
		ProxyPort:    proxyPort,
		Upstream:     cfg.Name,
		UpstreamAddr: cfg.Addr,
		AuthResult:   audit.AuthOK,
		At:           time.Now().UTC().Format(eventTimeFormat),
	})

	// Ensure we emit a disconnect event when the session ends.
	defer func() {
		// Guard against nil-pointer panics in conn.Close() — when the
		// client has already disconnected, go-mysql's internal connection
		// state may already be torn down.
		if !conn.Closed() {
			func() {
				defer func() { recover() }()
				conn.Close()
			}()
		}
		// Also carries auth_result: this session authenticated, and if the
		// connect event was dropped by a full sink buffer the row is created
		// from this one instead.
		p.Sink.Emit(audit.Event{
			ConnID:       connID,
			Kind:         audit.KindDisconnect,
			User:         username,
			ClientAddr:   clientAddr,
			ProxyPort:    proxyPort,
			Upstream:     cfg.Name,
			UpstreamAddr: cfg.Addr,
			AuthResult:   audit.AuthOK,
			At:           time.Now().UTC().Format(eventTimeFormat),
		})
	}()

	// Command loop — handle client commands until the connection closes.
	// HandleCommand only errors when the client socket fails; a handler error
	// reaches the client as an error packet and is reported as success here, so
	// a dead upstream has to be checked separately.
	for {
		if err := conn.HandleCommand(); err != nil {
			p.Logger.Printf("session ended for %s@%s: %v", username, clientAddr, err)
			return
		}
		if handler.upstreamDead.Load() {
			p.Logger.Printf("upstream connection lost for %s@%s, closing client session", username, clientAddr)
			return
		}
	}
}
