package e2e

import (
	"fmt"
	"testing"
	"time"

	"lustre/internal/audit"
)

// markerQuery reads the routing marker. Both upstreams answer it; the value
// names whichever one actually served the query.
const markerQuery = "SELECT name FROM lustre_test.upstream_marker"

// TestEachProxyPortReachesItsOwnUpstream is the acceptance test for the
// multi-upstream environment: several databases, each reachable through its own
// proxy port.
//
// The same query on two ports must give two different answers. If a port were
// wired to the wrong upstream — or both to the same one — this is what notices.
func TestEachProxyPortReachesItsOwnUpstream(t *testing.T) {
	a, b := upstreams(t)
	seedUpstream(t, a)
	seedUpstream(t, b)

	s := startStack(t, a, b)

	for _, u := range []upstream{a, b} {
		db := s.openThroughProxy(t, u.name, aliceUser, alicePass(t), "")

		var got string
		if err := db.QueryRow(markerQuery).Scan(&got); err != nil {
			t.Fatalf("querying %s through :%d: %v", u.name, s.proxyPorts[u.name], err)
		}
		if got != u.name {
			t.Errorf("proxy port %d reached %q, want %q",
				s.proxyPorts[u.name], got, u.name)
		}
	}
}

// TestUpstreamsAreNotInterchangeable guards the property that makes the
// environment worth having: the two databases hold different schemas, so a
// misrouted query fails loudly instead of returning believable rows.
//
// If both upstreams were seeded alike, every routing test above could pass
// while the routing itself was broken.
func TestUpstreamsAreNotInterchangeable(t *testing.T) {
	a, b := upstreams(t)
	seedUpstream(t, a)
	seedUpstream(t, b)

	s := startStack(t, a, b)

	// db-a holds `sales`, db-b holds `hr`. Each must serve its own table and
	// reject the other's.
	//
	// Both halves matter. Asserting only that the foreign table fails would
	// pass just as happily against two empty databases, where nothing is
	// distinguishable at all — which is exactly the state a CI runner starts in.
	for _, c := range []struct{ own, other upstream }{{a, b}, {b, a}} {
		db := s.openThroughProxy(t, c.own.name, aliceUser, alicePass(t), "")

		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + c.own.ownTable).Scan(&n); err != nil {
			t.Errorf("%s cannot read its own table %s: %v", c.own.name, c.own.ownTable, err)
		}

		err := db.QueryRow("SELECT COUNT(*) FROM " + c.other.ownTable).Scan(&n)
		if err == nil {
			t.Errorf("%s served %s, which belongs to %s (returned %d) — the upstreams are not "+
				"distinguishable, so a routing bug would go unnoticed",
				c.own.name, c.other.ownTable, c.other.name, n)
		}
	}
}

// TestAuditTrailRecordsWhichUpstream covers the third part of the environment:
// the audit log must make it clear which database a statement reached.
//
// This is the full path — client, proxy, sink, API, store, read back over HTTP.
func TestAuditTrailRecordsWhichUpstream(t *testing.T) {
	a, b := upstreams(t)
	seedUpstream(t, a)
	seedUpstream(t, b)

	s := startStack(t, a, b)

	// A token per run makes the assertion independent of anything else in the
	// store, and names the failing query in the output when it does not arrive.
	token := fmt.Sprintf("e2e-%d", time.Now().UnixNano())

	// Same query, same user, different port. Only `upstream` can tell them apart.
	for _, u := range []upstream{a, b} {
		db := s.openThroughProxy(t, u.name, aliceUser, alicePass(t), "")
		q := fmt.Sprintf("%s /* %s %s */", markerQuery, token, u.name)
		var got string
		if err := db.QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("querying %s: %v", u.name, err)
		}
	}

	for _, u := range []upstream{a, b} {
		want := fmt.Sprintf("/* %s %s */", token, u.name)
		st := s.waitForStatement(t, want)

		if st.Upstream != u.name {
			t.Errorf("statement %q recorded upstream %q, want %q", want, st.Upstream, u.name)
		}
		if st.DBUser != "alice" {
			t.Errorf("statement %q recorded db_user %q, want %q", want, st.DBUser, "alice")
		}
		if st.StmtType != "SELECT" {
			t.Errorf("statement %q recorded stmt_type %q, want %q", want, st.StmtType, "SELECT")
		}
	}
}

// TestServersEndpointDescribesRunningStack checks that the back-office is told
// the mapping the proxies are actually routing by.
//
// It returned an empty list for the whole of the first version — the API read its config
// from a path nothing supplied — which looked exactly like "no servers
// configured" to the UI.
func TestServersEndpointDescribesRunningStack(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	var resp struct {
		Servers []audit.Server `json:"servers"`
	}
	s.getJSON(t, "/api/servers", &resp)

	if len(resp.Servers) != 2 {
		t.Fatalf("GET /api/servers returned %d servers, want 2: %+v", len(resp.Servers), resp.Servers)
	}

	byName := map[string]audit.Server{}
	for _, srv := range resp.Servers {
		byName[srv.Name] = srv
	}

	for _, u := range []upstream{a, b} {
		srv, ok := byName[u.name]
		if !ok {
			t.Errorf("GET /api/servers does not list %q", u.name)
			continue
		}
		if srv.ListenPort != s.proxyPorts[u.name] {
			t.Errorf("%s listed on port %d, but its proxy is listening on %d",
				u.name, srv.ListenPort, s.proxyPorts[u.name])
		}
		if !srv.Enabled {
			t.Errorf("%s listed as disabled", u.name)
		}
	}
}

// TestConnectionRecordsPortAndUpstream checks a session is attributed to the
// port it arrived on as well as the database it reached — the two fields the
// viewer needs to group by once there is more than one server.
func TestConnectionRecordsPortAndUpstream(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	// Different users per upstream, so the connection rows are unambiguous.
	users := map[string][2]string{
		a.name: {aliceUser, alicePass(t)},
		b.name: {bobUser, bobPass(t)},
	}
	for _, u := range []upstream{a, b} {
		cred := users[u.name]
		db := s.openThroughProxy(t, u.name, cred[0], cred[1], "")
		if err := db.Ping(); err != nil {
			t.Fatalf("connecting to %s: %v", u.name, err)
		}
	}

	for _, u := range []upstream{a, b} {
		cred := users[u.name]
		conn := s.waitForConnection(t, cred[0])

		if conn.Upstream != u.name {
			t.Errorf("%s's connection recorded upstream %q, want %q", cred[0], conn.Upstream, u.name)
		}
		if conn.ProxyPort != s.proxyPorts[u.name] {
			t.Errorf("%s's connection recorded proxy_port %d, want %d",
				cred[0], conn.ProxyPort, s.proxyPorts[u.name])
		}
		if conn.AuthResult != "ok" {
			t.Errorf("%s's connection recorded auth_result %q, want %q", cred[0], conn.AuthResult, "ok")
		}
	}
}
