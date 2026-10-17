package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
)

// Connect follows the stock go-mysql dial used by the COM_CHANGE_USER prototype
// on origin/53-spike-com-change-user, with mandatory TLS before authentication.
// Never pool a returned bootstrap connection: reconnect would restore its identity.
func Connect(ctx context.Context, addr, user, password string, cfg *tls.Config, timeout, ioTimeout time.Duration) (*client.Conn, error) {
	if cfg == nil {
		return nil, fmt.Errorf("upstream TLS configuration is required")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var raw net.Conn
	var stop func() bool
	conn, err := client.ConnectWithDialer(ctx, "tcp", addr, user, password, "",
		func(ctx context.Context, network, address string) (net.Conn, error) {
			var err error
			raw, err = (&net.Dialer{}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			// Closing on cancellation also bounds TLS and authentication reads; library
			// per-I/O deadlines alone do not bound the entire handshake.
			stop = context.AfterFunc(ctx, func() { _ = raw.Close() })
			return raw, nil
		}, func(c *client.Conn) error {
			c.SetTLSConfig(cfg.Clone())
			c.ReadTimeout = ioTimeout
			c.WriteTimeout = ioTimeout
			return nil
		})
	if stop != nil {
		stop()
	}
	if err != nil {
		if raw != nil {
			_ = raw.Close()
		}
		return nil, fmt.Errorf("upstream TLS connection failed: %w", err)
	}
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	return conn, nil
}
