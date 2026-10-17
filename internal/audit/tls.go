package audit

import "fmt"

const (
	TLSVerify     = "verify"
	TLSSkipVerify = "skip-verify"
)

// NormalizeTLS applies the secure default shared by every configuration source.
func NormalizeTLS(mode string) (string, error) {
	switch mode {
	case "", TLSVerify:
		return TLSVerify, nil
	case TLSSkipVerify:
		return mode, nil
	default:
		return "", fmt.Errorf("tls must be verify or skip-verify")
	}
}
