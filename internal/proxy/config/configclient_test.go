package config

// Unit tests for ConfigClient — the config-poll API client (see the package
// comment on configclient.go). These run against an in-process
// httptest.Server, not a real API, and don't need Docker or a real upstream
// at all: the whole point of this chunk is that the client's HTTP/ETag/
// backoff behaviour is testable in complete isolation from reconciliation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
)

func sampleServers() []audit.Server {
	return []audit.Server{
		{ID: 1, Name: "db-a", ListenPort: 3307, UpstreamHost: "db-a", UpstreamPort: 3306, UpstreamUser: "root", UpstreamPass: "rootpass", Enabled: true},
	}
}

// TestConfigClient_FetchSendsBearerTokenAndReturnsSnapshot is the direct
// test for "Proxy service bearer token" and the basic happy path: a 200
// response decodes into a ConfigSnapshot carrying the revision, the
// servers, and the ETag for the next poll.
func TestConfigClient_FetchSendsBearerTokenAndReturnsSnapshot(t *testing.T) {
	const token = "super-secret-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Log("Step 1: Server checking the request path and bearer token")
		if r.URL.Path != "/api/proxy/config" {
			t.Errorf("request path = %q, want /api/proxy/config", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer "+token)
		}

		w.Header().Set("ETag", `"rev-1"`)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"revision": 1,
			"servers":  sampleServers(),
		})
	}))
	defer srv.Close()

	c := &ConfigClient{BaseURL: srv.URL, Token: token}

	t.Log("Step 2: Fetching")
	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap == nil {
		t.Fatal("Fetch returned a nil snapshot on a 200 response")
	}
	if snap.Revision != 1 {
		t.Errorf("Revision = %d, want 1", snap.Revision)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Name != "db-a" {
		t.Errorf("Servers = %+v, want the one sample server", snap.Servers)
	}
	if snap.ETag != `"rev-1"` {
		t.Errorf("ETag = %q, want %q", snap.ETag, `"rev-1"`)
	}
}

// TestConfigClient_SecondFetchSendsIfNoneMatchAndHandles304 is the direct
// test for "ETag support": once a client has committed an ETag, the next
// Fetch must send it as If-None-Match, and a 304 response must come back as
// (nil, nil) — no reconciliation needed, per the ticket.
//
// Fetch itself deliberately does not store the ETag the moment it decodes a
// snapshot — see CommitETag's doc comment in configclient.go. In
// production, configPoller only commits an ETag once that snapshot has
// actually been applied; this test simulates that one call so it can still
// exercise the real If-None-Match behaviour on the second request.
func TestConfigClient_SecondFetchSendsIfNoneMatchAndHandles304(t *testing.T) {
	var requests atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 1 {
			t.Log("Step 1: First request — no If-None-Match yet, respond 200 with an ETag")
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Errorf("first request sent If-None-Match: %q, want none", got)
			}
			w.Header().Set("ETag", `"rev-1"`)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"revision": 1, "servers": sampleServers()})
			return
		}

		t.Log("Step 2: Second request — must carry the ETag from the first response")
		if got := r.Header.Get("If-None-Match"); got != `"rev-1"` {
			t.Errorf("second request If-None-Match = %q, want %q", got, `"rev-1"`)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := &ConfigClient{BaseURL: srv.URL, Token: "t"}

	firstSnap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	t.Log("Step 1.5: Simulate the snapshot having been successfully applied, which is the only thing that commits an ETag now")
	c.CommitETag(firstSnap.ETag)

	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if snap != nil {
		t.Errorf("second Fetch (304) returned a non-nil snapshot: %+v, want nil (no reconciliation needed)", snap)
	}
	if requests.Load() != 2 {
		t.Fatalf("server saw %d requests, want 2", requests.Load())
	}
}

// TestConfigClient_UnauthorizedReturnsSentinelError is the direct test for
// the ticket's "Authentication temporarily fails" failure-safety case: the
// caller needs to be able to tell an auth rejection apart from any other
// failure with errors.Is, since the two might warrant different handling
// (e.g. alerting louder on a bad token than on a network blip) even though
// both leave the last known-good configuration in place.
func TestConfigClient_UnauthorizedReturnsSentinelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &ConfigClient{BaseURL: srv.URL, Token: "wrong-token"}

	snap, err := c.Fetch(context.Background())
	if snap != nil {
		t.Errorf("Fetch on 401 returned a non-nil snapshot: %+v", snap)
	}
	if !errors.Is(err, ErrConfigUnauthorized) {
		t.Fatalf("Fetch on 401 returned %v, want errors.Is(err, ErrConfigUnauthorized)", err)
	}
	if strings.Contains(err.Error(), "wrong-token") {
		t.Errorf("error message leaked the token: %v", err)
	}
}

// TestConfigClient_MalformedBodyDoesNotAdvanceETag is the direct test for
// two requirements at once: "A response is malformed" (failure safety) and
// the documented promise that a failed Fetch never updates the stored ETag.
// If it did, a transient bad response could permanently desync the client
// from a revision it never actually saw, silently skipping it on every
// later poll.
func TestConfigClient_MalformedBodyDoesNotAdvanceETag(t *testing.T) {
	var requests atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 1 {
			t.Log("Step 1: First request — 200 but a malformed (non-JSON) body")
			w.Header().Set("ETag", `"should-not-be-stored"`)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "not json")
			return
		}

		t.Log("Step 2: Second request — must NOT carry the ETag from the malformed response")
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Errorf("second request sent If-None-Match: %q, want none (first response's ETag must not have been stored)", got)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"revision": 2, "servers": sampleServers()})
	}))
	defer srv.Close()

	c := &ConfigClient{BaseURL: srv.URL, Token: "t"}

	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch on a malformed body returned no error")
	}

	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
}

// TestConfigClient_RejectsBodiesThatAreNotASnapshot is the direct test for the
// failure-safety gap where a 200 response with the wrong shape decoded as
// "revision 0, no servers" and made the caller remove every working listener.
func TestConfigClient_RejectsBodiesThatAreNotASnapshot(t *testing.T) {
	bodies := map[string]string{
		"empty object":      `{}`,
		"null":              `null`,
		"error object":      `{"error":"temporary failure"}`,
		"servers missing":   `{"revision":3}`,
		"revision missing":  `{"servers":[]}`,
		"servers null":      `{"revision":3,"servers":null}`,
		"negative revision": `{"revision":-1,"servers":[]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"should-not-matter"`)
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, body)
			}))
			defer srv.Close()

			c := &ConfigClient{BaseURL: srv.URL, Token: "t"}
			snap, err := c.Fetch(context.Background())
			if err == nil {
				t.Fatalf("Fetch accepted %s as a snapshot: %+v", body, snap)
			}
			if snap != nil {
				t.Fatalf("Fetch returned both a snapshot and an error: %+v", snap)
			}
		})
	}
}

// TestConfigClient_AcceptsExplicitEmptyServerList is the other half: an
// operator who really did remove every server sends "servers": [], and that
// must still come through as an (empty) snapshot, not an error.
func TestConfigClient_AcceptsExplicitEmptyServerList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"rev-7"`)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"revision":7,"servers":[]}`)
	}))
	defer srv.Close()

	c := &ConfigClient{BaseURL: srv.URL, Token: "t"}
	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch rejected an explicit empty server list: %v", err)
	}
	if snap == nil || snap.Revision != 7 || len(snap.Servers) != 0 {
		t.Fatalf("snapshot = %+v, want revision 7 with no servers", snap)
	}
}

// TestConfigClient_RequestTimeoutIsEnforced is the direct test for
// "HTTP request timeout": a server that never responds must not hang Fetch
// forever.
func TestConfigClient_RequestTimeoutIsEnforced(t *testing.T) {
	block := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never respond until this test unblocks it below
	}))
	// Registration order matters here: srv.Close() blocks until every
	// outstanding request completes, and the handler above is stuck on
	// <-block until something closes it — so block must close BEFORE
	// Close() runs. Plain defers in the same function run LIFO, so
	// registering close(block) after defer srv.Close() puts it first.
	defer srv.Close()
	defer close(block)

	c := &ConfigClient{
		BaseURL:        srv.URL,
		Token:          "t",
		RequestTimeout: 100 * time.Millisecond,
	}

	start := time.Now()
	_, err := c.Fetch(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Fetch against a server that never responds returned no error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Fetch took %v to time out, want well under RequestTimeout's neighbourhood (100ms)", elapsed)
	}
}

// TestBackoff_BoundedExponentialWithJitter is the direct test for "Bounded
// exponential backoff with jitter": each attempt's un-jittered ceiling must
// grow (until it hits the cap), the cap must never be exceeded, and jitter
// must actually vary the result rather than always returning the ceiling.
func TestBackoff_BoundedExponentialWithJitter(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: 1 * time.Second}

	t.Log("Step 1: Un-jittered ceiling roughly doubles per attempt, capped at Max")
	prev := time.Duration(0)
	for attempt := 1; attempt <= 6; attempt++ {
		d := b.Duration(attempt, nil)
		if d > b.Max {
			t.Fatalf("attempt %d: Duration = %v, exceeds Max %v", attempt, d, b.Max)
		}
		if d < prev {
			t.Fatalf("attempt %d: Duration = %v, less than previous attempt's %v — should be non-decreasing until the cap", attempt, d, prev)
		}
		prev = d
	}
	if got := b.Duration(1, nil); got != b.Base {
		t.Errorf("Duration(1, nil) = %v, want Base %v", got, b.Base)
	}
	if got := b.Duration(100, nil); got != b.Max {
		t.Errorf("Duration(100, nil) = %v, want it capped at Max %v", got, b.Max)
	}

	t.Log("Step 2: With a random source, jitter must stay within [0, ceiling] and actually vary")
	rnd := rand.New(rand.NewSource(1))
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := b.Duration(4, rnd)
		if d < 0 || d > b.Duration(4, nil) {
			t.Fatalf("jittered Duration(4, rnd) = %v, want it within [0, %v]", d, b.Duration(4, nil))
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Errorf("jittered Duration returned the same value every time across 50 calls — jitter doesn't look like it's doing anything: %v", seen)
	}
}

// TestConfigClient_PollStopsOnContextCancellation confirms Poll actually
// returns promptly when its context is cancelled, rather than blocking
// through a poll interval or backoff wait — the ticket needs Poll to be a
// well-behaved background loop the manager (a later chunk) can start and
// stop via ctx like everything else in this refactor (see ../runtime/session_test.go).
func TestConfigClient_PollStopsOnContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"revision": 1, "servers": sampleServers()})
	}))
	defer srv.Close()

	c := &ConfigClient{
		BaseURL:      srv.URL,
		Token:        "t",
		PollInterval: time.Hour, // long enough that only cancellation could end this test promptly
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	var calls atomic.Int64
	go func() {
		c.Poll(ctx, func(snap *ConfigSnapshot, err error) {
			calls.Add(1)
		})
		close(done)
	}()

	// Let the first poll happen, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("Poll never invoked onResult even once")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Poll did not return within 2s of ctx being cancelled — it's stuck waiting on the 1h poll interval")
	}
}

// TestConfigClient_PollRetriesFailuresWithoutWaitingFullInterval confirms
// Poll notices a failure and retries sooner than PollInterval (i.e. it's
// actually using the backoff path, not just the success-path interval
// regardless of outcome) — the direct test for the ticket's "the proxy
// catches up after API recovery" acceptance criterion, in miniature.
func TestConfigClient_PollRetriesFailuresWithoutWaitingFullInterval(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"revision": 1, "servers": sampleServers()})
	}))
	defer srv.Close()

	c := &ConfigClient{
		BaseURL:      srv.URL,
		Token:        "t",
		PollInterval: time.Hour, // only reached after a success; failures must retry much sooner
		RetryBase:    5 * time.Millisecond,
		RetryMax:     20 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var successes atomic.Int64
	done := make(chan struct{})
	go func() {
		c.Poll(ctx, func(snap *ConfigSnapshot, err error) {
			if err == nil && snap != nil {
				successes.Add(1)
				if successes.Load() == 1 {
					close(done)
				}
			}
		})
	}()

	select {
	case <-done:
		// Got a success after two failures, well before PollInterval (1h)
		// could ever have elapsed — proves the backoff path, not the
		// success-interval path, drove those retries.
	case <-time.After(3 * time.Second):
		t.Fatalf("Poll had not succeeded within 3s (requests so far: %d) — backoff retries don't look like they're happening", requests.Load())
	}
}

// TestConfigClient_ReportAppliedPostsRevision covers the heartbeat's wire
// format: the back-office reads what this sends to decide whether a saved
// change is live.
func TestConfigClient_ReportAppliedPostsRevision(t *testing.T) {
	const token = "service-token"
	var gotBody atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/proxy/status" {
			t.Errorf("path = %q, want /api/proxy/status", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want %q", got, "Bearer "+token)
		}
		var payload struct {
			AppliedRevision *int `json:"applied_revision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding body: %v", err)
		} else if payload.AppliedRevision == nil {
			t.Error("body carried no applied_revision")
		} else {
			gotBody.Store(*payload.AppliedRevision)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := &ConfigClient{BaseURL: srv.URL, Token: token}
	if err := c.ReportApplied(context.Background(), 9); err != nil {
		t.Fatalf("ReportApplied: %v", err)
	}
	if got := gotBody.Load(); got != 9 {
		t.Errorf("server received applied_revision = %v, want 9", got)
	}
}

// A non-2xx is an error the caller logs, and the token never appears in it.
func TestConfigClient_ReportAppliedFailureDoesNotLeakToken(t *testing.T) {
	const token = "super-secret-token"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &ConfigClient{BaseURL: srv.URL, Token: token}
	err := c.ReportApplied(context.Background(), 1)
	if err == nil {
		t.Fatal("ReportApplied against a 500 returned no error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error message leaked the token: %v", err)
	}
}
