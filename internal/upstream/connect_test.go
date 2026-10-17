package upstream

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
)

func certificate(t *testing.T, expired bool) (tls.Certificate, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Hour)
	if expired {
		end = time.Now().Add(-time.Hour)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: end, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, path
}

func mysqlServer(t *testing.T, cfg *tls.Config) (string, <-chan error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	result := make(chan error, 1)
	go func() {
		raw, err := l.Accept()
		if err != nil {
			result <- err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		auth := server.NewInMemoryAuthenticationHandler(mysql.AUTH_NATIVE_PASSWORD)
		auth.AddUser("test", "secret")
		svr := server.NewServer("8.0.0", mysql.DEFAULT_COLLATION_ID, mysql.AUTH_NATIVE_PASSWORD, nil, cfg)
		_, err = svr.NewCustomizedConn(raw, auth, server.EmptyHandler{})
		result <- err
	}()
	return l.Addr().String(), result
}

func TestMandatoryTLS(t *testing.T) {
	cert, ca := certificate(t, false)
	expired, expiredCA := certificate(t, true)
	for _, tc := range []struct {
		name, host, mode, ca string
		cert                 *tls.Certificate
		want                 string
	}{
		{"trusted", "localhost", "", ca, &cert, ""},
		{"untrusted", "localhost", "verify", "", &cert, "certificate"},
		{"hostname", "wrong.example", "verify", ca, &cert, "certificate"},
		{"expired", "localhost", "verify", expiredCA, &expired, "expired"},
		{"skip verification still encrypts", "localhost", "skip-verify", "", &cert, ""},
		{"TLS required verify", "localhost", "verify", ca, nil, "does not support TLS"},
		{"TLS required skip", "localhost", "skip-verify", "", nil, "does not support TLS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var serverTLS *tls.Config
			if tc.cert != nil {
				serverTLS = &tls.Config{Certificates: []tls.Certificate{*tc.cert}}
			}
			addr, result := mysqlServer(t, serverTLS)
			cfg, err := TLSConfig(tc.host, tc.mode, tc.ca)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := Connect(context.Background(), addr, "test", "secret", cfg, time.Second, time.Second)
			if tc.want != "" {
				if err == nil {
					conn.Close()
					t.Fatal("connection unexpectedly succeeded")
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("got %v, want %q", err, tc.want)
				}
				if serverErr := <-result; serverErr == nil {
					t.Fatal("server authenticated despite TLS failure")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				secure, ok := conn.Conn.Conn.(*tls.Conn)
				if !ok || !secure.ConnectionState().HandshakeComplete || secure.ConnectionState().CipherSuite == 0 {
					t.Fatal("connection is not encrypted")
				}
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestTLSConfigurationErrors(t *testing.T) {
	for _, mode := range []string{"disabled", "preferred", "plaintext"} {
		if _, err := TLSConfig("localhost", mode, ""); err == nil {
			t.Fatalf("accepted %q", mode)
		}
	}
	if _, err := TLSConfig("localhost", "verify", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted missing CA")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	os.WriteFile(bad, []byte("invalid"), 0600)
	if _, err := TLSConfig("localhost", "verify", bad); err == nil {
		t.Fatal("accepted invalid CA")
	}
	if _, err := Connect(context.Background(), "invalid", "test", "secret", nil, time.Second, 0); err == nil {
		t.Fatal("accepted nil TLS")
	}
}

func TestHandshakeDeadline(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	closed := make(chan struct{})
	go func() {
		raw, err := l.Accept()
		if err != nil {
			close(closed)
			return
		}
		defer raw.Close()
		defer close(closed)
		var b [1]byte
		raw.Read(b[:])
	}()
	cfg, _ := TLSConfig("localhost", "verify", "")
	start := time.Now()
	conn, err := Connect(context.Background(), l.Addr().String(), "test", "secret", cfg, 100*time.Millisecond, 0)
	if err == nil {
		conn.Close()
		t.Fatal("stalled greeting succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("handshake exceeded deadline")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("failed handshake leaked socket")
	}
}
