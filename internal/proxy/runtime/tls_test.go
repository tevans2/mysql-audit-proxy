package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	sqldriver "github.com/go-sql-driver/mysql"

	"lustre/internal/audit"
	"lustre/internal/proxy/config"
)

// TestVerify_PlaintextClientIsRefused connects without TLS. The proxy relays
// the client's password answer to the database as it is, which is only safe
// on a TLS connection, so a plaintext login is refused before the database is
// contacted, with MySQL's own "secure transport required" error (3159). It
// must not be audited as a failed login: nobody got a password wrong.
func TestVerify_PlaintextClientIsRefused(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, sink := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	dsn := fmt.Sprintf("demo:demopass@tcp(%s)/demo?timeout=5s&tls=false", proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	err = db.Ping()
	var mysqlErr *sqldriver.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != 3159 {
		t.Fatalf("plaintext login: got %v, want MySQL error 3159", err)
	}
	waitAndAssertNoAuthFail(t, sink, 500*time.Millisecond)
}

// handshakeCapabilityFlags dials proxyAddr directly and parses just enough of
// the raw initial handshake packet (protocol_connection_phase_packets ::
// Handshake, v10) to return the server's advertised capability flags —
// the 4 bytes split either side of the charset/status fields, per
// server.Conn.writeInitialHandshake. Everything before the capability flags
// (server version, connection id, first half of the salt) is variable-length
// or random, so this locates them by finding the NUL that terminates the
// server-version string rather than assuming fixed offsets from packet start.
func handshakeCapabilityFlags(t *testing.T, proxyAddr string) uint32 {
	t.Helper()

	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("read packet header: %v", err)
	}
	length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read handshake payload: %v", err)
	}

	// payload[0] is the protocol version (10); the NUL-terminated server
	// version string starts right after it.
	if len(payload) < 1 {
		t.Fatalf("handshake payload too short: %x", payload)
	}
	nul := bytes.IndexByte(payload[1:], 0x00)
	if nul == -1 {
		t.Fatalf("no NUL terminator found for server version string: %x", payload)
	}
	verEnd := 1 + nul // index of the terminating NUL itself

	// verEnd+1 : connection id (4 bytes)
	// verEnd+5 : salt part 1 (8 bytes)
	// verEnd+13: filter byte (0x00)
	// verEnd+14: capability flags, lower 2 bytes
	// verEnd+16: charset (1 byte)
	// verEnd+17: status (2 bytes)
	// verEnd+19: capability flags, upper 2 bytes
	capUpperEnd := verEnd + 21
	if len(payload) < capUpperEnd {
		t.Fatalf("handshake payload too short to hold capability flags: %x", payload)
	}
	capLower := payload[verEnd+14 : verEnd+16]
	capUpper := payload[verEnd+19 : verEnd+21]

	return uint32(capLower[0]) | uint32(capLower[1])<<8 |
		uint32(capUpper[0])<<16 | uint32(capUpper[1])<<24
}

// TestVerify_OffersClientSSL checks the initial handshake packet advertises
// CLIENT_SSL, the capability flag that tells a client TLS is available at
// all. go-mysql only sets this bit when server.NewServer is given a non-nil
// *tls.Config — the earlier caching_sha2_password fix passed nil there, so a
// default client (ssl-mode=PREFERRED) had nothing to negotiate and fell back
// to the RSA-consent exchange, which it refuses on a plaintext connection:
//
//	ERROR 2061 (HY000): Authentication plugin 'caching_sha2_password' reported error: Authentication requires secure connection.
//
// The earlier caching_sha2_password tests in this file all go through
// go-sql-driver/mysql, which requests the server's RSA public key
// automatically regardless of whether TLS is offered — so none of them would
// have caught that regression. This test reads the raw handshake packet
// instead, the same way TestVerify_OffersCachingSha2PasswordByDefault checks
// the advertised auth plugin, so it fails the same way a stock client's
// negotiation logic would: on what the server actually offers, not on
// whether a permissive driver worked around the gap.
func TestVerify_OffersClientSSL(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Dialing the proxy directly and reading the initial handshake packet")
	capFlags := handshakeCapabilityFlags(t, proxyAddr)

	t.Log("Step 2: Confirming CLIENT_SSL is advertised")
	if capFlags&mysql.CLIENT_SSL == 0 {
		t.Errorf("initial handshake packet does not advertise CLIENT_SSL (capability flags = %#x) — "+
			"a default client (ssl-mode=PREFERRED) has nothing to negotiate TLS with and falls back "+
			"to the caching_sha2_password RSA-consent exchange, which it refuses on a plaintext "+
			"connection with ERROR 2061", capFlags)
	}
}

// TestVerify_CachingSha2PasswordTLSLoginSucceeds connects with tls=skip-verify
// — the same TLS-but-don't-check-the-cert posture a default client's
// ssl-mode=PREFERRED takes against a self-signed certificate it has no
// reason to trust — on a brand new proxy instance (a cache miss, forcing
// the full caching_sha2_password exchange to actually happen).
//
// This is the direct regression test for the scenario the code review
// raised: proving a client that negotiates TLS (rather than requesting the
// RSA public key over plaintext, which every earlier test in this file
// relies on go-sql-driver doing automatically) can still complete a fresh
// caching_sha2_password login end to end, self-signed certificate and all.
func TestVerify_CachingSha2PasswordTLSLoginSucceeds(t *testing.T) {
	upstreamAddr, upstreamUser, upstreamPass := testUpstreamConfig(t)
	proxyAddr, _ := startTestProxy(t, upstreamAddr, upstreamUser, upstreamPass)

	t.Log("Step 1: Connecting as demo/demopass over TLS (tls=skip-verify) on a fresh proxy instance")
	dsn := proxyDSN("demo", "demopass", proxyAddr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		t.Fatalf("Ping over TLS against the proxy's self-signed certificate failed: %v", err)
	}
}

func TestUpstreamTLSStartup(t *testing.T) {
	var logs bytes.Buffer
	p := &Proxy{UpstreamAddr: "db-a:3306", UpstreamName: "db-a", Logger: log.New(&logs, "", 0)}
	if err := p.configureUpstreamTLS(); err != nil {
		t.Fatal(err)
	}
	if p.upstreamTLS.InsecureSkipVerify || p.upstreamTLS.ServerName != "db-a" {
		t.Fatal("insecure default")
	}
	p.TLS = "skip-verify"
	if err := p.configureUpstreamTLS(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "db-a") || !strings.Contains(logs.String(), "development only") {
		t.Fatal("missing startup warning")
	}
	p.TLS = "verify"
	p.TLSCAFile = "/nonexistent/lustre-test-ca"
	if err := p.Run(context.Background(), nil); err == nil {
		t.Fatal("invalid CA did not fail startup")
	}
}

func TestTLSConfigurationSources(t *testing.T) {
	servers := []audit.Server{{Name: "db-a", ListenPort: 3307, UpstreamHost: "db-a", UpstreamPort: 3306, UpstreamUser: "bootstrap", Enabled: true, TLS: "verify", TLSCAFile: "/certs/ca.pem"}}
	ps, err := BuildProxies(servers, map[string]string{"db-a": "secret"}, &testSink{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].TLS != "verify" || ps[0].TLSCAFile != "/certs/ca.pem" {
		t.Fatal("TLS settings lost")
	}
	servers[0].TLS = "disabled"
	if err := config.ValidateServers(servers); err == nil {
		t.Fatal("accepted plaintext configuration")
	}
}
