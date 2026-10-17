package config

// ConfigClient is the dedicated API client for live proxy configuration
// polling (live configuration reconciliation). It
// owns exactly the concerns that belong to "how to ask the API for the
// current configuration": the bearer token, ETag-based polling so an
// unchanged snapshot costs a 304 instead of a full download, a bounded
// request timeout, and a bounded exponential backoff with jitter for
// retrying a failed poll. It does not know what a snapshot means or what to
// do with one — that is the reconciliation manager's job (a later chunk of
// this ticket), which is what actually drives Poll's callback.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"lustre/internal/audit"
)

// ErrConfigUnauthorized is returned by Fetch when the API rejects the
// configured service token (401/403). It is a sentinel rather than a
// message substring so callers can distinguish it from a transient failure
// with errors.Is — an auth failure will not fix itself by retrying, but per
// the ticket's failure-safety requirements it should also not tear down a
// working configuration; the two need different handling upstream.
var ErrConfigUnauthorized = errors.New("proxy config request rejected: check the service token")

// DefaultConfigRequestTimeout, DefaultConfigPollInterval, defaultConfigRetryBase
// and DefaultConfigRetryMax match the ticket's suggested environment
// variable defaults (PROXY_CONFIG_REQUEST_TIMEOUT, PROXY_CONFIG_POLL_INTERVAL,
// PROXY_CONFIG_RETRY_MAX) and apply whenever a ConfigClient field is left at
// its zero value, so a caller that only sets BaseURL and Token still gets
// sane polling behaviour.
const (
	DefaultConfigRequestTimeout = 5 * time.Second
	DefaultConfigPollInterval   = 3 * time.Second
	defaultConfigRetryBase      = 1 * time.Second
	DefaultConfigRetryMax       = 30 * time.Second
)

// ConfigSnapshot is one complete configuration returned by the API: a
// revision number and the full set of servers as of that revision, plus the
// ETag identifying it for the next poll's If-None-Match. It deliberately
// carries the complete set, not a diff — the ticket requires the manager to
// reconcile complete snapshots rather than replay individual mutations, and
// this is the type that contract is built on.
type ConfigSnapshot struct {
	Revision int
	Servers  []audit.Server
	ETag     string
}

// ConfigClient polls GET /api/proxy/config for the live routing
// configuration. Every field has a documented zero-value default, so a
// caller can construct one with just BaseURL and Token and get reasonable
// behaviour; see the fields below for what each env var in the ticket maps
// to.
type ConfigClient struct {
	// BaseURL is the API's base address, e.g. "http://api:8080"
	// (API_URL). "/api/proxy/config" is appended to it for every request.
	BaseURL string

	// Token authenticates this client to the API as the proxy service, not
	// as an operator — see FetchInitialSnapshotWithRetry's comment
	// elsewhere in this package for why that distinction matters
	// (API_PROXY_TOKEN). Never included in any error this client returns or
	// any value it logs.
	Token string

	// RequestTimeout bounds a single Fetch call end to end. Zero means
	// DefaultConfigRequestTimeout (PROXY_CONFIG_REQUEST_TIMEOUT).
	RequestTimeout time.Duration

	// PollInterval is how long Poll waits after a successful (or
	// not-modified) fetch before trying again. Zero means
	// DefaultConfigPollInterval (PROXY_CONFIG_POLL_INTERVAL).
	PollInterval time.Duration

	// RetryBase and RetryMax bound the backoff Poll uses after a failed
	// fetch — RetryBase is the delay after the first consecutive failure,
	// roughly doubling with each further failure, capped at RetryMax
	// regardless of how many follow. Zero means defaultConfigRetryBase and
	// DefaultConfigRetryMax respectively (PROXY_CONFIG_RETRY_MAX covers the
	// max; there is no suggested env var for the base, so it is fixed at a
	// conservative 1s).
	RetryBase time.Duration
	RetryMax  time.Duration

	// HTTPClient lets a caller inject its own client — a test asserting on
	// timeout behaviour, or a future caller with special transport needs.
	// A nil one is built lazily from RequestTimeout on first use.
	HTTPClient *http.Client

	mu   sync.Mutex
	etag string
	rnd  *rand.Rand
}

// httpClient returns c.HTTPClient if set, otherwise a client built once from
// RequestTimeout and cached for reuse — so repeated polls don't pay for a
// fresh client (and its connection pool) every time.
func (c *ConfigClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.HTTPClient == nil {
		timeout := c.RequestTimeout
		if timeout <= 0 {
			timeout = DefaultConfigRequestTimeout
		}
		c.HTTPClient = &http.Client{Timeout: timeout}
	}
	return c.HTTPClient
}

// currentETag and setETag guard c.etag with c.mu: Fetch can in principle be
// called concurrently (a caller retrying by hand alongside its own Poll
// loop, say), and a torn read/write on a plain string is still a data race
// even though string assignment itself is not read to be non-atomic on all
// platforms.
func (c *ConfigClient) currentETag() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.etag
}

func (c *ConfigClient) setETag(etag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.etag = etag
}

// CommitETag records etag as the value this client sends as If-None-Match
// on its next Fetch. Call it only once a snapshot has been confirmed fully
// applied — see startup in ../app/startup.go and the configPoller in
// ../runtime/configpoll.go.
//
// Fetch itself deliberately does not do this the moment it decodes a new
// snapshot. It used to, and that was the bug: saving the ETag before
// anything had actually been done with the snapshot meant a reconciliation
// failure (a port briefly in use, say) or a snapshot that failed validation
// got the ETag committed anyway. The next poll would then present that
// already-committed ETag, the API would correctly answer 304 Not Modified
// for it, and the caller would see "nothing changed" and never retry —
// silently leaving a server down until someone edited the config again to
// produce a genuinely new revision to retry against.
func (c *ConfigClient) CommitETag(etag string) {
	c.setETag(etag)
}

// Fetch performs one GET against BaseURL+"/api/proxy/config", authenticating
// with the bearer token and sending If-None-Match when a previously
// committed ETag is known (see CommitETag).
//
// Three outcomes:
//   - A new snapshot: (snapshot, nil). The client's stored ETag is NOT
//     updated here — the caller must call CommitETag(snapshot.ETag) once it
//     has confirmed the snapshot was fully applied. See CommitETag's
//     comment for why.
//   - No change (304 Not Modified): (nil, nil). The ticket is explicit that
//     this requires no reconciliation — this is what lets a caller
//     distinguish "I checked and nothing changed" from "the check itself
//     failed" without inspecting an error.
//   - A failure — network error, non-2xx/304 status, a response body that
//     doesn't decode, or one that decodes but is not a configuration
//     snapshot (see the envelope check below): (nil, err). The stored ETag is left untouched on
//     any failure, so a transient error never causes the next successful
//     poll to be compared against a snapshot this client never actually
//     saw. No error path here includes the token or the raw response body,
//     so nothing this method returns is safe to skip redacting when logged
//     — it already is.
func (c *ConfigClient) Fetch(ctx context.Context) (*ConfigSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/proxy/config", nil)
	if err != nil {
		return nil, fmt.Errorf("build proxy config request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if etag := c.currentETag(); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch proxy config: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrConfigUnauthorized
	case http.StatusOK:
		// fall through
	default:
		// Deliberately not echoing the response body here: on an error
		// path there's a real chance it's an upstream/proxy error page or
		// carries more detail than intended, and a status code is enough
		// to act on (retry, alert, whatever the caller does with it).
		return nil, fmt.Errorf("proxy config request failed with status %d", resp.StatusCode)
	}

	// Both fields are pointers on purpose, so "the field was not there" can be
	// told apart from "the field was there and empty". A plain int/slice
	// decodes {}, null and {"error":"temporary failure"} as revision 0 with no
	// servers — a perfectly valid-looking snapshot that would make the caller
	// tear down every working listener. Only an explicit "servers": [] is a
	// real, intentional empty configuration.
	var payload struct {
		Revision *int            `json:"revision"`
		Servers  *[]audit.Server `json:"servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode proxy config response: %w", err)
	}
	if payload.Revision == nil || payload.Servers == nil {
		// Not echoing the body, for the same reason as the status-code error
		// above.
		return nil, errors.New("proxy config response is not a configuration snapshot: " +
			`"revision" and "servers" must both be present`)
	}
	if *payload.Revision < 0 {
		return nil, fmt.Errorf("proxy config response has an invalid revision %d", *payload.Revision)
	}

	etag := resp.Header.Get("ETag")

	return &ConfigSnapshot{
		Revision: *payload.Revision,
		Servers:  *payload.Servers,
		ETag:     etag,
	}, nil
}

// ReportApplied tells the API which revision the proxy is now running, so the
// back-office can show whether a saved change has actually taken effect.
// Unlike Fetch this is best effort: the caller logs a failure and carries on,
// since a reconciliation that succeeded must not be undone by a status report
// that did not.
func (c *ConfigClient) ReportApplied(ctx context.Context, rev int) error {
	body, err := json.Marshal(map[string]int{"applied_revision": rev})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/proxy/status", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build proxy status request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("report applied revision: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// The status code alone is enough to act on, and the body is not echoed
	// for the same reason Fetch does not echo it.
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy status request failed with status %d", resp.StatusCode)
	}
	return nil
}

// backoff returns this client's bounded-exponential-backoff parameters,
// filling in defaults for whichever of RetryBase/RetryMax are unset.
func (c *ConfigClient) backoff() Backoff {
	base := c.RetryBase
	if base <= 0 {
		base = defaultConfigRetryBase
	}
	retryMax := c.RetryMax
	if retryMax <= 0 {
		retryMax = DefaultConfigRetryMax
	}
	return Backoff{Base: base, Max: retryMax}
}

// randSource lazily builds this client's own time-seeded random source for
// backoff jitter, guarded by c.mu. A dedicated source per client (rather
// than the shared global math/rand functions) keeps jitter from contending
// on a package-level lock across many proxies' poll loops, and makes it
// possible for a test to inject a seeded one for deterministic assertions.
func (c *ConfigClient) randSource() *rand.Rand {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rnd == nil {
		c.rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return c.rnd
}

func (c *ConfigClient) pollInterval() time.Duration {
	if c.PollInterval > 0 {
		return c.PollInterval
	}
	return DefaultConfigPollInterval
}

// Poll runs Fetch on PollInterval, forever, until ctx is done. onResult is
// called after every attempt — with (snapshot, nil) for a new snapshot,
// (nil, nil) for a not-modified poll, or (nil, err) for a failed one. Poll
// itself has no opinion on what any of those mean for the running
// configuration; that is entirely the caller's decision (the reconciliation
// manager, once it exists — see the package comment on this file). Poll only
// decides when to try again:
//
//   - After a successful poll (new snapshot or not-modified), the retry
//     count resets and Poll waits PollInterval.
//   - After a failed poll, Poll waits a bounded exponential backoff with
//     jitter (see Backoff) instead, so a flapping or down API isn't hammered
//     every PollInterval, and many proxies recovering from the same outage
//     don't all retry in lockstep.
//
// onResult is called synchronously from this goroutine — a slow or blocking
// onResult delays the next poll, which is deliberate: reconciliation for one
// snapshot should finish (or at least be handed off) before the next poll
// result can arrive and race it.
func (c *ConfigClient) Poll(ctx context.Context, onResult func(*ConfigSnapshot, error)) {
	attempt := 0
	for {
		snap, err := c.Fetch(ctx)
		onResult(snap, err)

		if ctx.Err() != nil {
			return
		}

		var wait time.Duration
		if err != nil {
			attempt++
			wait = c.backoff().Duration(attempt, c.randSource())
		} else {
			attempt = 0
			wait = c.pollInterval()
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Backoff computes a bounded exponential delay with full jitter, for
// retrying something that just failed. Base is the delay after the first
// failure; it roughly doubles with each further consecutive failure, never
// exceeding Max.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Duration returns the delay to wait before the attempt'th retry (attempt
// 1 is the first failure, 2 the second consecutive one, and so on).
//
// It computes Base*2^(attempt-1), capped at Max before the shift can
// overflow into something absurd (or negative, wrapping a time.Duration's
// signed int64), then — if rnd is non-nil — applies full jitter: a uniform
// random value in [0, that capped delay]. Full jitter (as opposed to
// capping the raw exponential alone) is what actually spreads retries out
// across many callers hitting the same outage; without it, every caller
// with the same failure history retries at exactly the same instant, which
// defeats the point. rnd may be nil (returns the un-jittered capped delay
// instead), which a test uses to assert on the exact bound rather than a
// random point inside it.
func (b Backoff) Duration(attempt int, rnd *rand.Rand) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := b.Base
	if base <= 0 {
		base = time.Second
	}
	capDelay := b.Max
	if capDelay <= 0 || capDelay < base {
		capDelay = base
	}

	shift := attempt - 1
	if shift > 32 { // more than enough to have already hit the cap; avoids a huge shift
		shift = 32
	}
	mult := int64(1) << uint(shift)
	d := base * time.Duration(mult)
	if d <= 0 || d > capDelay { // overflow (d<=0) or past the cap
		d = capDelay
	}

	if rnd == nil {
		return d
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rnd.Int63n(int64(d) + 1))
}
