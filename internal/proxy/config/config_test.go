package config

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/sink"
)

// ---------------------------------------------------------------------------
// Multi-upstream config — one listener per configured port.
// ---------------------------------------------------------------------------

// TestLoadUpstreamConfig_FiltersDisabled verifies the JSON loader returns
// only enabled entries, using a temp file rather than deploy/upstreams.json
// so the test doesn't depend on that file's current contents.
func TestLoadUpstreamConfig_FiltersDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upstreams.json")

	cfg := struct {
		Servers []audit.Server `json:"servers"`
	}{
		Servers: []audit.Server{
			{ID: 1, Name: "db-a", ListenPort: 3307, UpstreamHost: "db-a", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true},
			{ID: 2, Name: "db-b", ListenPort: 3308, UpstreamHost: "db-b", UpstreamPort: 3306, UpstreamUser: "root", Enabled: false},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	servers, err := LoadUpstreamConfig(path)
	if err != nil {
		t.Fatalf("loadUpstreamConfig: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d enabled servers, want 1: %+v", len(servers), servers)
	}
	if servers[0].Name != "db-a" {
		t.Errorf("got enabled server %q, want db-a", servers[0].Name)
	}
}

// TestValidateServers_Valid verifies a well-formed set of servers passes
// validation cleanly.
func TestValidateServers_Valid(t *testing.T) {
	servers := []audit.Server{
		{Name: "db-a", ListenPort: 3307, UpstreamHost: "db-a", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true},
		{Name: "db-b", ListenPort: 3308, UpstreamHost: "db-b", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true},
	}
	if err := ValidateServers(servers); err != nil {
		t.Fatalf("validateServers: unexpected error: %v", err)
	}
}

// TestValidateServers_Invalid covers the misconfigurations the code reviewer
// asked to be caught at startup instead of only once a client connects (or,
// for listen_port 0, not caught at all — it silently binds a random port).
func TestValidateServers_Invalid(t *testing.T) {
	base := audit.Server{Name: "db-a", ListenPort: 3307, UpstreamHost: "db-a", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true}

	withName := func(s audit.Server, name string) audit.Server { s.Name = name; return s }
	withHost := func(s audit.Server, host string) audit.Server { s.UpstreamHost = host; return s }
	withUser := func(s audit.Server, user string) audit.Server { s.UpstreamUser = user; return s }
	withListenPort := func(s audit.Server, port int) audit.Server { s.ListenPort = port; return s }
	withUpstreamPort := func(s audit.Server, port int) audit.Server { s.UpstreamPort = port; return s }

	cases := map[string][]audit.Server{
		"missing name":            {withName(base, "")},
		"missing upstream host":   {withHost(base, "")},
		"missing upstream user":   {withUser(base, "")},
		"listen port zero":        {withListenPort(base, 0)},
		"listen port negative":    {withListenPort(base, -1)},
		"listen port too large":   {withListenPort(base, 65536)},
		"upstream port zero":      {withUpstreamPort(base, 0)},
		"upstream port too large": {withUpstreamPort(base, 70000)},
		"duplicate name": {
			withListenPort(base, 3307),
			withName(withListenPort(base, 3308), "db-a"),
		},
		"duplicate listen port": {
			withName(base, "db-a"),
			withName(base, "db-b"),
		},
	}

	for name, servers := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateServers(servers); err == nil {
				t.Fatalf("validateServers(%+v): want an error, got nil", servers)
			}
		})
	}
}

// An invalid SINK_ON_FULL must be fatal, not silently defaulted: this knob
// decides whether a full audit buffer stalls queries or discards records, so
// a typo must not be resolved by guessing. log.Fatalf exits the process, so
// the fatal path runs in a subprocess and the parent asserts on its exit.
func TestGetEnvOverflowInvalidIsFatal(t *testing.T) {
	if os.Getenv("SINK_ON_FULL_FATAL_SUBPROCESS") == "1" {
		getEnvOverflow("SINK_ON_FULL", sink.Block)
		return // unreachable if getEnvOverflow is fatal as intended
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestGetEnvOverflowInvalidIsFatal")
	cmd.Env = append(os.Environ(),
		"SINK_ON_FULL_FATAL_SUBPROCESS=1",
		"SINK_ON_FULL=blok",
	)
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("SINK_ON_FULL=blok did not exit non-zero (err=%v), output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "SINK_ON_FULL") {
		t.Fatalf("fatal message should name the variable, got:\n%s", out)
	}
}

// The two valid values must parse, and an unset variable must take the
// fallback rather than erroring.
func TestGetEnvOverflowValidValues(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want sink.OverflowMode
	}{
		{"block", sink.Block},
		{"drop", sink.Drop},
		{"BLOCK", sink.Block},
		{"  drop  ", sink.Drop},
	} {
		t.Setenv("SINK_ON_FULL", c.raw)
		if got := getEnvOverflow("SINK_ON_FULL", sink.Block); got != c.want {
			t.Errorf("SINK_ON_FULL=%q: got %v, want %v", c.raw, got, c.want)
		}
	}

	t.Setenv("SINK_ON_FULL", "")
	if got := getEnvOverflow("SINK_ON_FULL", sink.Drop); got != sink.Drop {
		t.Errorf("unset: got %v, want the fallback (Drop)", got)
	}
}

// Tests for FetchInitialSnapshotWithRetry — the startup half of the
// live-config-reconciliation ticket's "Startup behavior" requirement:
// retry temporary API failures during startup rather than immediately
// terminating, but fail fast on a bad token, and eventually fail with a
// clear error if no valid configuration arrives within a configured
// deadline. runLiveProxy and runStaticProxy themselves are not tested here
// — they call log.Fatal on every error path (this is main()'s job, not a
// unit's), so what's actually unit-testable, and what carries all of this
// requirement's real logic, is FetchInitialSnapshotWithRetry alone.
//
// Like configclient_test.go, these run against an in-process
// httptest.Server rather than a real API.

// fastRetryClient returns a ConfigClient pointed at srv with retry timing
// small enough that a test exercising several attempts still finishes in
// well under a second.
func fastRetryClient(srv *httptest.Server) *ConfigClient {
	return &ConfigClient{
		BaseURL:   srv.URL,
		Token:     "test-token",
		RetryBase: 10 * time.Millisecond,
		RetryMax:  40 * time.Millisecond,
	}
}

// TestFetchInitialSnapshotWithRetry_SucceedsAfterTransientFailures is the
// direct test for "Retry temporary API failures during startup rather than
// immediately terminating": a server that fails a few times and then
// recovers must still yield the snapshot from the eventually-successful
// attempt, well within the deadline.
func TestFetchInitialSnapshotWithRetry_SucceedsAfterTransientFailures(t *testing.T) {
	var attempts atomic.Int32
	const failuresBeforeSuccess = 3

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= failuresBeforeSuccess {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"rev-1"`)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"revision": 1, "servers": sampleServers()})
	}))
	defer srv.Close()

	client := fastRetryClient(srv)

	snap, err := FetchInitialSnapshotWithRetry(context.Background(), client, 2*time.Second)
	if err != nil {
		t.Fatalf("fetchInitialSnapshotWithRetry: %v", err)
	}
	if snap == nil || snap.Revision != 1 {
		t.Fatalf("snapshot = %+v, want revision 1", snap)
	}
	if got := attempts.Load(); got != failuresBeforeSuccess+1 {
		t.Errorf("server saw %d attempt(s), want %d (the failures plus the successful one)", got, failuresBeforeSuccess+1)
	}
}

// TestFetchInitialSnapshotWithRetry_FailsFastOnUnauthorized is the direct
// test for "an auth failure is returned immediately, since waiting out the
// deadline on a bad token turns a one-line misconfiguration into a silence
// followed by a timeout that names the wrong cause" (the same reasoning
// the old fetchProxyConfigWithRetry documented, carried over here): a
// service token the API rejects must not be retried at all.
func TestFetchInitialSnapshotWithRetry_FailsFastOnUnauthorized(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := fastRetryClient(srv)

	start := time.Now()
	_, err := FetchInitialSnapshotWithRetry(context.Background(), client, 5*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrConfigUnauthorized) {
		t.Fatalf("err = %v, want errors.Is(err, ErrConfigUnauthorized)", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("server saw %d attempt(s), want exactly 1 (no retrying an auth failure)", attempts.Load())
	}
	if elapsed > time.Second {
		t.Errorf("fetchInitialSnapshotWithRetry took %s to fail on an unauthorized response, want well under the 5s deadline", elapsed)
	}
}

// TestFetchInitialSnapshotWithRetry_TimesOutAfterDeadline is the direct
// test for "eventually fail with a clear error if no valid initial
// configuration [arrives] within a configured startup deadline": a server
// that never recovers must make FetchInitialSnapshotWithRetry give up once
// the deadline elapses, wrapping the last error it actually saw rather than
// a generic timeout with no cause.
func TestFetchInitialSnapshotWithRetry_TimesOutAfterDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := fastRetryClient(srv)
	deadline := 150 * time.Millisecond

	start := time.Now()
	_, err := FetchInitialSnapshotWithRetry(context.Background(), client, deadline)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("fetchInitialSnapshotWithRetry against a permanently failing API returned no error")
	}
	if elapsed < deadline {
		t.Errorf("fetchInitialSnapshotWithRetry returned after %s, before its own %s deadline had even elapsed", elapsed, deadline)
	}
	if elapsed > deadline+time.Second {
		t.Errorf("fetchInitialSnapshotWithRetry took %s to give up on a %s deadline — far too slow", elapsed, deadline)
	}
}

// TestChooseConfigSource covers all four combinations of
// PROXY_CONFIG_FILE/API_PROXY_TOKEN being set or not — the direct test for
// the ticket's "configuration source should be explicit" requirement, which
// main() itself can't be unit-tested for since its own version of this
// decision ends the process via log.Fatal.
func TestChooseConfigSource(t *testing.T) {
	tests := []struct {
		name                          string
		fileConfigured, apiConfigured bool
		want                          ConfigSource
		wantErr                       error
	}{
		{"file only: static startup-only behaviour", true, false, ConfigSourceFile, nil},
		{"API only: live reconciliation", false, true, ConfigSourceAPI, nil},
		{"both set: rejected as ambiguous", true, true, ConfigSourceNone, ErrAmbiguousConfigSource},
		{"neither set: rejected as unconfigured", false, false, ConfigSourceNone, ErrNoConfigSource},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ChooseConfigSource(tt.fileConfigured, tt.apiConfigured)
			if got != tt.want {
				t.Errorf("chooseConfigSource(%v, %v) source = %v, want %v", tt.fileConfigured, tt.apiConfigured, got, tt.want)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("chooseConfigSource(%v, %v) err = %v, want errors.Is(err, %v)", tt.fileConfigured, tt.apiConfigured, err, tt.wantErr)
			}
		})
	}
}

// TestFetchInitialSnapshotWithRetry_RespectsParentContextCancellation
// confirms the retry loop honours ctx being cancelled independently of its
// own deadline — a caller (or a future graceful-shutdown path) must be able
// to give up early without waiting out the full startup deadline.
func TestFetchInitialSnapshotWithRetry_RespectsParentContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := fastRetryClient(srv)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := FetchInitialSnapshotWithRetry(ctx, client, 10*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("fetchInitialSnapshotWithRetry returned no error after its parent context was cancelled")
	}
	if elapsed > 2*time.Second {
		t.Errorf("fetchInitialSnapshotWithRetry took %s to notice its parent context was cancelled, want well under its 10s deadline", elapsed)
	}
}

func TestResolveSinkToken(t *testing.T) {
	cases := []struct {
		name            string
		sink, proxy     string
		setSink, setPrx bool
		want            string
	}{
		{"neither set", "", "", false, false, ""},
		{"falls back to API_PROXY_TOKEN", "", "proxy-tok", false, true, "proxy-tok"},
		{"SINK_TOKEN alone", "sink-tok", "", true, false, "sink-tok"},
		{"SINK_TOKEN wins over API_PROXY_TOKEN", "sink-tok", "proxy-tok", true, true, "sink-tok"},
		{"empty SINK_TOKEN falls back", "", "proxy-tok", true, true, "proxy-tok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SINK_TOKEN", "")
			t.Setenv("API_PROXY_TOKEN", "")
			if tc.setSink {
				t.Setenv("SINK_TOKEN", tc.sink)
			}
			if tc.setPrx {
				t.Setenv("API_PROXY_TOKEN", tc.proxy)
			}
			if got := ResolveSinkToken(); got != tc.want {
				t.Errorf("ResolveSinkToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

// sinkAuthorization runs one event through the sink NewAuditSink builds from
// the current environment and returns the Authorization header it posted, plus
// whatever the process logged while building it.
func sinkAuthorization(t *testing.T) (header, logged string) {
	t.Helper()
	got := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	t.Setenv("SINK_INGEST_URL", srv.URL)
	t.Setenv("SINK_BATCH_SIZE", "1")

	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	s, closeSink := NewAuditSink()
	s.Emit(audit.Event{ConnID: "c1", Kind: "connect", At: "2026-10-14T10:00:00.000000Z", User: "alice"})
	closeSink()

	select {
	case header = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("sink never posted")
	}
	return header, buf.String()
}

func TestNewAuditSinkAuthenticatesWithTheResolvedToken(t *testing.T) {
	t.Run("SINK_TOKEN only, as a file-mode proxy has it", func(t *testing.T) {
		t.Setenv("SINK_TOKEN", "sink-tok")
		t.Setenv("API_PROXY_TOKEN", "")
		if h, _ := sinkAuthorization(t); h != "Bearer sink-tok" {
			t.Errorf("Authorization = %q, want %q", h, "Bearer sink-tok")
		}
	})
	t.Run("falls back to API_PROXY_TOKEN, as live mode and compose have it", func(t *testing.T) {
		t.Setenv("SINK_TOKEN", "")
		t.Setenv("API_PROXY_TOKEN", "proxy-tok")
		if h, _ := sinkAuthorization(t); h != "Bearer proxy-tok" {
			t.Errorf("Authorization = %q, want %q", h, "Bearer proxy-tok")
		}
	})
}

func TestNewAuditSinkWarnsWhenNoTokenResolves(t *testing.T) {
	t.Setenv("SINK_TOKEN", "")
	t.Setenv("API_PROXY_TOKEN", "")
	header, logged := sinkAuthorization(t)
	if header != "" {
		t.Errorf("Authorization = %q, want none", header)
	}
	if !strings.Contains(logged, "neither SINK_TOKEN nor API_PROXY_TOKEN") {
		t.Errorf("no startup warning for a missing token; log was %q", logged)
	}

	t.Setenv("SINK_TOKEN", "sink-tok")
	if _, logged := sinkAuthorization(t); strings.Contains(logged, "neither SINK_TOKEN") {
		t.Errorf("warned although a token resolves; log was %q", logged)
	}
}
