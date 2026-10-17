package passthrough

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
)

// errSecureTransportRequired is MySQL's own code for refusing an insecure
// connection (ER_SECURE_TRANSPORT_REQUIRED). go-mysql does not define it.
const errSecureTransportRequired = 3159

// Config is what a Provider needs to log a connecting client in.
type Config struct {
	// Addr is the upstream database address.
	Addr string
	// BootstrapUser and BootstrapPassword are the powerless account the
	// upstream connection is opened with before COM_CHANGE_USER. They are never
	// shown to a client.
	BootstrapUser     string
	BootstrapPassword string
	// TLS is the upstream TLS configuration. Required.
	TLS *tls.Config
	// IOTimeout is the per-read and per-write limit left on the upstream
	// connection for the rest of the session. Zero means none.
	IOTimeout time.Duration
	// Timeout bounds the whole login: the upstream connect, every packet
	// exchanged with the database, and every packet exchanged with the client
	// during it.
	Timeout time.Duration
	// Logf receives operator-level messages. It never receives a password or
	// authentication bytes. Nil discards them.
	Logf func(format string, args ...any)
}

// Provider is go-mysql's server.AuthenticationProvider for pass-through
// authentication. It does not check any password itself: it opens the upstream
// connection as the bootstrap account, sends COM_CHANGE_USER for the user who
// is connecting, and relays the database's challenge to the client and the
// client's answer back. The database accepts or refuses the login.
//
// A Provider serves exactly one client connection, because go-mysql hangs the
// provider off the server object. Build one per accepted connection:
//
//	p := passthrough.NewProvider(cfg)
//	defer p.Close()
//	svr := server.NewServerWithAuth(..., p)
//	conn, err := svr.NewCustomizedConn(raw, passthrough.Credentials{}, handler)
//	upstream := p.Conn() // the logged-in database connection
type Provider struct {
	cfg  Config
	conn *client.Conn
}

var (
	_ server.AuthenticationProvider = (*Provider)(nil)
	_ server.AuthenticationHandler  = Credentials{}
)

// NewProvider returns a Provider for one client connection.
func NewProvider(cfg Config) *Provider {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Provider{cfg: cfg}
}

// Validate reports whether go-mysql may offer authPluginName to clients. The
// plugin the client uses first is irrelevant (the database's challenge is what
// counts), but go-mysql insists the server's default plugin is accepted.
func (p *Provider) Validate(authPluginName string) bool {
	switch authPluginName {
	case mysql.AUTH_NATIVE_PASSWORD, mysql.AUTH_CACHING_SHA2_PASSWORD, mysql.AUTH_SHA256_PASSWORD:
		return true
	}
	return false
}

// Authenticate logs the connecting client in at the database.
//
// clientAuthData is the client's first response. It was computed against a
// salt only go-mysql knows and means nothing to the database, so it is
// ignored (and, like every authentication byte, never logged). The client is
// then challenged with the database's own plugin and salt.
//
// Returning nil tells go-mysql the login succeeded. Every error is either the
// database's own refusal, returned as an unwrapped *mysql.MyError so its code
// and reason reach the client and the audit log, or a generic *mysql.MyError
// that reveals nothing about the proxy's internals.
func (p *Provider) Authenticate(c *server.Conn, authPluginName string, clientAuthData []byte) error {
	if p.conn != nil {
		return p.fail(c.GetUser(), errors.New("connection authenticated twice"))
	}
	relay := &clientRelay{c: c, logf: p.logf}
	if !relay.ClientOnTLS() {
		return &mysql.MyError{
			Code:    errSecureTransportRequired,
			State:   "HY000",
			Message: "Connections using insecure transport are prohibited while pass-through authentication is in use",
		}
	}

	// One deadline for the whole login covers the client's side too: the
	// relay's reads from the client are otherwise unbounded.
	nc := c.Conn.Conn
	_ = nc.SetDeadline(time.Now().Add(p.cfg.Timeout))
	defer func() { _ = nc.SetDeadline(time.Time{}) }()

	conn, err := DialChangeUserIO(context.Background(), p.cfg.Addr, p.cfg.BootstrapUser, p.cfg.BootstrapPassword,
		c.GetUser(), relay, p.cfg.Timeout, p.cfg.IOTimeout, p.cfg.TLS)
	if err != nil {
		return p.fail(c.GetUser(), err)
	}
	p.conn = conn
	return nil
}

// Conn returns the upstream connection logged in as the client's user, or nil
// if authentication has not succeeded. The Provider still owns it.
func (p *Provider) Conn() *client.Conn { return p.conn }

// Release hands the upstream connection to the caller, who must close it, and
// stops the Provider closing it.
func (p *Provider) Release() *client.Conn {
	conn := p.conn
	p.conn = nil
	return conn
}

// Close closes the upstream connection if the Provider still owns it. It is
// safe to call more than once, and exists so a connection opened during the
// handshake is not leaked when anything after authentication fails.
func (p *Provider) Close() {
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
}

// fail turns an internal error into what the client may see, and logs the
// detail for the operator.
func (p *Provider) fail(user string, err error) error {
	var be *BootstrapError
	if errors.As(err, &be) {
		p.logf("pass-through: bootstrap login to %s as %q failed, check the bootstrap credential: %s",
			p.cfg.Addr, p.cfg.BootstrapUser, be.Detail())
		return &mysql.MyError{Code: mysql.ER_UNKNOWN_ERROR, State: "HY000", Message: bootstrapClientMessage(be.Cause)}
	}
	var dbErr *mysql.MyError
	if errors.As(err, &dbErr) {
		// The database refused this user: pass its answer through untouched.
		return dbErr
	}
	p.logf("pass-through: login for %q was aborted: %v", user, err)
	return genericAuthError()
}

func (p *Provider) logf(format string, args ...any) {
	if p.cfg.Logf != nil {
		p.cfg.Logf(format, args...)
	}
}

// LoginRefusal reports whether err, as passed to go-mysql's OnAuthFailure hook,
// is the database refusing this user's login: a wrong password, an unknown
// user, a locked account, a host the account is not allowed from, a user over
// its connection limit. Those carry the database's own error code and reason
// and are what the audit log records as failed logins.
//
// Everything the proxy itself produces is not a refusal: the generic error for
// an infrastructure problem (the bootstrap account, TLS, an unreachable
// database, a timeout) and the refusal of a client that is not on TLS. They are
// the operator's concern, not a user's failed login, and recording them as one
// would put false security events in the audit log.
func LoginRefusal(err error) (*mysql.MyError, bool) {
	var me *mysql.MyError
	if !errors.As(err, &me) {
		return nil, false
	}
	switch me.Code {
	case mysql.ER_UNKNOWN_ERROR, errSecureTransportRequired:
		return nil, false
	}
	return me, true
}

const genericAuthMessage = "The proxy could not complete authentication with the database"

func genericAuthError() *mysql.MyError {
	return &mysql.MyError{Code: mysql.ER_UNKNOWN_ERROR, State: "HY000", Message: genericAuthMessage}
}

// bootstrapClientMessage is what a client is told when the proxy could not
// open its own connection to the database. It names TLS problems, which are
// the usual cause and which an administrator can fix, and reveals nothing
// else: no account name, no address, no certificate details.
func bootstrapClientMessage(cause error) string {
	var certErr *tls.CertificateVerificationError
	switch {
	case cause == nil:
		return genericAuthMessage
	case errors.As(cause, &certErr):
		return "Upstream TLS certificate verification failed; contact the administrator"
	case strings.Contains(cause.Error(), "does not support TLS"):
		return "Upstream server does not offer TLS; plaintext connections are forbidden"
	}
	return genericAuthMessage
}

// clientRelay is the client side of the exchange: it speaks to the connecting
// client the way a MySQL server would, for packets the database dictates.
type clientRelay struct {
	c    *server.Conn
	logf func(format string, args ...any)
}

func (r *clientRelay) ClientOnTLS() bool {
	_, ok := r.c.Conn.Conn.(*tls.Conn)
	return ok
}

// Challenge sends an AuthSwitchRequest with the database's plugin and salt and
// returns the client's raw answer.
func (r *clientRelay) Challenge(plugin string, salt []byte) ([]byte, error) {
	pkt := make([]byte, 4, 4+1+len(plugin)+1+len(salt)+1)
	pkt = append(pkt, mysql.EOF_HEADER)
	pkt = append(pkt, plugin...)
	pkt = append(pkt, 0)
	pkt = append(pkt, salt...)
	pkt = append(pkt, 0) // MySQL's own servers end the salt with a NUL; clients expect it
	if err := r.c.WritePacket(pkt); err != nil {
		return nil, fmt.Errorf("send auth switch to client: %w", err)
	}
	return r.read()
}

// Notify sends AuthMoreData that needs no answer (caching_sha2 fast auth).
func (r *clientRelay) Notify(payload []byte) error {
	pkt := make([]byte, 4, 4+1+len(payload))
	pkt = append(pkt, mysql.MORE_DATE_HEADER)
	pkt = append(pkt, payload...)
	if err := r.c.WritePacket(pkt); err != nil {
		return fmt.Errorf("send auth data to client: %w", err)
	}
	return nil
}

// Relay sends AuthMoreData and returns the client's raw answer. The answer may
// be a cleartext password (TLS is required, so that is safe); it is passed on
// untouched and never inspected or logged.
func (r *clientRelay) Relay(payload []byte) ([]byte, error) {
	if err := r.Notify(payload); err != nil {
		return nil, err
	}
	return r.read()
}

func (r *clientRelay) Log(message string) {
	if r.logf != nil {
		r.logf("pass-through: %s", message)
	}
}

func (r *clientRelay) read() ([]byte, error) {
	data, err := r.c.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("read client auth response: %w", err)
	}
	return data, nil
}

// Credentials is the server.AuthenticationHandler to pair with a Provider. It
// holds no users and no passwords: every username is "found", with a
// placeholder credential that is never compared against anything, because the
// Provider ignores go-mysql's own password check and the database decides.
//
// Embed it to add hooks (the proxy's auditing handler does).
type Credentials struct{}

// GetCredential accepts every non-empty user name.
func (Credentials) GetCredential(username string) (server.Credential, bool, error) {
	if username == "" {
		return server.Credential{}, false, nil
	}
	// The plugin is the server's default, so a stock client that answers
	// the handshake with that plugin is not switched before Authenticate runs.
	return server.Credential{
		Passwords:      []string{"unused"},
		AuthPluginName: mysql.AUTH_CACHING_SHA2_PASSWORD,
	}, true, nil
}

func (Credentials) OnAuthSuccess(*server.Conn) error { return nil }

func (Credentials) OnAuthFailure(*server.Conn, error) {}
