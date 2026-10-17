// Package config loads and validates proxy configuration from the environment, files, and API.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"lustre/internal/audit"
	"lustre/internal/sink"
)

// DefaultUpstreamReadTimeout bounds every read on the upstream connection
// (Execute, UseDB, Ping) so a wedged-but-not-closed upstream — hung rather
// than dropped — cannot block a command forever. This is the primary defence
// against a hang: it caps max single-query duration as a side effect, which
// is the trade-off for the guarantee. Configurable because that ceiling is a
// workload-specific choice; PROXY_UPSTREAM_READ_TIMEOUT=0 disables it.
const DefaultUpstreamReadTimeout = 60 * time.Second

// ---------------------------------------------------------------------------
// Configuration helpers
// ---------------------------------------------------------------------------

// GetEnv returns the value of the environment variable named by key, or
// fallback if the variable is not set.
func GetEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvInt reads key as an integer. An unset variable yields fallback; a
// malformed one is fatal rather than silently ignored, since a mistyped batch
// size would otherwise look like the sink working normally.
func getEnvInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		log.Fatalf("%s=%q is not a number: %v", key, raw, err)
	}
	return v
}

// GetEnvDuration reads key as a Go duration string ("500ms", "2s").
func GetEnvDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		log.Fatalf("%s=%q is not a duration: %v", key, raw, err)
	}
	return v
}

// getEnvOverflow reads key as "block" or "drop". Malformed is fatal rather
// than defaulted: this knob decides whether a full buffer stalls queries or
// discards audit records, and silently picking one would hide a typo behind
// whichever behaviour it guessed.
func getEnvOverflow(key string, fallback sink.OverflowMode) sink.OverflowMode {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "block":
		return sink.Block
	case "drop":
		return sink.Drop
	default:
		log.Fatalf("%s=%q is not \"block\" or \"drop\"", key, raw)
		return fallback // unreachable
	}
}

// ResolveSinkToken returns the bearer token the audit sink presents to the
// API: SINK_TOKEN if set, otherwise API_PROXY_TOKEN, otherwise "".
//
// It is separate from the token the proxy uses to fetch its configuration
// because API_PROXY_TOKEN together with PROXY_CONFIG_FILE is an ambiguous
// config source and fatal. A file-mode proxy that still posts audit events
// therefore needs SINK_TOKEN to carry the credential on its own.
func ResolveSinkToken() string {
	return GetEnv("SINK_TOKEN", GetEnv("API_PROXY_TOKEN", ""))
}

// NewAuditSink builds the sink the proxy emits through, and returns a function
// to shut it down.
//
// With no SINK_INGEST_URL the proxy logs events instead of posting them, so it
// still runs standalone without the back-office API. Zero values for batch size
// and flush interval mean "use the sink's own defaults".
func NewAuditSink() (audit.Sink, func()) {
	ingestURL := GetEnv("SINK_INGEST_URL", "")
	if ingestURL == "" {
		log.Print("SINK_INGEST_URL not set: audit events will be logged, not posted to the API")
		return sink.Fake{}, func() {}
	}

	token := ResolveSinkToken()
	if token == "" {
		log.Print("warning: SINK_INGEST_URL is set but neither SINK_TOKEN nor API_PROXY_TOKEN is: " +
			"the API will reject every audit batch with 401 and no events will be stored")
	}

	s, err := sink.New(sink.Config{
		IngestURL:     ingestURL,
		ServiceToken:  token,
		BatchSize:     getEnvInt("SINK_BATCH_SIZE", 0),
		FlushInterval: GetEnvDuration("SINK_FLUSH_INTERVAL", 0),
		OnFull:        getEnvOverflow("SINK_ON_FULL", sink.Block),
	})
	if err != nil {
		log.Fatalf("SINK_INGEST_URL: %v", err)
	}
	log.Printf("audit sink posting to %s", ingestURL)
	return s, s.Close
}

// ParseKV reads a "key:value,key2:value2" list, as used by
// PROXY_UPSTREAM_PASSWORDS. An empty string yields a nil map rather than an
// error, so an unset variable and an empty one behave the same way. envVar is
// only used to name the offending variable in an error.
func ParseKV(raw, envVar string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	kv := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, ":")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid %s entry %q, want key:value", envVar, entry)
		}
		kv[key] = value
	}
	return kv, nil
}

// DefaultConfigStartupDeadline bounds FetchInitialSnapshotWithRetry. The
// ticket asks startup to retry temporary API failures "rather than
// immediately terminating" but to "eventually fail with a clear error if no
// valid initial configuration [arrives] within a configured startup
// deadline." It is not one of the ticket's suggested env vars, so this
// keeps the exact value the old one-time fetchProxyConfigWithRetry used
// (30s) as the default; PROXY_CONFIG_STARTUP_DEADLINE overrides it.
const DefaultConfigStartupDeadline = 30 * time.Second

// DefaultDrainTimeout is PROXY_DRAIN_TIMEOUT's default, per the ticket's
// suggested env vars.
const DefaultDrainTimeout = 30 * time.Second

// FetchInitialSnapshotWithRetry polls client until it returns a real
// snapshot or deadline elapses, retrying transient failures instead of
// giving up on the first one — the ticket's startup behaviour requirement.
//
// An ErrConfigUnauthorized failure is returned immediately without
// retrying: a bad service token is a deployment mistake that waiting out
// the deadline will never fix, and per ConfigClient.Fetch's own contract
// nothing on this path ever logs the token itself. This mirrors why the
// proxy authenticates to /api/proxy/config with its own service token
// rather than an operator's login in the first place: that endpoint (unlike
// the sanitised back-office view at /api/servers) is the one that returns
// decrypted upstream credentials, and the service token is what
// distinguishes the proxy from a logged-in operator when asking for them.
//
// A 304 response to this very first request is also treated as a failure
// rather than "nothing changed": this client has no prior ETag to have
// gotten one against, so a 304 here would mean the API is behaving
// unexpectedly, not that there is nothing to do.
func FetchInitialSnapshotWithRetry(ctx context.Context, client *ConfigClient, deadline time.Duration) (*ConfigSnapshot, error) {
	startCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	attempt := 0
	var lastErr error
	for {
		snap, err := client.Fetch(startCtx)
		if err == nil && snap != nil {
			return snap, nil
		}
		if err == nil && snap == nil {
			err = errors.New("initial proxy config fetch returned no snapshot (unexpected 304 with no prior ETag)")
		}
		if errors.Is(err, ErrConfigUnauthorized) {
			return nil, err
		}

		lastErr = err
		attempt++
		wait := client.backoff().Duration(attempt, client.randSource())
		log.Printf("Waiting for API to become available (attempt %d): %v", attempt, err)

		select {
		case <-startCtx.Done():
			return nil, fmt.Errorf("timeout waiting for initial proxy configuration after %s: %w", deadline, lastErr)
		case <-time.After(wait):
		}
	}
}

// ---------------------------------------------------------------------------
// Multi-upstream configuration — a listener per configured port.
// ---------------------------------------------------------------------------

// LoadUpstreamConfig reads the port -> upstream mapping from a JSON file
// shaped like {"servers": [...audit.Server]} — deploy/upstreams.json is one,
// and it is the exact shape GET /api/servers already returns, so the API and
// the proxy read the same file with no format of its own to invent. Only
// enabled entries are returned; a disabled one is left out on purpose rather
// than filtered later, so nothing downstream has to know the field exists.
func LoadUpstreamConfig(path string) ([]audit.Server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var config struct {
		Servers []audit.Server `json:"servers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	var enabled []audit.Server
	for _, s := range config.Servers {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	return enabled, nil
}

// ValidateServers checks configured servers for the mistakes that would
// otherwise only surface once a client tries to connect: a missing name,
// host or user; a listen or upstream port outside the valid TCP range
// (notably 0, which net.Listen/net.Dial happily accept but which means
// "pick a random port" — never what a config file meant to say); and a
// name or listen port reused across entries, either of which would make
// routing or audit attribution ambiguous. It fails loud at startup instead
// of letting any of these fail quietly, or not at all, later.
func ValidateServers(servers []audit.Server) error {
	names := make(map[string]bool, len(servers))
	ports := make(map[int]bool, len(servers))

	for _, s := range servers {
		if _, err := audit.NormalizeTLS(s.TLS); err != nil {
			return fmt.Errorf("server %q: %w", s.Name, err)
		}
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("server %+v: name is required", s)
		}
		if strings.TrimSpace(s.UpstreamHost) == "" {
			return fmt.Errorf("server %q: upstream_host is required", s.Name)
		}
		if strings.TrimSpace(s.UpstreamUser) == "" {
			return fmt.Errorf("server %q: upstream_user is required", s.Name)
		}
		if s.ListenPort < 1 || s.ListenPort > 65535 {
			return fmt.Errorf("server %q: listen_port %d out of range 1-65535", s.Name, s.ListenPort)
		}
		if s.UpstreamPort < 1 || s.UpstreamPort > 65535 {
			return fmt.Errorf("server %q: upstream_port %d out of range 1-65535", s.Name, s.UpstreamPort)
		}
		if names[s.Name] {
			return fmt.Errorf("duplicate server name %q", s.Name)
		}
		names[s.Name] = true
		if ports[s.ListenPort] {
			return fmt.Errorf("duplicate listen_port %d (server %q)", s.ListenPort, s.Name)
		}
		ports[s.ListenPort] = true
	}
	return nil
}

// ConfigSource identifies which of the two ways main() can be told to load
// its configuration is actually in effect.
type ConfigSource int

const (
	ConfigSourceNone ConfigSource = iota
	ConfigSourceFile
	ConfigSourceAPI
)

// ErrAmbiguousConfigSource and ErrNoConfigSource are ChooseConfigSource's
// two failure cases — plain, user-facing messages that main() passes
// straight to log.Fatal. They are sentinels (rather than being constructed
// inline in ChooseConfigSource) so a test can assert on exactly which one
// came back with errors.Is.
var (
	ErrAmbiguousConfigSource = errors.New("ambiguous proxy configuration: both PROXY_CONFIG_FILE and API_PROXY_TOKEN are set. " +
		"Set PROXY_CONFIG_FILE alone for a static, non-reloading configuration, " +
		"or API_PROXY_TOKEN alone for live-reconciled configuration from the API — not both.")
	ErrNoConfigSource = errors.New("no proxy configuration source is set: set PROXY_CONFIG_FILE to read a static config file, " +
		"or API_PROXY_TOKEN to load and live-reconcile configuration from the API")
)

// ChooseConfigSource decides which configuration source main() should use,
// from whether PROXY_CONFIG_FILE and API_PROXY_TOKEN are set. It is pulled
// out of main() as a plain function specifically so this decision — unlike
// the log.Fatal calls around it, which end the process — is unit-testable:
// the ticket requires the configuration source to be explicit (API enabled
// means live reconciliation, file enabled means the unchanged startup-only
// behaviour, both or neither is rejected), and that requirement deserves a
// real test rather than only a read-through of main().
func ChooseConfigSource(fileConfigured, apiConfigured bool) (ConfigSource, error) {
	switch {
	case fileConfigured && apiConfigured:
		return ConfigSourceNone, ErrAmbiguousConfigSource
	case fileConfigured:
		return ConfigSourceFile, nil
	case apiConfigured:
		return ConfigSourceAPI, nil
	default:
		return ConfigSourceNone, ErrNoConfigSource
	}
}
