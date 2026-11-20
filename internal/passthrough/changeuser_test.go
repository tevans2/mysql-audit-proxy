package passthrough

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
)

func TestBuildChangeUserLayout(t *testing.T) {
	got, err := buildChangeUser(changeUserCaps, "alice")
	if err != nil {
		t.Fatal(err)
	}

	const clientName = "mysql-audit-proxy"
	attrs := []byte{12, '_', 'c', 'l', 'i', 'e', 'n', 't', '_', 'n', 'a', 'm', 'e', byte(len(clientName))}
	attrs = append(attrs, clientName...)
	want := []byte{0, 0, 0, 0, mysql.COM_CHANGE_USER}
	want = append(want, "alice"...)
	want = append(want, 0)                                   // user terminator
	want = append(want, 0)                                   // empty auth response
	want = append(want, 0)                                   // no database
	want = append(want, byte(mysql.DEFAULT_COLLATION_ID), 0) // two-byte charset
	want = append(want, mysql.AUTH_CLEAR_PASSWORD...)
	want = append(want, 0)
	want = append(want, byte(len(attrs)))
	want = append(want, attrs...)

	if !bytes.Equal(got, want) {
		t.Fatalf("COM_CHANGE_USER payload mismatch\n got % x\nwant % x", got, want)
	}
}

func TestBuildChangeUserRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps uint32
		user string
	}{
		{"empty user", changeUserCaps, ""},
		{"NUL in user", changeUserCaps, "al\x00ice"},
		{"oversized user", changeUserCaps, strings.Repeat("a", maxUserLen+1)},
		{"no secure connection", changeUserCaps &^ mysql.CLIENT_SECURE_CONNECTION, "alice"},
		{"no plugin auth", changeUserCaps &^ mysql.CLIENT_PLUGIN_AUTH, "alice"},
		{"no connect attrs", changeUserCaps &^ mysql.CLIENT_CONNECT_ATTRS, "alice"},
		{"no capabilities", 0, "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildChangeUser(tc.caps, tc.user); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
}

func TestBuildChangeUserAcceptsLongestUser(t *testing.T) {
	if _, err := buildChangeUser(changeUserCaps, strings.Repeat("a", maxUserLen)); err != nil {
		t.Fatal(err)
	}
}

// A bootstrap failure must read as infrastructure, not as the user's failed
// login, and must not carry the bootstrap account name in anything a caller
// could forward to the client.
func TestBootstrapFailureIsClassifiedAndQuiet(t *testing.T) {
	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	_, err = DialChangeUser(context.Background(), addr, "auditproxy_bootstrap_secret_name", "pw", "alice",
		&testRelay{secure: true}, time.Second, &tls.Config{InsecureSkipVerify: true})
	if err == nil {
		t.Fatal("expected a bootstrap failure")
	}
	var be *BootstrapError
	if !errors.As(err, &be) {
		t.Fatalf("not classified as BootstrapError: %v", err)
	}
	var myErr *mysql.MyError
	if errors.As(err, &myErr) {
		t.Fatal("bootstrap failure must never look like a database rejection (it would be audited as auth_fail)")
	}
	if strings.Contains(err.Error(), "auditproxy_bootstrap_secret_name") {
		t.Fatalf("Error() leaks the bootstrap account: %q", err.Error())
	}
}

func TestBootstrapErrorDetailKeepsCauseForOperators(t *testing.T) {
	be := &BootstrapError{Cause: errors.New("Access denied for user 'auditproxy_bootstrap'")}
	if !strings.Contains(be.Detail(), "auditproxy_bootstrap") {
		t.Fatalf("operator detail lost the cause: %q", be.Detail())
	}
	if strings.Contains(be.Error(), "auditproxy_bootstrap") {
		t.Fatalf("Error() leaks the cause: %q", be.Error())
	}
	if (&BootstrapError{}).Detail() != "" {
		t.Fatal("nil cause should give empty detail")
	}
}
