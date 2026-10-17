package e2e

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// Per-user accounts (ADR 0004). deploy/initdb/*/03-users.sh creates them for the
// compose stack; CI attaches bare MySQL containers with nowhere to run that
// script, so ensureAccounts establishes the same accounts here, as root.
//
// The passwords are never literals: `make test-e2e` reads them from deploy/.env
// and CI generates them per job. Without them the test skips, or fails under
// E2E_REQUIRE, like an unreachable upstream.

const (
	bootstrapUser = "proxy_bootstrap"
	aliceUser     = "alice"
	bobUser       = "bob"
)

// alicePass is alice's password on BOTH upstreams. She exists on both so a
// routing change (a PUT repointing a port at the other database) leaves her able
// to log in: with pass-through authentication the database, not the proxy,
// decides who may connect, so a user has to exist where the proxy sends her.
func alicePass(t *testing.T) string { return accountPass(t, "E2E_DB_A_ALICE_PASS") }

// bobPass is bob's password, on db-b.
func bobPass(t *testing.T) string { return accountPass(t, "E2E_DB_B_BOB_PASS") }

// bootstrapPass is the password of the proxy's own login on u: a database
// account with USAGE and nothing else (docs/design/decisions/0004).
func bootstrapPass(t *testing.T, u upstream) string {
	t.Helper()
	if u.name == "db-b" {
		return accountPass(t, "E2E_DB_B_BOOTSTRAP_PASS")
	}
	return accountPass(t, "E2E_DB_A_BOOTSTRAP_PASS")
}

// account is one MySQL user, its grants, and what it should and should not be
// able to run.
type account struct {
	user string
	pass string
	// grants are GRANT statements with a %s where the user goes.
	grants []string
	// allowed and denied are queries run as the account directly.
	allowed []string
	denied  []string
}

func accountPass(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		msg := fmt.Sprintf("%s not set (see deploy/.env.example; `make test-e2e` sets it)", key)
		if os.Getenv("E2E_REQUIRE") != "" {
			t.Fatalf("E2E_REQUIRE is set: %s", msg)
		}
		t.Skip(msg)
	}
	return v
}

// accountsFor returns the accounts expected on an upstream.
func accountsFor(t *testing.T, u upstream) []account {
	t.Helper()
	marker := "SELECT name FROM lustre_test.upstream_marker"
	boot := account{
		user:    bootstrapUser,
		pass:    bootstrapPass(t, u),
		grants:  []string{"GRANT USAGE ON *.* TO %s"},
		allowed: []string{"SELECT 1"},
		denied:  []string{"SELECT COUNT(*) FROM " + u.ownTable, marker},
	}
	switch u.name {
	case "db-b":
		return []account{{
			user:    aliceUser,
			pass:    alicePass(t),
			grants:  []string{"GRANT SELECT ON hr.* TO %s", "GRANT SELECT ON lustre_test.upstream_marker TO %s"},
			allowed: []string{"SELECT COUNT(*) FROM hr.employees", marker},
			denied:  []string{"SELECT COUNT(*) FROM mysql.user"},
		}, {
			user:    bobUser,
			pass:    bobPass(t),
			grants:  []string{"GRANT SELECT ON hr.* TO %s", "GRANT SELECT ON lustre_test.upstream_marker TO %s"},
			allowed: []string{"SELECT COUNT(*) FROM hr.employees", marker},
			denied:  []string{"SELECT COUNT(*) FROM mysql.user"},
		}, boot}
	default:
		return []account{{
			user: aliceUser,
			pass: alicePass(t),
			// sales.* rather than one table, so the performance tests can read
			// the table they seed as alice.
			grants:  []string{"GRANT SELECT ON sales.* TO %s", "GRANT SELECT ON lustre_test.upstream_marker TO %s"},
			allowed: []string{"SELECT COUNT(*) FROM sales.customers", marker},
			denied:  []string{"SELECT COUNT(*) FROM mysql.user"},
		}, boot}
	}
}

// ensureAccounts creates each account with exactly its grants, dropping any
// earlier version first so a rerun against a stale server cannot leave extra
// privileges behind.
func ensureAccounts(t *testing.T, u upstream, accts []account) {
	t.Helper()
	root := openDirect(t, u, u.user, u.pass)
	defer root.Close()
	// The schemas and marker must exist before they can be granted on.
	seedUpstream(t, u)

	for _, a := range accts {
		who := fmt.Sprintf("'%s'@'%%'", a.user)
		stmts := []string{
			"DROP USER IF EXISTS " + who,
			fmt.Sprintf("CREATE USER %s IDENTIFIED WITH caching_sha2_password BY '%s'", who, a.pass),
		}
		for _, g := range a.grants {
			stmts = append(stmts, fmt.Sprintf(g, who))
		}
		for _, q := range stmts {
			if _, err := root.Exec(q); err != nil {
				t.Fatalf("%s: preparing account %s: %v", u.name, a.user, err)
			}
		}
	}
}

func openDirect(t *testing.T, u upstream, user, pass string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s%s", user, pass, u.addr, u.extraParams()))
	if err != nil {
		t.Fatalf("opening %s as %s: %v", u.name, user, err)
	}
	return db
}

// TestAccountsGetExactlyTheirGrants logs in to each upstream directly as each
// account and checks what MySQL allows and refuses. Direct, not through the
// proxy: this is the baseline pass-through authentication is measured against.
func TestAccountsGetExactlyTheirGrants(t *testing.T) {
	a, b := upstreams(t)
	for _, u := range []upstream{a, b} {
		accts := accountsFor(t, u)
		ensureAccounts(t, u, accts)

		for _, acct := range accts {
			t.Run(u.name+"/"+acct.user, func(t *testing.T) {
				db := openDirect(t, u, acct.user, acct.pass)
				defer db.Close()

				var who string
				if err := db.QueryRow("SELECT CURRENT_USER()").Scan(&who); err != nil {
					t.Fatalf("logging in as %s: %v", acct.user, err)
				}
				if want := acct.user + "@%"; who != want {
					t.Errorf("CURRENT_USER() = %q, want %q", who, want)
				}

				for _, q := range acct.allowed {
					rows, err := db.Query(q)
					if err != nil {
						t.Errorf("%q: want allowed, got %v", q, err)
						continue
					}
					rows.Close()
				}
				for _, q := range acct.denied {
					rows, err := db.Query(q)
					if err == nil {
						rows.Close()
						t.Errorf("%q: succeeded, want 1142", q)
						continue
					}
					var me *mysql.MySQLError
					if !errors.As(err, &me) || me.Number != 1142 {
						t.Errorf("%q: got %v, want error 1142", q, err)
					}
				}
			})
		}
	}
}
