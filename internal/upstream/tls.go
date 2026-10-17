// Package upstream opens mandatory TLS MySQL connections for ordinary and
// bootstrap sessions. It deliberately provides no plaintext connection mode.
package upstream

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"lustre/internal/audit"
)

// TLSConfig verifies the configured DNS name or IP, never the resolved address.
// A supplied PEM bundle replaces system roots for this server.
func TLSConfig(host, mode, caFile string) (*tls.Config, error) {
	mode, err := audit.NormalizeTLS(mode)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, fmt.Errorf("upstream TLS requires a hostname or IP")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host,
		InsecureSkipVerify: mode == audit.TLSSkipVerify} // Explicit development-only mode.
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read upstream TLS CA file: %w", err)
		}
		cfg.RootCAs = x509.NewCertPool()
		if !cfg.RootCAs.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("upstream TLS CA file contains no valid certificates")
		}
	}
	return cfg, nil
}
