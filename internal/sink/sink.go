// sink implements the audit pipeline:
// buffered + batched POST to the API, with the fail-safe rule
package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"lustre/internal/audit"
)

// What Emit does when the buffer is full.
type OverflowMode int

const (
	// Block holds the caller until there is room: completeness over latency
	// (ADR 0005). Drop discards the event: latency over completeness.
	Block OverflowMode = iota
	Drop
)

// tunes sink.
type Config struct {
	IngestURL     string        // back-office ingest endpoint
	ServiceToken  string        // bearer token the ingest endpoint requires; empty sends no Authorization header
	BufferSize    int           // channel capacity(1024) (when full= emit)
	BatchSize     int           // flush once this many events are queued (50)
	FlushInterval time.Duration // flush this often atleast (1s)
	HTTPTimeout   time.Duration // request timeout (5s)
	MaxRetries    int           // extra attempts when batch fail (2)
	Logger        *log.Logger   // drop lines (log.Default())
	OnFull        OverflowMode  // buffer-full behaviour (Block)
}

// Snapshot of whats been done
type Stats struct {
	Emitted int64 // events to Emit()
	Sent    int64 // events delivered
	Dropped int64 // events dropped
	Failed  int64 // events that failed retry(batches)
}

// outcome of a POST attempt
type postResult int

const (
	postOK        postResult = iota //2xx: delivered
	postRetry                       //5xx: retrying
	postPermanent                   //4xx: retry won't help
)

// concrete Sink. Build with New(runs till close)
// an audit.Sink
type HTTPSink struct {
	cfg       Config
	client    *http.Client
	events    chan audit.Event
	done      chan struct{} //closed once to signal shutdown
	wg        sync.WaitGroup
	closeOnce sync.Once
	emitGate  sync.RWMutex
	closing   bool

	emitted atomic.Int64
	sent    atomic.Int64
	dropped atomic.Int64
	failed  atomic.Int64
}

// Compile time check
var _ audit.Sink = (*HTTPSink)(nil)

// builds sink and starts its background worker.
//
// A bad IngestURL is rejected here rather than discovered at the first flush.
// Every send would fail the same way the API being down does, so a typo in the
// config would read as an outage and events would be dropped indefinitely with
// nothing pointing at the cause.
func New(cfg Config) (*HTTPSink, error) {
	u, err := url.Parse(cfg.IngestURL)
	if err != nil {
		return nil, fmt.Errorf("sink: IngestURL %q: %w", cfg.IngestURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("sink: IngestURL %q needs an http:// or https:// scheme", cfg.IngestURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("sink: IngestURL %q has no host", cfg.IngestURL)
	}

	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1024
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}

	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}

	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}

	s := &HTTPSink{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.HTTPTimeout},
		events: make(chan audit.Event, cfg.BufferSize),
		done:   make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()
	return s, nil
}

// Emit queues one event for delivery(hot path)
func (s *HTTPSink) Emit(e audit.Event) {
	s.emitted.Add(1)

	s.emitGate.RLock()
	defer s.emitGate.RUnlock()

	if s.closing {
		s.dropped.Add(1) //drop rather than send on a draining sink
		return
	}

	if s.cfg.OnFull == Drop {
		select {
		case s.events <- e:
		default:
			s.dropped.Add(1) // buffer full-->drop(doesnt block)
		}
		return
	}

	// Block mode: wait for room, but never past shutdown
	// a plain send would deadlock once the worker stops draining.
	select {
	case s.events <- e:
	case <-s.done:
		s.dropped.Add(1)
	}
}

// single background worker that owns the batch slice
// no locks on the hot data.
func (s *HTTPSink) run() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]audit.Event, 0, s.cfg.BatchSize)
	// owned by this goroutine alone, like batch --> needs no synchronisation
	lastDropped := int64(0)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.send(batch)
		batch = batch[:0]
	}

	for {
		select {
		case e := <-s.events:
			batch = append(batch, e)
			if len(batch) >= s.cfg.BatchSize {
				flush() //size trigger
			}
		case <-ticker.C:
			flush() //time trigger
			if d := s.dropped.Load(); d > lastDropped {
				s.cfg.Logger.Printf("sink dropped %d events since the last flush", d-lastDropped)
				lastDropped = d
			}
		case <-s.done:
			//drain everything buffered--> then stop
			for {
				select {
				case e := <-s.events:
					batch = append(batch, e)
					if len(batch) >= s.cfg.BatchSize {
						flush()
					}
				default:
					flush() //tail
					return
				}
			}
		}
	}

}

// delivers one batch with retry +and backoff.
// if every event fails--> event is dropped
// never blocked back onto the proxy (D8).
func (s *HTTPSink) send(batch []audit.Event) {
	body, err := json.Marshal(batch)
	if err != nil {
		s.failed.Add(int64(len(batch)))
		s.cfg.Logger.Printf("sink: marshal failed, dropping %d events: %v", len(batch), err)
		return
	}

	// Kept so the give-up line can say why, not just that. A misconfigured
	// endpoint and a genuine outage otherwise log identically.
	var lastErr error

	for attempt := 0; attempt <= s.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff(attempt))
		}
		result, err := s.post(body)
		lastErr = err
		switch result {
		case postOK:
			s.sent.Add(int64(len(batch)))
			return
		case postPermanent:
			s.failed.Add(int64(len(batch)))
			s.cfg.Logger.Printf("sink: %s rejected batch of %d, not retrying: %v",
				s.cfg.IngestURL, len(batch), err)
			return
		case postRetry:
			// fall through to the next attempt
		}
	}

	s.failed.Add(int64(len(batch)))
	s.cfg.Logger.Printf("sink: gave up on batch of %d after %d attempts to %s: %v",
		len(batch), s.cfg.MaxRetries+1, s.cfg.IngestURL, lastErr)
}

// one HTTP POST. Reports whether it delivered, is worth retrying, or failed
// permanently (a 4xx --> retrying wont help), and why it failed.
func (s *HTTPSink) post(body []byte) (postResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.HTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.IngestURL, bytes.NewReader(body))
	if err != nil {
		return postPermanent, err // a request that cant build
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.ServiceToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.ServiceToken)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return postRetry, err // retry
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted:
		return postOK, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return postPermanent, fmt.Errorf("HTTP %s", resp.Status) // 4xx: dont retry
	default:
		return postRetry, fmt.Errorf("HTTP %s", resp.Status) // 5xx: retry
	}
}

// stops accepting events, flushes, waits for worker
// call on proxy shutdown.
func (s *HTTPSink) Close() {
	s.closeOnce.Do(func() {
		// Wake any blocked emitter, then wait for every in-flight Emit to leave the send path before the worker is allowed to finish draining.
		close(s.done)
		s.emitGate.Lock()
		s.closing = true
		s.emitGate.Unlock()
	})
	s.wg.Wait()
}

// snapshot of counters.
func (s *HTTPSink) Stats() Stats {
	return Stats{
		Emitted: s.emitted.Load(),
		Sent:    s.sent.Load(),
		Dropped: s.dropped.Load(),
		Failed:  s.failed.Load(),
	}
}

// QueueDepth is how many events are buffered and not yet picked up by the
// worker. An event the worker has already taken into a batch it is sending is
// not counted, so under a stalled API this settles at BufferSize, not above it.
func (s *HTTPSink) QueueDepth() int {
	return len(s.events)
}

// StatsProvider is what an operator-facing endpoint needs from a sink. It is
// separate from audit.Sink so the hot-path interface stays one method, and a
// sink with no counters (Fake) simply does not implement it.
type StatsProvider interface {
	Stats() Stats
	QueueDepth() int
}

var _ StatsProvider = (*HTTPSink)(nil)

// grows wait between retries (capped at 1s).
func backoff(attempt int) time.Duration {
	d := 50 * time.Millisecond * time.Duration(int64(1)<<uint(attempt-1))
	if d > time.Second {
		d = time.Second
	}
	return d
}
