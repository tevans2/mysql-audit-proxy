package passthrough

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/packet"
)

// Edge cases for pass-through authentication, against a real MySQL (see
// provider_live_test.go for how these find it and when they skip).

// Error numbers go-mysql does not name.
const (
	errAccountLocked  = 3118 // ER_ACCOUNT_HAS_BEEN_LOCKED
	errMustChangePass = 1862 // ER_MUST_CHANGE_PASSWORD_LOGIN
	errPassExpiredCmd = 1820 // ER_MUST_CHANGE_PASSWORD
)

// sqlStr quotes s as a SQL string literal.
func sqlStr(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// newCustomAccount creates an account with exactly this name and password.
// options is appended after IDENTIFIED BY (for example " ACCOUNT LOCK"). With
// grant it may read auditproxy_pt.customers.
func (db *liveDB) newCustomAccount(t *testing.T, name, password, options string, grant bool) account {
	t.Helper()
	db.exec(t, "%s", fmt.Sprintf("CREATE USER %s@'%%' IDENTIFIED BY %s%s", sqlStr(name), sqlStr(password), options))
	t.Cleanup(func() {
		db.mu.Lock()
		defer db.mu.Unlock()
		_, _ = db.admin.Execute(fmt.Sprintf("DROP USER IF EXISTS %s@'%%'", sqlStr(name)))
	})
	if grant {
		db.exec(t, "%s", fmt.Sprintf("GRANT SELECT ON auditproxy_pt.customers TO %s@'%%'", sqlStr(name)))
	}
	if password != "" {
		db.mu.Lock()
		db.secrets = append(db.secrets, password)
		db.mu.Unlock()
	}
	return account{name: name, password: password}
}

// sessionsOf counts the database sessions currently running as any of users.
func (db *liveDB) sessionsOf(t *testing.T, users ...string) int {
	t.Helper()
	conds := make([]string, len(users))
	for i, u := range users {
		conds[i] = "user = " + sqlStr(u)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	res, err := db.admin.Execute("SELECT COUNT(*) FROM information_schema.processlist WHERE " + strings.Join(conds, " OR "))
	if err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	n, err := res.GetInt(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

// waitForNoSessions fails unless every session of users ends within a few
// seconds. A session that never ends is an upstream connection the proxy leaked.
func (db *liveDB) waitForNoSessions(t *testing.T, what string, users ...string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		n := db.sessionsOf(t, users...)
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d database session(s) for %v are still open: the proxy leaked an upstream connection", what, n, users)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestLoginRefusalClassification(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"wrong password", &mysql.MyError{Code: mysql.ER_ACCESS_DENIED_ERROR}, true},
		{"unknown user", &mysql.MyError{Code: mysql.ER_NO_SUCH_USER}, true},
		{"locked account", &mysql.MyError{Code: errAccountLocked}, true},
		{"host not allowed", &mysql.MyError{Code: mysql.ER_HOST_NOT_PRIVILEGED}, true},
		{"too many connections for the user", &mysql.MyError{Code: mysql.ER_USER_LIMIT_REACHED}, true},
		{"expired password", &mysql.MyError{Code: errMustChangePass}, true},
		{"wrapped refusal", fmt.Errorf("handshake: %w", &mysql.MyError{Code: mysql.ER_ACCESS_DENIED_ERROR}), true},
		{"the proxy's generic error", genericAuthError(), false},
		{"bootstrap failure", &mysql.MyError{Code: mysql.ER_UNKNOWN_ERROR, Message: bootstrapClientMessage(nil)}, false},
		{"client not on TLS", &mysql.MyError{Code: errSecureTransportRequired}, false},
		{"not a MySQL error", errors.New("EOF"), false},
		{"nil", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, got := LoginRefusal(c.err); got != c.want {
				t.Errorf("LoginRefusal = %v, want %v", got, c.want)
			}
		})
	}
}

// TestEdgeUnusualNamesAndPasswords: quoting, spaces, non-ASCII and extreme
// lengths in user names and passwords all arrive at the database intact.
func TestEdgeUnusualNamesAndPasswords(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	proxy := startProxy(t, db, boot, 10*time.Second)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
	for _, c := range []struct{ label, name, password string }{
		{"quotes", "o'brien_" + suffix, `p'w"x\y` + "`z"},
		{"spaces", "user name " + suffix, "pass word  with  spaces"},
		{"non-ASCII", "ünï_" + suffix, "pässwörd-é-日本語"},
		{"longest user name (32)", "u" + strings.Repeat("a", 31-len(suffix)) + suffix, strings.Repeat("p", 64)},
		{"empty password", "nopw_" + suffix, ""},
		{"long password", "longpw_" + suffix, strings.Repeat("x9", 100)},
		{"percent and underscore", "50%_off_" + suffix, "100%_sure"},
	} {
		t.Run(c.label, func(t *testing.T) {
			acct := db.newCustomAccount(t, c.name, c.password, "", true)
			conn, err := dial(proxy.addr, acct, acct.password, true)
			if err != nil {
				t.Fatalf("login as %q: %v", c.name, err)
			}
			defer conn.Close()

			res, err := conn.Execute("SELECT CURRENT_USER()")
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := res.GetString(0, 0); got != c.name+"@%" {
				t.Errorf("CURRENT_USER() = %q, want %q", got, c.name+"@%")
			}
			if _, err := conn.Execute("SELECT COUNT(*) FROM auditproxy_pt.customers"); err != nil {
				t.Errorf("granted table: %v", err)
			}
		})
	}
	proxy.assertNoSecrets(t, db)
}

// TestEdgeInvalidUserNames: a name the database refuses and a name too long
// for the proxy to send are both refused cleanly, without revealing the
// bootstrap account and without a panic.
func TestEdgeInvalidUserNames(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	proxy := startProxy(t, db, boot, 10*time.Second)

	t.Run("longer than MySQL allows", func(t *testing.T) {
		_, err := dial(proxy.addr, account{name: strings.Repeat("n", 40)}, "whatever", true)
		if err == nil {
			t.Fatal("login with a 40-character user name succeeded")
		}
		if strings.Contains(err.Error(), boot.name) {
			t.Errorf("the error names the bootstrap account: %v", err)
		}
	})

	t.Run("longer than the proxy will send", func(t *testing.T) {
		before := len(proxy.auth.auditedRejections())
		_, err := dial(proxy.addr, account{name: strings.Repeat("n", 300)}, "whatever", true)
		me := asMyError(err)
		if me == nil || me.Code != mysql.ER_UNKNOWN_ERROR {
			t.Fatalf("got %v, want the proxy's generic error (1105)", err)
		}
		if strings.Contains(me.Message, boot.name) {
			t.Errorf("the error names the bootstrap account: %q", me.Message)
		}
		if got := len(proxy.auth.auditedRejections()); got != before {
			t.Errorf("an oversize name was audited as a failed login (%d new auth_fail events)", got-before)
		}
	})
}

// TestEdgeDatabaseRefusalsReachTheClientAndAreAudited: whatever reason the
// database gives for refusing a login is the client's error and an auth_fail.
func TestEdgeDatabaseRefusalsReachTheClientAndAreAudited(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)

	t.Run("locked account", func(t *testing.T) {
		proxy := startProxy(t, db, boot, 10*time.Second)
		acct := db.newCustomAccount(t, fmt.Sprintf("locked_%d", time.Now().UnixNano()%1_000_000), "pw-locked-1", " ACCOUNT LOCK", true)
		_, err := dial(proxy.addr, acct, acct.password, true)
		me := asMyError(err)
		if me == nil || me.Code != errAccountLocked {
			t.Fatalf("got %v, want error %d from the database", err, errAccountLocked)
		}
		if rej := proxy.auth.auditedRejections(); len(rej) != 1 || rej[0].Code != errAccountLocked {
			t.Errorf("audited rejections = %v, want exactly one with code %d", rej, errAccountLocked)
		}
	})

	t.Run("certificate required", func(t *testing.T) {
		proxy := startProxy(t, db, boot, 10*time.Second)
		acct := db.newCustomAccount(t, fmt.Sprintf("x509_%d", time.Now().UnixNano()%1_000_000), "pw-x509-1", " REQUIRE X509", true)
		_, err := dial(proxy.addr, acct, acct.password, true)
		me := asMyError(err)
		if me == nil {
			t.Fatalf("got %v, want the database's refusal", err)
		}
		if len(proxy.auth.auditedRejections()) != 1 {
			t.Errorf("the refusal (%v) was not audited", me)
		}
	})

	t.Run("too many sessions for the user", func(t *testing.T) {
		proxy := startProxy(t, db, boot, 10*time.Second)
		acct := db.newCustomAccount(t, fmt.Sprintf("limit_%d", time.Now().UnixNano()%1_000_000), "pw-limit-1", " WITH MAX_USER_CONNECTIONS 1", true)

		first, err := dial(proxy.addr, acct, acct.password, true)
		if err != nil {
			t.Fatalf("first login: %v", err)
		}
		defer first.Close()

		_, err = dial(proxy.addr, acct, acct.password, true)
		if me := asMyError(err); me == nil || me.Code != mysql.ER_USER_LIMIT_REACHED {
			t.Fatalf("second login: got %v, want error %d from the database", err, mysql.ER_USER_LIMIT_REACHED)
		}
		// The refusal must not have disturbed the first session.
		if _, err := first.Execute("SELECT 1"); err != nil {
			t.Errorf("first session after the refusal: %v", err)
		}
		first.Close()
		db.waitForNoSessions(t, "after the first session closed", acct.name)
		again, err := dial(proxy.addr, acct, acct.password, true)
		if err != nil {
			t.Fatalf("login after the first session closed: %v", err)
		}
		again.Close()
	})

	t.Run("expired password", func(t *testing.T) {
		proxy := startProxy(t, db, boot, 10*time.Second)
		acct := db.newCustomAccount(t, fmt.Sprintf("expired_%d", time.Now().UnixNano()%1_000_000), "pw-expired-1", " PASSWORD EXPIRE", true)
		conn, err := dial(proxy.addr, acct, acct.password, true)
		if err != nil {
			// The database refuses a client that cannot change the password.
			if me := asMyError(err); me == nil || me.Code != errMustChangePass {
				t.Fatalf("got %v, want error %d from the database", err, errMustChangePass)
			}
			return
		}
		defer conn.Close()
		// Or it lets the session in "sandbox" mode, where statements fail.
		_, err = conn.Execute("SELECT 1")
		if me := asMyError(err); me == nil || me.Code != errPassExpiredCmd {
			t.Errorf("statement as an expired user: got %v, want error %d", err, errPassExpiredCmd)
		}
	})
}

// rawLoginThenDrop is a client that starts a TLS login and walks away when the
// proxy challenges it: it returns once the proxy has sent something back.
func rawLoginThenDrop(addr, user string) error {
	raw, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))

	pc := packet.NewConn(raw)
	if _, err := pc.ReadPacket(); err != nil { // the greeting
		return err
	}
	const caps = mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SSL
	header := func() []byte {
		b := make([]byte, 4+32)
		binary.LittleEndian.PutUint32(b[4:], caps)
		b[4+8] = mysql.DEFAULT_COLLATION_ID
		return b
	}
	if err := pc.WritePacket(header()); err != nil { // SSL request
		return err
	}
	tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		return err
	}
	pc2 := packet.NewConn(tc)
	pc2.Sequence = pc.Sequence
	resp := append(header(), user...)
	resp = append(resp, 0, 0)
	resp = append(resp, mysql.AUTH_CACHING_SHA2_PASSWORD...)
	resp = append(resp, 0)
	if err := pc2.WritePacket(resp); err != nil {
		return err
	}
	_, err = pc2.ReadPacket() // the database's challenge, relayed
	return err
}

// TestEdgeNoUpstreamConnectionLeaks: every way a login can end leaves no
// session open on the database. A leak here is a connection per attempt.
func TestEdgeNoUpstreamConnectionLeaks(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	acct := db.newAccount(t, "", true)
	proxy := startProxy(t, db, boot, 10*time.Second)

	for i := 0; i < 15; i++ {
		if _, err := dial(proxy.addr, acct, "wrong-password", true); err == nil {
			t.Fatal("a wrong password was accepted")
		}
	}
	db.waitForNoSessions(t, "after wrong passwords", boot.name, acct.name)

	for i := 0; i < 15; i++ {
		conn, err := dial(proxy.addr, acct, acct.password, true)
		if err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
		if _, err := conn.Execute("SELECT 1"); err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	db.waitForNoSessions(t, "after logins that closed normally", boot.name, acct.name)

	for i := 0; i < 10; i++ {
		raw, err := net.DialTimeout("tcp", proxy.addr, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		raw.Close() // connect and leave, like a health check
	}
	for i := 0; i < 10; i++ {
		_ = rawLoginThenDrop(proxy.addr, acct.name)
	}
	db.waitForNoSessions(t, "after clients that walked away mid-login", boot.name, acct.name)

	proxy.assertNoSecrets(t, db)
}

// TestEdgeConcurrentLoginsKeepTheirOwnIdentity: many simultaneous logins, some
// wrong, never see each other's identity.
func TestEdgeConcurrentLoginsKeepTheirOwnIdentity(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	proxy := startProxy(t, db, boot, 10*time.Second)

	accts := []account{
		db.newAccount(t, "", true),
		db.newAccount(t, mysql.AUTH_NATIVE_PASSWORD, true),
		db.newAccount(t, mysql.AUTH_SHA256_PASSWORD, true),
	}

	var wg sync.WaitGroup
	for _, a := range accts {
		for worker := 0; worker < 8; worker++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				conn, err := dial(proxy.addr, a, a.password, true)
				if err != nil {
					t.Errorf("%s: login: %v", a.name, err)
					return
				}
				defer conn.Close()
				for i := 0; i < 20; i++ {
					res, err := conn.Execute("SELECT CURRENT_USER()")
					if err != nil {
						t.Errorf("%s: query: %v", a.name, err)
						return
					}
					if got, _ := res.GetString(0, 0); got != a.name+"@%" {
						t.Errorf("a session logged in as %s ran as %q", a.name, got)
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				if conn, err := dial(proxy.addr, a, "not-the-password", true); err == nil {
					conn.Close()
					t.Errorf("%s: a wrong password was accepted", a.name)
				}
			}()
		}
	}
	wg.Wait()
	db.waitForNoSessions(t, "after the concurrent logins", append([]string{boot.name}, accts[0].name, accts[1].name, accts[2].name)...)
}

// TestEdgeClientCannotChangeUserAfterLogin: a client that sends its own
// COM_CHANGE_USER on a logged-in session is not switched to another identity.
// The proxy relays only what it understands; this is a command it does not.
func TestEdgeClientCannotChangeUserAfterLogin(t *testing.T) {
	db := newLiveDB(t)
	boot := db.newAccount(t, "", false)
	proxy := startProxy(t, db, boot, 10*time.Second)

	alice := db.newAccount(t, "", true)
	other := db.newAccount(t, "", true)

	conn, err := dial(proxy.addr, alice, alice.password, true)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	payload, err := buildChangeUser(changeUserCaps, other.name)
	if err != nil {
		t.Fatal(err)
	}
	conn.Conn.ResetSequence()
	if err := conn.Conn.WritePacket(payload); err != nil {
		t.Fatal(err)
	}
	reply, err := conn.Conn.ReadPacket()
	if err == nil && len(reply) > 0 && (reply[0] == mysql.OK_HEADER || reply[0] == mysql.EOF_HEADER) {
		t.Fatalf("the proxy answered a client's COM_CHANGE_USER with %#x, not an error", reply[0])
	}

	// If the session survived, it must still be alice.
	if res, err := conn.Execute("SELECT CURRENT_USER()"); err == nil {
		if got, _ := res.GetString(0, 0); got != alice.name+"@%" {
			t.Errorf("after COM_CHANGE_USER the session runs as %q, want %q", got, alice.name+"@%")
		}
	}
}
