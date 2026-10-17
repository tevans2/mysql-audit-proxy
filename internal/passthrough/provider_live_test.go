package passthrough

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lustre/internal/upstream"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/packet"
	"github.com/go-mysql-org/go-mysql/server"
)

// These tests run the real Provider, inside a real go-mysql server, in front
// of a real MySQL (db-a from `make test-env`). They skip when no MySQL is
// reachable, unless PROXY_TEST_REQUIRE_UPSTREAM is set, which makes that a
// failure (as `make test` and CI do).
//
//	PROXY_TEST_UPSTREAM_ADDR  default 127.0.0.1:3306
//	PROXY_TEST_UPSTREAM_USER  default root
//	PROXY_TEST_UPSTREAM_PASS  default rootpass

const requireUpstreamEnv = "PROXY_TEST_REQUIRE_UPSTREAM"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type account struct{ name, password string }

type liveDB struct {
	addr    string
	tls     *tls.Config
	admin   *client.Conn
	secrets []string
	mu      sync.Mutex
}

var accountCounter atomic.Uint64

func newLiveDB(t *testing.T) *liveDB {
	t.Helper()
	addr := envOr("PROXY_TEST_UPSTREAM_ADDR", "127.0.0.1:3306")
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		if os.Getenv(requireUpstreamEnv) != "" {
			t.Fatalf("%s is set but MySQL is not reachable at %s: %v", requireUpstreamEnv, addr, err)
		}
		t.Skipf("MySQL not reachable at %s (run `make test-env`): %v", addr, err)
	}
	c.Close()

	// The test database presents a self-signed certificate; the point of these
	// tests is the authentication relay, not certificate checking, which has
	// its own tests.
	cfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	admin, err := upstream.Connect(context.Background(), addr,
		envOr("PROXY_TEST_UPSTREAM_USER", "root"), envOr("PROXY_TEST_UPSTREAM_PASS", "rootpass"),
		cfg, 5*time.Second, 5*time.Second)
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	// The tests bring their own tables, so they run against any MySQL (the
	// CI service has no fixture data). IF NOT EXISTS makes parallel packages
	// and repeated runs harmless.
	for _, q := range []string{
		"CREATE DATABASE IF NOT EXISTS lustre_pt",
		"CREATE TABLE IF NOT EXISTS lustre_pt.customers (id INT PRIMARY KEY)",
		"CREATE TABLE IF NOT EXISTS lustre_pt.secret (id INT PRIMARY KEY)",
	} {
		if _, err := admin.Execute(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return &liveDB{addr: addr, tls: cfg, admin: admin}
}

func (db *liveDB) exec(t *testing.T, format string, args ...any) {
	t.Helper()
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, err := db.admin.Execute(fmt.Sprintf(format, args...)); err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
}

// newAccount creates a database user with the given plugin ("" for the
// server default). With grant it may read lustre_pt.customers (and not lustre_pt.secret);
// without, it has no privileges at all (the shape of the bootstrap account).
func (db *liveDB) newAccount(t *testing.T, plugin string, grant bool) account {
	t.Helper()
	id := strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatUint(accountCounter.Add(1), 36)
	a := account{name: "pt_" + id, password: "pw_" + id}
	if plugin == "" {
		db.exec(t, "CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", a.name, a.password)
	} else {
		db.exec(t, "CREATE USER '%s'@'%%' IDENTIFIED WITH %s BY '%s'", a.name, plugin, a.password)
	}
	t.Cleanup(func() {
		db.mu.Lock()
		defer db.mu.Unlock()
		_, _ = db.admin.Execute(fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", a.name))
	})
	if grant {
		db.exec(t, "GRANT SELECT ON lustre_pt.customers TO '%s'@'%%'", a.name)
	}
	db.secrets = append(db.secrets, a.password)
	return a
}

// authRecorder is the handler the proxy would use, recording what go-mysql
// reports to OnAuthFailure so tests can ask: would this have been audited?
type authRecorder struct {
	Credentials
	mu       sync.Mutex
	failures []error
}

func (a *authRecorder) OnAuthFailure(_ *server.Conn, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = append(a.failures, err)
}

// auditedRejections applies the same rule as auditingAuthHandler.OnAuthFailure
// in cmd/proxy (LoginRefusal): only the database's refusals of a user's login
// become auth_fail.
func (a *authRecorder) auditedRejections() []*mysql.MyError {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*mysql.MyError
	for _, err := range a.failures {
		if me, ok := LoginRefusal(err); ok {
			out = append(out, me)
		}
	}
	return out
}

type queryHandler struct {
	server.EmptyHandler
	p *Provider
}

func (h *queryHandler) HandleQuery(query string) (*mysql.Result, error) {
	res, err := h.p.Conn().Execute(query)
	if err != nil {
		if me := asMyError(err); me != nil {
			return nil, me // unwrapped, or go-mysql would send 1105
		}
		return nil, err
	}
	return res, nil
}

func asMyError(err error) *mysql.MyError {
	var me *mysql.MyError
	if errors.As(err, &me) {
		return me
	}
	return nil
}

type testProxy struct {
	addr string
	logs *logCapture
	auth *authRecorder
	// ioTimeout is the read timeout left on the most recent upstream
	// connection, in nanoseconds, recorded once its login succeeded.
	ioTimeout atomic.Int64
}

// testIOTimeout is the per-read limit the test proxy asks for. It is
// deliberately unlike the login timeout, so mixing them up shows.
const testIOTimeout = 7 * time.Second

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startProxy listens on a loopback port and serves each connection the way
// cmd/proxy will: one Provider and one go-mysql server per client.
func startProxy(t *testing.T, db *liveDB, bootstrap account, timeout time.Duration) *testProxy {
	t.Helper()
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}, MinVersion: tls.VersionTLS12}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	tp := &testProxy{addr: l.Addr().String(), logs: &logCapture{}, auth: &authRecorder{}}
	db.mu.Lock()
	db.secrets = append(db.secrets, bootstrap.password)
	db.mu.Unlock()

	go func() {
		for {
			raw, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				p := NewProvider(Config{
					Addr: db.addr, BootstrapUser: bootstrap.name, BootstrapPassword: bootstrap.password,
					TLS: db.tls, Timeout: timeout, IOTimeout: testIOTimeout, Logf: tp.logs.Logf,
				})
				defer p.Close()
				svr := server.NewServerWithAuth("8.0.11", mysql.DEFAULT_COLLATION_ID,
					mysql.AUTH_CACHING_SHA2_PASSWORD, nil, tlsCfg, p)
				conn, err := svr.NewCustomizedConn(raw, tp.auth, &queryHandler{p: p})
				if err != nil {
					return
				}
				tp.ioTimeout.Store(int64(p.Conn().ReadTimeout))
				for conn.HandleCommand() == nil {
				}
			}()
		}
	}()
	return tp
}

func (tp *testProxy) assertNoSecrets(t *testing.T, db *liveDB) {
	t.Helper()
	db.mu.Lock()
	secrets := append([]string(nil), db.secrets...)
	db.mu.Unlock()
	text := tp.logs.text()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("a password appeared in the proxy log: %q", text)
		}
	}
}

func dial(addr string, a account, password string, secure bool) (*client.Conn, error) {
	return client.Connect(addr, a.name, password, "", func(c *client.Conn) error {
		if secure {
			c.SetTLSConfig(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
		}
		return nil
	})
}

func TestLiveLoginIsTheRealUser(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	proxy := startProxy(t, db, boot, 10*time.Second)

	for _, plugin := range []string{mysql.AUTH_NATIVE_PASSWORD, mysql.AUTH_CACHING_SHA2_PASSWORD, mysql.AUTH_SHA256_PASSWORD} {
		t.Run(plugin, func(t *testing.T) {
			acct := db.newAccount(t, plugin, true)
			// Twice: for caching_sha2 the first login is full authentication
			// and the second takes the database's fast-auth path.
			for attempt := 1; attempt <= 2; attempt++ {
				conn, err := dial(proxy.addr, acct, acct.password, true)
				if err != nil {
					t.Fatalf("login %d: %v", attempt, err)
				}

				res, err := conn.Execute("SELECT USER()")
				if err != nil {
					t.Fatalf("SELECT USER(): %v", err)
				}
				got, _ := res.GetString(0, 0)
				if !strings.HasPrefix(got, acct.name+"@") {
					t.Errorf("SELECT USER() = %q, want it to start with %q", got, acct.name+"@")
				}

				if _, err := conn.Execute("SELECT COUNT(*) FROM lustre_pt.customers"); err != nil {
					t.Errorf("granted table was refused: %v", err)
				}

				_, err = conn.Execute("SELECT * FROM lustre_pt.secret")
				if me := asMyError(err); me == nil || me.Code != mysql.ER_TABLEACCESS_DENIED_ERROR {
					t.Errorf("ungranted table: got %v, want error 1142 from the database", err)
				}
				conn.Close()
			}
		})
	}
	// The login's own timeout must not stay on the connection: a long query
	// would then be cut off after it.
	if got := time.Duration(proxy.ioTimeout.Load()); got != testIOTimeout {
		t.Errorf("upstream read timeout after login = %v, want the session's %v", got, testIOTimeout)
	}
	proxy.assertNoSecrets(t, db)
}

func TestLiveWrongPasswordIsTheDatabasesAnswer(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	proxy := startProxy(t, db, boot, 10*time.Second)
	acct := db.newAccount(t, mysql.AUTH_CACHING_SHA2_PASSWORD, true)

	_, err := dial(proxy.addr, acct, "wrong-"+acct.password, true)
	me := asMyError(err)
	if me == nil || me.Code != mysql.ER_ACCESS_DENIED_ERROR {
		t.Fatalf("got %v, want error 1045", err)
	}
	if !strings.Contains(me.Message, acct.name) {
		t.Errorf("the database's reason was lost: %q", me.Message)
	}
	if strings.Contains(me.Message, boot.name) {
		t.Errorf("the bootstrap account name reached the client: %q", me.Message)
	}

	rej := proxy.auth.auditedRejections()
	if len(rej) != 1 || rej[0].Code != mysql.ER_ACCESS_DENIED_ERROR || !strings.Contains(rej[0].Message, acct.name) {
		t.Errorf("auth_fail would see %v, want exactly one 1045 naming %q", rej, acct.name)
	}
	proxy.assertNoSecrets(t, db)
}

func TestLiveUnknownUserIsRefusedByTheDatabase(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	proxy := startProxy(t, db, boot, 10*time.Second)

	ghost := account{name: "nobody_" + strconv.FormatInt(time.Now().UnixNano(), 36), password: "whatever"}
	_, err := dial(proxy.addr, ghost, ghost.password, true)
	if me := asMyError(err); me == nil || me.Code != mysql.ER_ACCESS_DENIED_ERROR {
		t.Fatalf("got %v, want error 1045", err)
	}
	if len(proxy.auth.auditedRejections()) != 1 {
		t.Errorf("an unknown user must be audited as one failed login, got %v", proxy.auth.auditedRejections())
	}
}

func TestLiveBootstrapFailureIsNotAUsersFailedLogin(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	acct := db.newAccount(t, mysql.AUTH_CACHING_SHA2_PASSWORD, true)
	broken := account{name: boot.name, password: "wrong-" + boot.password}
	db.secrets = append(db.secrets, broken.password)
	proxy := startProxy(t, db, broken, 10*time.Second)

	_, err := dial(proxy.addr, acct, acct.password, true)
	me := asMyError(err)
	if me == nil || me.Code != mysql.ER_UNKNOWN_ERROR {
		t.Fatalf("got %v, want the generic error 1105", err)
	}
	if strings.Contains(me.Message, boot.name) || strings.Contains(me.Message, "Access denied") {
		t.Errorf("the client was told about the bootstrap account: %q", me.Message)
	}
	if rej := proxy.auth.auditedRejections(); len(rej) != 0 {
		t.Errorf("a proxy misconfiguration would be audited as the user's failed login: %v", rej)
	}
	if text := proxy.logs.text(); !strings.Contains(text, boot.name) || !strings.Contains(text, "bootstrap") {
		t.Errorf("the operator log does not say what is wrong: %q", text)
	}
	proxy.assertNoSecrets(t, db)
}

func TestLivePlaintextClientIsRefusedBeforeTouchingTheDatabase(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	proxy := startProxy(t, db, boot, 10*time.Second)
	acct := db.newAccount(t, mysql.AUTH_CACHING_SHA2_PASSWORD, true)

	_, err := dial(proxy.addr, acct, acct.password, false)
	if me := asMyError(err); me == nil || me.Code != errSecureTransportRequired {
		t.Fatalf("got %v, want error %d", err, errSecureTransportRequired)
	}
	if rej := proxy.auth.auditedRejections(); len(rej) != 0 {
		t.Errorf("a plaintext attempt was audited as a failed password: %v", rej)
	}
}

// A client that starts logging in and then goes silent must be dropped when
// the login deadline passes, not held for ever.
func TestLiveSilentClientIsDropped(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	acct := db.newAccount(t, mysql.AUTH_CACHING_SHA2_PASSWORD, true)
	proxy := startProxy(t, db, boot, time.Second)

	raw, err := net.Dial("tcp", proxy.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))

	pc := packet.NewConn(raw)
	if _, err := pc.ReadPacket(); err != nil { // the server greeting
		t.Fatal(err)
	}
	const caps = mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SSL
	header := func() []byte {
		b := make([]byte, 4+32) // packet header slot, then caps, max packet, charset, 23 reserved
		binary.LittleEndian.PutUint32(b[4:], caps)
		b[4+8] = mysql.DEFAULT_COLLATION_ID
		return b
	}
	if err := pc.WritePacket(header()); err != nil { // SSLRequest
		t.Fatal(err)
	}
	tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	pc2 := packet.NewConn(tc)
	pc2.Sequence = pc.Sequence

	resp := append(header(), acct.name...)
	resp = append(resp, 0, 0) // end of user name, empty auth response
	resp = append(resp, mysql.AUTH_CACHING_SHA2_PASSWORD...)
	resp = append(resp, 0)
	if err := pc2.WritePacket(resp); err != nil {
		t.Fatal(err)
	}

	first, err := pc2.ReadPacket()
	if err != nil {
		t.Fatalf("reading the auth switch: %v", err)
	}
	if len(first) == 0 || first[0] != mysql.EOF_HEADER {
		t.Fatalf("expected the database's auth switch, got % x", first)
	}

	// Say nothing. The proxy must give up on its own.
	start := time.Now()
	dropped := false
	for i := 0; i < 3 && !dropped; i++ {
		if _, err := pc2.ReadPacket(); err != nil {
			dropped = true
		}
	}
	if !dropped {
		t.Fatal("the proxy kept a silent client")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the proxy took %v to drop a silent client with a 1s login deadline", elapsed)
	}
}
