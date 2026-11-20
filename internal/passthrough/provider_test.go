package passthrough

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-mysql-org/go-mysql/mysql"
)

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logCapture) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func TestProviderValidatesTheDefaultPlugin(t *testing.T) {
	p := NewProvider(Config{})
	for _, plugin := range []string{mysql.AUTH_NATIVE_PASSWORD, mysql.AUTH_CACHING_SHA2_PASSWORD, mysql.AUTH_SHA256_PASSWORD} {
		if !p.Validate(plugin) {
			t.Errorf("Validate(%q) = false; go-mysql would panic if this is the default plugin", plugin)
		}
	}
	if p.Validate(mysql.AUTH_CLEAR_PASSWORD) || p.Validate("") {
		t.Error("accepted a plugin the proxy does not offer")
	}
}

func TestCredentialsFindEveryoneButNobody(t *testing.T) {
	cred, found, err := Credentials{}.GetCredential("alice")
	if err != nil || !found {
		t.Fatalf("alice: found=%v err=%v", found, err)
	}
	if cred.AuthPluginName != mysql.AUTH_CACHING_SHA2_PASSWORD || len(cred.Passwords) == 0 {
		t.Fatalf("unexpected placeholder credential: %+v", cred)
	}
	if _, found, _ := (Credentials{}).GetCredential(""); found {
		t.Fatal("an empty user name was accepted")
	}
}

func TestFailClassification(t *testing.T) {
	logs := &logCapture{}
	p := NewProvider(Config{Addr: "db:3306", BootstrapUser: "auditproxy_bootstrap", Logf: logs.Logf})

	t.Run("bootstrap failure is generic to the client and detailed to the operator", func(t *testing.T) {
		cause := errors.New("Access denied for user 'auditproxy_bootstrap'@'10.0.0.1' (using password: YES)")
		err := p.fail("alice", &BootstrapError{Cause: cause})
		me, ok := err.(*mysql.MyError)
		if !ok || me.Code != mysql.ER_UNKNOWN_ERROR {
			t.Fatalf("got %T %v, want the generic 1105 error", err, err)
		}
		if strings.Contains(me.Error(), "auditproxy_bootstrap") || strings.Contains(me.Error(), "Access denied") {
			t.Fatalf("client-visible error leaks the bootstrap account: %q", me.Error())
		}
		if !strings.Contains(logs.text(), "auditproxy_bootstrap") {
			t.Fatalf("operator log lost the bootstrap detail: %q", logs.text())
		}
	})

	t.Run("TLS problems are named without naming the account", func(t *testing.T) {
		for cause, want := range map[error]string{
			&tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}: "certificate verification failed",
			errors.New("readInitialHandshake: the MySQL Server does not support TLS required by the client"):    "does not offer TLS",
		} {
			err := p.fail("alice", &BootstrapError{Cause: fmt.Errorf("upstream TLS connection failed: %w", cause)})
			me, ok := err.(*mysql.MyError)
			if !ok || me.Code != mysql.ER_UNKNOWN_ERROR || !strings.Contains(me.Message, want) {
				t.Errorf("cause %v: got %v, want a message containing %q", cause, err, want)
			}
			if strings.Contains(me.Message, "auditproxy_bootstrap") {
				t.Errorf("message names the bootstrap account: %q", me.Message)
			}
		}
	})

	t.Run("a database refusal passes through unchanged", func(t *testing.T) {
		refusal := &mysql.MyError{Code: mysql.ER_ACCESS_DENIED_ERROR, State: "28000", Message: "Access denied for user 'alice'"}
		if got := p.fail("alice", fmt.Errorf("wrapped: %w", refusal)); got != error(refusal) {
			t.Fatalf("got %v, want the very same *mysql.MyError (go-mysql type-asserts it, so it must not be wrapped)", got)
		}
	})

	t.Run("anything else is generic", func(t *testing.T) {
		err := p.fail("alice", errors.New("read client auth response: i/o timeout"))
		me, ok := err.(*mysql.MyError)
		if !ok || me.Code != mysql.ER_UNKNOWN_ERROR || strings.Contains(me.Message, "i/o") {
			t.Fatalf("internal detail reached the client: %v", err)
		}
	})
}

func TestNilLogfIsSafe(t *testing.T) {
	p := NewProvider(Config{})
	_ = p.fail("alice", errors.New("boom"))
	p.Close()
	p.Close() // closing twice, with nothing open, must not panic
	if p.Conn() != nil || p.Release() != nil {
		t.Fatal("a provider that never logged anyone in holds a connection")
	}
}
