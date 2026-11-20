// Package runtime serves MySQL sessions and reconciles their listeners against live configuration.
package runtime

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/proxy/config"
)

// ---------------------------------------------------------------------------
// Proxy — ties listener + handler + sink together.
// ---------------------------------------------------------------------------

// upstreamConfig is the immutable snapshot of a Proxy's upstream connection
// details that one session actually uses, once it has been accepted.
//
// It exists so an in-place upstream update (see Proxy.UpdateUpstream) can
// change what new connections get without disturbing sessions already in
// flight: handleClient loads *upstreamConfig once, right at accept time,
// and uses only that local copy for the rest of the session. UpdateUpstream
// only ever swaps Proxy.upstream to point at a brand new upstreamConfig
// value — it never mutates one a session might already hold a pointer to —
// so an already-running session's config can't change out from under it,
// and there is no lock to take on the hot path to read it safely.
type upstreamConfig struct {
	Addr string
	Name string

	// User and Pass are the bootstrap account: the powerless login the proxy
	// opens each upstream connection with before switching to the connecting
	// client's own identity with COM_CHANGE_USER (see internal/passthrough).
	// It needs no privileges at all (GRANT USAGE ON *.* is enough), and it is
	// never a login clients can use or are told about.
	User string
	Pass string

	// TLS is the client TLS configuration a session uses to reach this
	// upstream. It is built for Addr's own host (certificate verification and
	// ServerName included) at the same moment, and from the same settings, as
	// the rest of this snapshot, so a session can never pair one upstream's
	// address with another's certificate settings — which is exactly what
	// happened while TLS lived on Proxy as one value built once at startup. It
	// is never mutated after it is built; see the type comment.
	TLS *tls.Config

	// TLSMode and TLSCAFile are the settings TLS was built from (the
	// server's "tls" and "tls_ca_file"), kept so UpdateUpstream can rebuild
	// TLS for a new address without the caller restating them.
	TLSMode   string
	TLSCAFile string
}

// Proxy holds the configuration and dependencies for a single proxy instance.
type Proxy struct {
	TLS          string
	TLSCAFile    string
	upstreamTLS  *tls.Config
	ListenAddr   string
	UpstreamAddr string
	UpstreamName string
	UpstreamUser string
	UpstreamPass string

	UpstreamReadTimeout    time.Duration
	UpstreamCommandTimeout time.Duration

	// LoginTimeout bounds one client's whole login, from accept to the OK
	// packet. Zero means authLoginTimeout. A client that connects and then
	// says nothing, or stops answering the database's challenge, is dropped
	// when it passes.
	LoginTimeout time.Duration

	Logger *log.Logger

	Sink audit.Sink

	// hsRSAKey and hsTLS are the client-facing handshake's key material,
	// generated once in Run and shared by every connection. Each connection
	// gets its own go-mysql server around them (see newHandshakeServerFor),
	// because go-mysql hangs the authentication provider off the server and
	// each provider belongs to one connection.
	hsRSAKey *rsa.PrivateKey
	hsTLS    *tls.Config

	// acceptGate, when non-nil, holds Run back from accepting any connection
	// until it is closed. The listener is already bound (and has reported
	// itself via ready) by then, so the port is reserved, but nothing is
	// served. The Reconciler uses it to bring a listener up, finish
	// configuring it, and only then let it serve.
	acceptGate <-chan struct{}

	// activeSessions counts client sessions this proxy is currently
	// handling — incremented in Run right after Accept, decremented when
	// handleClient returns. It exists for observability now (a future
	// reconciliation manager logs it) and, together with sessionWG below,
	// as the building block a drain step needs: knowing how many sessions
	// are still open, and being able to wait for them to finish, without
	// that waiting ever blocking Run's own accept loop (a plain counter
	// can't be waited on; sessionWG can).
	activeSessions atomic.Int64
	sessionWG      sync.WaitGroup

	// upstream holds this proxy's current upstreamConfig, seeded once in Run
	// from the plain Upstream* fields above and swapped atomically by
	// UpdateUpstream from then on. handleClient reads it exactly once per
	// session (see upstreamConfig's comment); nothing else reads it, and
	// nothing ever writes through it in place.
	upstream atomic.Pointer[upstreamConfig]

	// conns and connsMu track every client connection currently accepted by
	// this proxy, keyed by the net.Conn itself, so CloseActiveSessions can
	// forcibly close them all — the "close remaining sessions after the
	// configured drain timeout" half of the ticket's disable/delete
	// behaviour. Populated by trackConn in Run's accept loop, cleared by
	// untrackConn once handleClient returns for that connection.
	connsMu sync.Mutex
	conns   map[net.Conn]struct{}
}

// Run starts the proxy and blocks until ctx is cancelled or the listener
// fails outright. To allow tests to get the actual listen address (when
// using ":0"), it returns the net.Listener via an optional channel.
//
// Cancelling ctx closes the listener — that is the only thing that stops the
// accept loop below, and it is a deliberate, narrow primitive: it stops new
// connections from being accepted and does nothing else. It does not touch
// any client session already accepted (those are handleClient goroutines
// running independently, tracked separately via activeSessions/sessionWG),
// and it is not itself a wait for those sessions to finish — see
// WaitSessions for that. This split is exactly what a caller managing a
// listener's lifecycle needs to be able to do independently: stop accepting
// new connections on one listener while an old one (or the sessions it
// already has open) keeps running, without one action forcing the other.
//
// A closed listener surfaces to Accept as net.ErrClosed. That is treated as
// a normal, expected way for this loop to end — not logged as an accept
// error — since ctx cancellation is the intended way to stop Run, not a
// malfunction.
func (p *Proxy) Run(ctx context.Context, ready chan<- net.Listener) error {

	if err := p.configureUpstreamTLS(); err != nil {
		return err
	}

	// Generate the client-facing handshake's key material before opening the
	// listener: it is the only thing here that can fail, and there is no
	// point binding a port only to tear it down again if it does.
	rsaKey, hsTLS, err := newHandshakeMaterial()
	if err != nil {
		return err
	}
	p.hsRSAKey, p.hsTLS = rsaKey, hsTLS

	// Seed the atomically-swappable upstream snapshot from the plain
	// Upstream* fields. From here on those plain fields are only the
	// original static configuration; UpdateUpstream and handleClient both
	// go through p.upstream instead — see upstreamConfig's comment.
	p.upstream.Store(&upstreamConfig{
		Addr:      p.UpstreamAddr,
		Name:      p.UpstreamName,
		User:      p.UpstreamUser,
		Pass:      p.UpstreamPass,
		TLS:       p.upstreamTLS,
		TLSMode:   p.TLS,
		TLSCAFile: p.TLSCAFile,
	})

	// "tcp", not "tcp4": ListenAddr can legitimately be an IPv6 address (a
	// bracketed literal or a hostname that resolves to one), and forcing
	// tcp4 would silently refuse to bind those. On some platforms this
	// means a listener bound to an unspecified IPv4 address like
	// "0.0.0.0:port" can end up reporting itself as "[::]:port" in the log
	// line below — cosmetic (clients still reach it fine), and the
	// trade-off for not breaking IPv6 configurations.
	listener, err := net.Listen("tcp", p.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	// Every way out of Run releases the port. The goroutine below closes the
	// listener when ctx is cancelled, but it also stops when Run returns, and
	// if that happens first (a return while waiting on acceptGate, say) it may
	// never close it. Closing twice is harmless.
	defer listener.Close()

	// Tie the listener's lifetime to ctx: as soon as ctx is done, close it,
	// which is what actually unblocks Accept below with net.ErrClosed. The
	// stopWatching channel exists only so this goroutine doesn't leak past
	// Run returning by some other route (the listener itself failing, say);
	// without it, a Run that exits some other way would leave this goroutine
	// parked on <-ctx.Done() for however long ctx (often the process
	// lifetime) takes to actually cancel.
	stopWatching := make(chan struct{})
	defer close(stopWatching)
	go func() {
		select {
		case <-ctx.Done():
			listener.Close()
		case <-stopWatching:
		}
	}()

	p.Logger.Printf("Proxy listening on %s, forwarding to %s", listener.Addr(), p.UpstreamAddr)

	// Signal the actual listener address to callers (used by tests).
	if ready != nil {
		ready <- listener
	}

	// If the owner asked for it, hold off serving until released. The port is
	// already held and reported above; connections that arrive meanwhile wait
	// in the listener's backlog, and nothing is accepted, handshaken or
	// connected upstream until the gate opens. If ctx is cancelled first the
	// listener is closed by the goroutine above and Run simply returns.
	if p.acceptGate != nil {
		select {
		case <-p.acceptGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				// Expected: either ctx was cancelled (the normal way to stop
				// Run) or something else closed this exact listener. Either
				// way there is nothing left to accept on, so return instead
				// of logging this as an accept failure and spinning on the
				// same error forever.
				return ctx.Err()
			}
			p.Logger.Printf("accept error: %v", err)
			continue
		}

		// sessionWG.Add must happen before activeSessions is incremented, not
		// after: a caller (WaitSessions) only learns a session has started by
		// observing activeSessions go up, and that atomic read is the only
		// happens-before edge it has with this goroutine. Incrementing
		// activeSessions first would let that caller call sessionWG.Wait()
		// concurrently with this sessionWG.Add — a real, race-detector-flagged
		// hazard even though in practice Add nearly always "wins" the race.
		p.sessionWG.Add(1)
		p.activeSessions.Add(1)
		p.trackConn(clientConn)
		go func() {
			defer p.sessionWG.Done()
			defer p.activeSessions.Add(-1)
			defer p.untrackConn(clientConn)
			p.handleClient(clientConn)
		}()
	}
}

// BuildProxies turns configured servers into one *Proxy per entry, each with
// its own listener, upstream and credentials, sharing only the audit sink and
// the set of client logins — clients connect as themselves on any port, and
// it is the port/upstream pairing, not who logged in, that the audit log
// tells sessions apart by.
//
// passwords supplies each server's upstream password by name. It has no
// fallback to a shared default: db-a and db-b are deliberately given
// different root passwords elsewhere in this project specifically so that a
// listener pointed at the wrong upstream, or carrying the wrong credentials,
// fails to connect instead of connecting anyway (see deploy/README.md) — a
// missing password here is the same kind of misconfiguration, so it is
// fatal rather than silently reused from another entry.
func BuildProxies(servers []audit.Server, passwords map[string]string, sink audit.Sink, upstreamReadTimeout, upstreamCommandTimeout time.Duration) ([]*Proxy, error) {
	if len(servers) == 0 {
		return nil, errors.New("no enabled upstream servers configured")
	}
	if err := config.ValidateServers(servers); err != nil {
		return nil, fmt.Errorf("invalid server configuration: %w", err)
	}

	proxies := make([]*Proxy, 0, len(servers))
	for _, s := range servers {
		pass, ok := passwords[s.Name]
		if !ok {
			return nil, fmt.Errorf("no password for upstream %q: set it in PROXY_UPSTREAM_PASSWORDS", s.Name)
		}
		proxies = append(proxies, &Proxy{
			TLS:                    s.TLS,
			TLSCAFile:              s.TLSCAFile,
			ListenAddr:             net.JoinHostPort("0.0.0.0", strconv.Itoa(s.ListenPort)),
			UpstreamAddr:           net.JoinHostPort(s.UpstreamHost, strconv.Itoa(s.UpstreamPort)),
			UpstreamName:           s.Name,
			UpstreamUser:           s.UpstreamUser,
			UpstreamPass:           pass,
			UpstreamReadTimeout:    upstreamReadTimeout,
			UpstreamCommandTimeout: upstreamCommandTimeout,
			Sink:                   sink,
			Logger:                 log.Default(),
		})
	}
	return proxies, nil
}

// RunAll starts every proxy concurrently and waits for the first one to
// return. Proxy.Run only returns when ctx is cancelled or its listener fails
// to bind or accept — otherwise it blocks forever — so in the normal case
// (ctx never cancelled) this blocks forever too, and a bind failure on any
// one listener is treated as fatal for all of them, consistent with Run's
// own single-proxy behaviour in main below.
//
// ctx is not yet cancelled from anywhere in main — that lands with the
// reconciliation manager, which needs to retire individual listeners (not
// this whole-process all-or-nothing stop) by cancelling each Proxy's own
// context independently. Threading ctx through here now, ahead of that,
// keeps this call site already correct for it rather than needing a second
// signature change later.
func RunAll(ctx context.Context, proxies []*Proxy) error {
	return RunAllNotify(ctx, proxies, nil)
}

// RunAllNotify is RunAll, and calls onBound once every listener is bound and
// accepting, so a caller can report readiness only when the proxy can actually
// take connections. onBound may be nil. If a listener fails first, Run's error
// is returned and onBound is never called.
func RunAllNotify(ctx context.Context, proxies []*Proxy, onBound func()) error {
	errCh := make(chan error, len(proxies))
	bound := make(chan net.Listener, len(proxies))
	for _, p := range proxies {
		p := p
		go func() { errCh <- p.Run(ctx, bound) }()
	}
	if onBound != nil {
		go func() {
			for range proxies {
				<-bound
			}
			onBound()
		}()
	}
	return <-errCh
}
