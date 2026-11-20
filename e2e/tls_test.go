package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/passthrough"
	secureupstream "github.com/tevans2/mysql-audit-proxy/internal/upstream"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// TestUpstreamTLS is opt-in locally and mandatory under make test-tls. It uses
// real MySQL servers, including a separately started --skip-ssl instance.
func TestUpstreamTLS(t *testing.T) {
	if os.Getenv("TLS_E2E_REQUIRE") == "" {
		t.Skip("run make test-tls for the mandatory TLS acceptance suite")
	}
	ca := os.Getenv("TLS_E2E_CA_FILE")
	password := os.Getenv("TLS_TEST_PASSWORD")
	if ca == "" || password == "" {
		t.Fatal("TLS_E2E_CA_FILE and TLS_TEST_PASSWORD are required")
	}
	if _, err := os.Stat(ca); err != nil {
		t.Fatal(err)
	}
	a := upstream{name: "db-a", addr: getEnv("TLS_E2E_DB_A_ADDR", "127.0.0.1:23306"), user: "root", pass: password, directParams: "tls=skip-verify"}
	b := upstream{name: "db-b", addr: getEnv("TLS_E2E_DB_B_ADDR", "127.0.0.1:23316"), user: "root", pass: password, directParams: "tls=skip-verify"}
	disabled := upstream{name: "tls-disabled", addr: getEnv("TLS_E2E_DISABLED_ADDR", "127.0.0.1:23326"), user: "root", pass: password}
	for _, u := range []upstream{a, b, disabled} {
		c, err := net.DialTimeout("tcp", u.addr, time.Second)
		if err != nil {
			t.Fatalf("required fixture %s: %v", u.name, err)
		}
		c.Close()
	}
	// Pass-through authentication needs the bootstrap account and the client
	// accounts to exist on both TLS databases. Their passwords are derived from
	// the fixture password, which is never a literal in the repository.
	t.Setenv("E2E_DB_A_ALICE_PASS", "alice-"+password)
	t.Setenv("E2E_DB_B_BOB_PASS", "bob-"+password)
	t.Setenv("E2E_DB_A_BOOTSTRAP_PASS", "boot-a-"+password)
	t.Setenv("E2E_DB_B_BOOTSTRAP_PASS", "boot-b-"+password)
	for _, u := range []upstream{a, b} {
		ensureAccounts(t, u, accountsFor(t, u))
	}
	t.Setenv("E2E_TLS", "verify")
	t.Setenv("E2E_TLS_CA_FILE", ca)
	t.Run("API configuration verifies both routes", func(t *testing.T) {
		s := startStack(t, a, b)
		for _, u := range []upstream{a, b} {
			db := s.openThroughProxy(t, u.name, aliceUser, alicePass(t), "")
			var name, cipher string
			if err := db.QueryRow("SHOW STATUS LIKE 'Ssl_cipher'").Scan(&name, &cipher); err != nil {
				t.Fatal(err)
			}
			if cipher == "" {
				t.Fatalf("%s upstream is plaintext", u.name)
			}
			t.Logf("%s cipher: %s", u.name, cipher)
		}
	})
	proxyBin, _ := buildBinaries(t)
	for _, tc := range []struct {
		name     string
		u        upstream
		mode, ca string
		fail     bool
	}{
		{"file trusted db-a", a, "verify", ca, false},
		{"file trusted db-b", b, "verify", ca, false},
		{"untrusted certificate", a, "verify", "", true},
		{"TLS disabled verify", disabled, "verify", ca, true},
		{"TLS disabled skip-verify", disabled, "skip-verify", "", true},
		{"development skip-verify", a, "skip-verify", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, portText, _ := net.SplitHostPort(tc.u.addr)
			port, _ := strconv.Atoi(portText)
			listen := freePort(t)
			cfg := struct {
				Servers []audit.Server `json:"servers"`
			}{[]audit.Server{{Name: tc.u.name, ListenPort: listen, UpstreamHost: host, UpstreamPort: port, UpstreamUser: bootstrapUser, Enabled: true, TLS: tc.mode, TLSCAFile: tc.ca}}}
			data, _ := json.Marshal(cfg)
			path := filepath.Join(t.TempDir(), "upstreams.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			start(t, "TLS proxy", proxyBin, []string{"PROXY_CONFIG_FILE=" + path, "PROXY_UPSTREAM_PASSWORDS=" + tc.u.name + ":" + bootstrapPass(t, tc.u), "SINK_INGEST_URL="})
			addr := fmt.Sprintf("127.0.0.1:%d", listen)
			waitTCP(t, "TLS proxy", addr)
			db, err := sql.Open("mysql", aliceUser+":"+alicePass(t)+"@tcp("+addr+")/?tls=skip-verify&timeout=3s&readTimeout=3s")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var name, cipher string
			err = db.QueryRow("SHOW STATUS LIKE 'Ssl_cipher'").Scan(&name, &cipher)
			if tc.fail {
				if err == nil {
					t.Fatal("unsafe upstream unexpectedly accepted")
				}
				if !strings.Contains(err.Error(), "Upstream") || !strings.Contains(err.Error(), "TLS") {
					t.Fatalf("unclear failure: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if cipher == "" {
					t.Fatal("plaintext upstream")
				}
			}
		})
	}
	t.Run("prototype bootstrap and change user", func(t *testing.T) {
		for _, u := range []upstream{a, b} {
			t.Run(u.name, func(t *testing.T) { testTLSChangeUser(t, u, ca) })
		}
	})
}

// passwordRelay stands in for a TLS client in the prototype integration test.
// Only this test client computes responses; the production relay forwards bytes.
type passwordRelay struct {
	password string
	full     bool
}

func (*passwordRelay) ClientOnTLS() bool { return true }
func (r *passwordRelay) Challenge(plugin string, salt []byte) ([]byte, error) {
	switch plugin {
	case mysql.AUTH_NATIVE_PASSWORD:
		return mysql.CalcNativePassword(salt, []byte(r.password)), nil
	case mysql.AUTH_CACHING_SHA2_PASSWORD:
		return mysql.CalcCachingSha2Password(salt, []byte(r.password)), nil
	case mysql.AUTH_SHA256_PASSWORD:
		return append([]byte(r.password), 0), nil
	default:
		return nil, fmt.Errorf("unexpected plugin %q", plugin)
	}
}
func (*passwordRelay) Notify([]byte) error { return nil }
func (*passwordRelay) Log(string)          {}
func (r *passwordRelay) Relay(payload []byte) ([]byte, error) {
	if len(payload) != 1 || payload[0] != mysql.CACHE_SHA2_FULL_AUTH {
		return nil, fmt.Errorf("unexpected auth request")
	}
	r.full = true
	return append([]byte(r.password), 0), nil
}

func testTLSChangeUser(t *testing.T, u upstream, ca string) {
	host, _, _ := net.SplitHostPort(u.addr)
	cfg, err := secureupstream.TLSConfig(host, "verify", ca)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := secureupstream.Connect(context.Background(), u.addr, u.user, u.pass, cfg, 3*time.Second, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	bootstrap := "boot_" + suffix
	password := "test_" + suffix
	if _, err := admin.Execute(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", bootstrap, password)); err != nil {
		t.Fatal(err)
	}
	defer admin.Execute(fmt.Sprintf("DROP USER '%s'@'%%'", bootstrap))
	boot, err := secureupstream.Connect(context.Background(), u.addr, bootstrap, password, cfg, 3*time.Second, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := boot.Execute("SHOW STATUS LIKE 'Ssl_cipher'")
	if err != nil {
		t.Fatal(err)
	}
	cipher, _ := result.GetString(0, 1)
	result.Close()
	boot.Close()
	if cipher == "" {
		t.Fatal("bootstrap was plaintext")
	}
	for _, plugin := range []string{mysql.AUTH_NATIVE_PASSWORD, mysql.AUTH_CACHING_SHA2_PASSWORD, mysql.AUTH_SHA256_PASSWORD} {
		t.Run(plugin, func(t *testing.T) {
			user := "user_" + strconv.FormatInt(time.Now().UnixNano(), 36)
			if _, err := admin.Execute(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED WITH %s BY '%s'", user, plugin, password)); err != nil {
				t.Fatal(err)
			}
			defer admin.Execute(fmt.Sprintf("DROP USER '%s'@'%%'", user))
			relay := &passwordRelay{password: password}
			conn, err := passthrough.DialChangeUser(context.Background(), u.addr, bootstrap, password, user, relay, 3*time.Second, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if plugin == mysql.AUTH_CACHING_SHA2_PASSWORD && !relay.full {
				t.Fatal("cold-cache full auth was not exercised")
			}
			res, err := conn.Execute("SHOW STATUS LIKE 'Ssl_cipher'")
			if err != nil {
				t.Fatal(err)
			}
			cipher, _ := res.GetString(0, 1)
			res.Close()
			if cipher == "" {
				t.Fatal("TLS lost across COM_CHANGE_USER")
			}
			res, err = conn.Execute("SELECT CURRENT_USER()")
			if err != nil {
				t.Fatal(err)
			}
			identity, _ := res.GetString(0, 0)
			res.Close()
			if identity != user+"@%" {
				t.Fatalf("identity %q is not the requested user", identity)
			}
		})
	}
}
