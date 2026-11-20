// Adapted from origin/53-spike-com-change-user, internal/passthrough/changeuser.go.
package passthrough

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/upstream"

	"github.com/go-mysql-org/go-mysql/client"
	"github.com/go-mysql-org/go-mysql/mysql"
)

// DialChangeUser opens an upstream connection authenticated as user, by
// connecting under a bootstrap account and then re-authenticating with
// COM_CHANGE_USER, relaying every challenge to the connecting client through
// relay.
//
// The returned connection is an ordinary *client.Conn: go-mysql performed its
// own handshake, so its capability flags, status and server version are
// whatever it actually negotiated rather than anything we told it.
//
// Two things about it are not ordinary, and both belong in the ADR rather than
// only here. Its unexported user field still holds the bootstrap account,
// which is cosmetic until something reads it. And it must never be put in a
// client.Pool: a pool reconnect would re-run the *bootstrap* login and hand
// back a connection silently running as the wrong identity.
func DialChangeUser(ctx context.Context, addr, bootstrapUser, bootstrapPass, user string, relay AuthRelay, timeout time.Duration, tlsConfig *tls.Config) (*client.Conn, error) {
	return DialChangeUserIO(ctx, addr, bootstrapUser, bootstrapPass, user, relay, timeout, timeout, tlsConfig)
}

// DialChangeUserIO is DialChangeUser with a separate per-read and per-write
// timeout for the returned connection. The whole login is still bounded by
// timeout; ioTimeout is what stays on the connection for the life of the
// session (zero means none), so it must be the session's own limit, not the
// login's.
func DialChangeUserIO(ctx context.Context, addr, bootstrapUser, bootstrapPass, user string, relay AuthRelay, timeout, ioTimeout time.Duration, tlsConfig *tls.Config) (*client.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !relay.ClientOnTLS() {
		return nil, fmt.Errorf("pass-through authentication requires client TLS")
	}
	conn, err := upstream.Connect(ctx, addr, bootstrapUser, bootstrapPass, tlsConfig, timeout, ioTimeout)
	if err != nil {
		return nil, &BootstrapError{Cause: err}
	}

	// The relay is synchronous; the caller must also bound reads on its client leg.
	stop := context.AfterFunc(ctx, func() { _ = conn.Conn.Conn.Close() })
	err = changeUser(conn, user, relay)
	stopped := stop()
	if err == nil && !stopped {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// changeUser drives the COM_CHANGE_USER exchange on an open connection.
//
// conn.Conn is go-mysql's own *packet.Conn for this connection, reached
// through the exported embedded field. Writing to it directly is safe here
// because the command phase is idle: we are between client.Connect returning
// and the first Execute.
func changeUser(conn *client.Conn, user string, relay AuthRelay) error {
	pc := conn.Conn

	// COM_CHANGE_USER is a command, and every command starts a new packet
	// sequence. go-mysql's own writeCommand does this; we are bypassing it, so
	// we do it ourselves. Forgetting it is an immediate "got packets out of
	// order" from the server.
	pc.ResetSequence()

	payload, err := buildChangeUser(negotiatedCaps(conn), user)
	if err != nil {
		return err
	}
	if err := pc.WritePacket(payload); err != nil {
		return fmt.Errorf("write COM_CHANGE_USER: %w", err)
	}

	// The upstream names the account's plugin in an auth switch, followed
	// by the plugin exchange and then OK or ERR.
	return relayConnectionPhase(pc, relay)
}

// changeUserCaps are the capability flags the COM_CHANGE_USER payload layout
// depends on.
const changeUserCaps = mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_CONNECT_ATTRS

// maxUserLen bounds the user name sent upstream. MySQL accepts at most 32
// characters (96 bytes); this is only a sanity limit on what a client can make
// the proxy send.
const maxUserLen = 128

// negotiatedCaps reports which of changeUserCaps go-mysql actually negotiated
// with this upstream, so the encoder checks the real connection instead of
// assuming it.
//
// client.Conn.HasCapability only reports the optional flags the caller asked
// for, not the negotiated set, and the negotiated set is otherwise reachable
// only as the names in CapabilityString.
func negotiatedCaps(conn *client.Conn) uint32 {
	have := make(map[string]bool)
	for _, name := range strings.Split(conn.CapabilityString(), "|") {
		have[name] = true
	}
	var caps uint32
	for _, c := range []uint32{mysql.CLIENT_SECURE_CONNECTION, mysql.CLIENT_PLUGIN_AUTH, mysql.CLIENT_CONNECT_ATTRS} {
		if have[mysql.CapNames[c]] {
			caps |= c
		}
	}
	return caps
}

// buildChangeUser encodes Protocol::COM_CHANGE_USER.
//
// The payload's shape depends on three negotiated capability flags:
//
//	CLIENT_SECURE_CONNECTION  the auth response is length-prefixed rather than
//	                          NUL-terminated
//	CLIENT_PLUGIN_AUTH        the plugin name is sent
//	CLIENT_CONNECT_ATTRS      connection attributes are sent
//
// go-mysql negotiates all three against MySQL 8, but caps is what the
// connection really agreed, and a connection missing any of them is refused
// here with a clear error instead of sending a packet the server would
// misread.
//
// The request names mysql_clear_password with no auth data. The account's real
// plugin is different, so the server answers with an auth switch naming it
// (and its salt) before any password is checked. That lets the connecting
// client be challenged exactly once, with the right plugin.
func buildChangeUser(caps uint32, user string) ([]byte, error) {
	if caps&changeUserCaps != changeUserCaps {
		return nil, fmt.Errorf("upstream connection lacks capabilities required for COM_CHANGE_USER (have %#x, need %#x)", caps&changeUserCaps, changeUserCaps)
	}
	if user == "" || len(user) > maxUserLen || strings.IndexByte(user, 0) >= 0 {
		return nil, errors.New("invalid user name for COM_CHANGE_USER")
	}

	key, value := []byte("_client_name"), []byte("mysql-audit-proxy")
	attrs := append(mysql.PutLengthEncodedInt(uint64(len(key))), key...)
	attrs = append(attrs, mysql.PutLengthEncodedInt(uint64(len(value)))...)
	attrs = append(attrs, value...)
	attrs = append(mysql.PutLengthEncodedInt(uint64(len(attrs))), attrs...)

	buf := make([]byte, 4, 32+len(user)+len(attrs))
	buf = append(buf, mysql.COM_CHANGE_USER)

	buf = append(buf, user...)
	buf = append(buf, 0x00)

	buf = append(buf, 0x00) // auth response length: none, see above
	buf = append(buf, 0x00) // database: none, applyPendingDB handles it later

	// Charset is two bytes here, where the handshake response has one.
	buf = binary.LittleEndian.AppendUint16(buf, uint16(mysql.DEFAULT_COLLATION_ID))

	buf = append(buf, mysql.AUTH_CLEAR_PASSWORD...)
	buf = append(buf, 0x00)

	buf = append(buf, attrs...)

	return buf, nil
}

// BootstrapError marks an infrastructure failure, never a user's failed login.
// The integrating auth handler must log the cause at operator level, return a
// generic client error and suppress auth_fail.
//
// Error() is deliberately generic, because the cause names the bootstrap
// account ("Access denied for user 'auditproxy_bootstrap'@…"): a caller that
// forwards err.Error() to a client by mistake must not leak it. Operators get
// the detail from Detail().
type BootstrapError struct{ Cause error }

func (e *BootstrapError) Error() string { return "upstream bootstrap connection failed" }

// Detail is the underlying failure, for the operator log only.
func (e *BootstrapError) Detail() string {
	if e.Cause == nil {
		return ""
	}
	return e.Cause.Error()
}

func (e *BootstrapError) Unwrap() error { return e.Cause }
