package store

import (
	"fmt"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
)

// TLSUpdate distinguishes omitted fields from explicit empty values in API PUTs.
// Nil values become SQL NULL parameters for COALESCE, preserving stored values.
type TLSUpdate struct {
	Mode   *string
	CAFile *string
}

func serverTLSUpdate(srv audit.Server, updates []TLSUpdate) (mode, caFile any, err error) {
	if len(updates) > 1 {
		return nil, nil, fmt.Errorf("only one TLS update is allowed")
	}
	if len(updates) == 0 {
		normalized, err := audit.NormalizeTLS(srv.TLS)
		return normalized, srv.TLSCAFile, err
	}
	update := updates[0]
	if update.Mode != nil {
		mode, err = audit.NormalizeTLS(*update.Mode)
		if err != nil {
			return nil, nil, err
		}
	}
	if update.CAFile != nil {
		caFile = *update.CAFile
	}
	return mode, caFile, nil
}
