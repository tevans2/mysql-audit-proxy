package sink

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lustre/internal/audit"
)

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// A misconfigured endpoint must be rejected at construction. Left to the first
// flush it fails exactly like the API being down, so a typo reads as an outage
// and events are dropped with nothing pointing at the cause.
func TestNewRejectsBadIngestURL(t *testing.T) {
	bad := []struct{ name, url string }{
		{"empty", ""},
		{"no scheme", "localhost:8080/api/events"},
		{"wrong scheme", "ftp://localhost:8080/api/events"},
		{"no host", "http:///api/events"},
		{"unparseable", "http://%zz"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			s, err := New(Config{IngestURL: c.url, Logger: quietLogger()})
			if err == nil {
				s.Close()
				t.Fatalf("New(%q) succeeded, want an error", c.url)
			}
			if s != nil {
				t.Errorf("New(%q) returned a sink alongside its error", c.url)
			}
			t.Logf("%s -> %v", c.name, err)
		})
	}

	s, err := New(Config{IngestURL: "http://localhost:8080/api/events", Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New rejected a valid URL: %v", err)
	}
	s.Close()
}

// newTestSink builds a sink and fails the test if the config is rejected.
func newTestSink(t *testing.T, cfg Config) *HTTPSink {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// a log target that can be read while a worker is still writing to it.
type safeBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func sampleStatement(connID string) audit.Event {
	return audit.Event{
		ConnID: connID, Kind: audit.KindStatement, At: time.Now().UTC().Format(time.RFC3339Nano),
		SQL: "SELECT 1", StmtType: "SELECT", DB: "sales",
	}
}

type countingServer struct {
	events   atomic.Int64
	requests atomic.Int64
}

func (c *countingServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var batch []audit.Event
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.requests.Add(1)
		c.events.Add(int64(len(batch)))
		w.WriteHeader(http.StatusAccepted)
	}
}

// server that hangs forever, the worker gets stuck
// the first POST and the buffer fills.
// emit must still return immediately dropping
func TestEmitNeverBlocksInDropMode(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	s := newTestSink(t, Config{IngestURL: srv.URL, BufferSize: 4, BatchSize: 2, HTTPTimeout: 50 * time.Millisecond, MaxRetries: 1, OnFull: Drop, Logger: quietLogger()})
	defer s.Close()

	const n = 5000
	start := time.Now()
	for i := 0; i < n; i++ {
		s.Emit(sampleStatement("c-block"))
	}
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("Emit blocked: %d events took %v", n, elapsed)
	}
	if got := s.Stats().Emitted; got != n {
		t.Fatalf("Emitted = %d, want %d", got, n)
	}
	if s.Stats().Dropped == 0 {
		t.Fatal("expected drops while the server hung, got 0")
	}
	t.Logf("emit of %d events took %v; dropped %d (as designed)", n, elapsed, s.Stats().Dropped)
}
func TestEmitBlocksWhenBufferFullInBlockMode(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	s := newTestSink(t, Config{IngestURL: srv.URL, BufferSize: 4, BatchSize: 2, HTTPTimeout: 50 * time.Millisecond, MaxRetries: 1, Logger: quietLogger()})
	defer s.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			s.Emit(sampleStatement("c-block"))
		}
	}()

	select {
	case <-done:
		t.Fatal("Emit did not block once the buffer filled")
	case <-time.After(200 * time.Millisecond):
	}
}

// Healthy server-->every event arrives, grouped into batches.
func TestBatchesDelivered(t *testing.T) {
	cs := &countingServer{}
	srv := httptest.NewServer(cs.handler())
	defer srv.Close()

	s := newTestSink(t, Config{IngestURL: srv.URL, BatchSize: 10, FlushInterval: 20 * time.Millisecond, Logger: quietLogger()})

	const n = 100
	for i := 0; i < n; i++ {
		s.Emit(sampleStatement("c-happy"))
	}
	s.Close()

	if got := cs.events.Load(); got != n {
		t.Fatalf("server received %d events, want %d", got, n)
	}
	if got := s.Stats().Sent; got != n {
		t.Fatalf("Sent = %d, want %d", got, n)
	}
	if reqs := cs.requests.Load(); reqs >= n {
		t.Fatalf("expected batching, got %d requests for %d events", reqs, n)
	}
	t.Logf("delivered %d events in %d batched requests", cs.events.Load(), cs.requests.Load())
}

// Fewer events than a batch--> must still deliver
func TestFlushOnClose(t *testing.T) {
	cs := &countingServer{}
	srv := httptest.NewServer(cs.handler())
	defer srv.Close()

	s := newTestSink(t, Config{IngestURL: srv.URL, BatchSize: 100, FlushInterval: time.Hour, Logger: quietLogger()})
	for i := 0; i < 3; i++ {
		s.Emit(sampleStatement("c-tail"))
	}
	s.Close()

	if got := cs.events.Load(); got != 3 {
		t.Fatalf("after Close, server received %d events, want 3", got)
	}
}

// API always 500s-->nothing blocks, Close returns, given-up events show as Failed.
func TestFailSafeWhenAPIDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newTestSink(t, Config{IngestURL: srv.URL, BatchSize: 5, FlushInterval: 10 * time.Millisecond, MaxRetries: 1, HTTPTimeout: 50 * time.Millisecond, Logger: quietLogger()})

	const n = 20
	for i := 0; i < n; i++ {
		s.Emit(sampleStatement("c-down"))
	}
	s.Close()

	st := s.Stats()
	if st.Sent != 0 {
		t.Fatalf("Sent = %d, want 0", st.Sent)
	}
	if st.Failed == 0 {
		t.Fatal("expected Failed > 0 when the API is down, got 0")
	}
	if st.Dropped+st.Failed != st.Emitted {
		t.Fatalf("accounting off: dropped(%d)+failed(%d) != emitted(%d)", st.Dropped, st.Failed, st.Emitted)
	}
}

// Many goroutines emitting at once must be race-free (run with -race).
func TestConcurrentEmit(t *testing.T) {
	cs := &countingServer{}
	srv := httptest.NewServer(cs.handler())
	defer srv.Close()

	s := newTestSink(t, Config{IngestURL: srv.URL, BatchSize: 20, FlushInterval: 10 * time.Millisecond, Logger: quietLogger()})

	var wg sync.WaitGroup
	const goroutines, each = 10, 100
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				s.Emit(sampleStatement("c-race"))
			}
		}()
	}
	wg.Wait()
	s.Close()

	if got := s.Stats().Emitted; got != goroutines*each {
		t.Fatalf("Emitted = %d, want %d", got, goroutines*each)
	}
}

// The proxy emits the disconnect event from the per-connection goroutine during
// teardown, so on shutdown those are still draining while main calls Close().
// A late Emit must be counted as dropped, never panic -- auditing cannot be
// allowed to take the proxy down.
func TestEmitAfterCloseDoesNotPanic(t *testing.T) {
	cs := &countingServer{}
	srv := httptest.NewServer(cs.handler())
	defer srv.Close()

	s := newTestSink(t, Config{IngestURL: srv.URL, FlushInterval: 10 * time.Millisecond, Logger: quietLogger()})
	s.Emit(sampleStatement("c-early"))
	s.Close()

	s.Emit(sampleStatement("c-late")) // late disconnect

	st := s.Stats()
	if st.Emitted != 2 {
		t.Fatalf("Emitted = %d, want 2", st.Emitted)
	}
	if st.Dropped != 1 {
		t.Fatalf("late event should be dropped: Dropped = %d, want 1", st.Dropped)
	}
	if got := cs.events.Load(); got != 1 {
		t.Fatalf("server received %d events, want only the pre-Close one", got)
	}
}

// Drop tracking is per sink, not package state. A quieter second sink in the
// same process must still report its own drops -- if the counter is shared, its
// delta never clears the louder sink's and it goes silent forever.
func TestDropLoggingIsPerSink(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	defer srv.Close()
	defer close(hang)

	newSink := func(out *safeBuf) *HTTPSink {
		return newTestSink(t, Config{IngestURL: srv.URL, BufferSize: 2, BatchSize: 1,
			FlushInterval: 10 * time.Millisecond, HTTPTimeout: 20 * time.Millisecond,
			MaxRetries: 1, OnFull: Drop, Logger: log.New(out, "", 0)})
	}

	// the loud one first, so a shared counter would be left high.
	var loudLog safeBuf
	loud := newSink(&loudLog)
	for i := 0; i < 500; i++ {
		loud.Emit(sampleStatement("c-loud"))
	}
	time.Sleep(300 * time.Millisecond)

	var quietLog safeBuf
	quiet := newSink(&quietLog)
	for i := 0; i < 50; i++ {
		quiet.Emit(sampleStatement("c-quiet"))
	}
	time.Sleep(300 * time.Millisecond)

	if quiet.Stats().Dropped == 0 {
		t.Fatal("expected the quiet sink to drop while the server hung, got 0")
	}
	if !strings.Contains(quietLog.String(), "dropped") {
		t.Fatalf("quiet sink dropped %d events but logged no drop line; loud sink dropped %d",
			quiet.Stats().Dropped, loud.Stats().Dropped)
	}
}

// Several sinks ticking at once --> catches drop tracking being shared across
// instances rather than owned by each worker (run with -race).
func TestConcurrentSinksDoNotShareDropState(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	defer srv.Close()
	defer close(hang)

	var wg sync.WaitGroup
	const sinks = 8
	for i := 0; i < sinks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := New(Config{IngestURL: srv.URL, BufferSize: 1, BatchSize: 1,
				FlushInterval: time.Millisecond, HTTPTimeout: 10 * time.Millisecond,
				MaxRetries: 1, Logger: quietLogger()})
			if err != nil {
				t.Errorf("New: %v", err) // t.Fatalf is not valid off the test goroutine
				return
			}
			deadline := time.Now().Add(200 * time.Millisecond)
			for time.Now().Before(deadline) {
				for j := 0; j < 200; j++ {
					s.Emit(sampleStatement("c-shared"))
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()

}

// Close must return while an Emit is blocked on a full buffer, counting the
// event as dropped rather than deadlocking (and the blocked send must not
// land in the channel behind the worker's drain, where nothing would read it).
func TestCloseWhileEmitBlockedDropsAndReturns(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	defer srv.Close()
	defer close(hang)

	s := newTestSink(t, Config{IngestURL: srv.URL, BufferSize: 2, BatchSize: 1,
		FlushInterval: 10 * time.Millisecond, HTTPTimeout: 20 * time.Millisecond,
		MaxRetries: 1, Logger: quietLogger()})

	emitting := make(chan struct{})
	go func() {
		defer close(emitting)
		for i := 0; i < 200; i++ {
			s.Emit(sampleStatement("c-blocked"))
		}
	}()

	time.Sleep(100 * time.Millisecond) // let it fill and block

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		s.Close()
	}()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked while an Emit was blocked")
	}

	select {
	case <-emitting:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit never returned after Close")
	}

	st := s.Stats()
	if st.Dropped == 0 {
		t.Fatal("expected blocked events to be counted as dropped, got 0")
	}
	// Nothing may go unaccounted: an event that won its send after the
	// worker stopped draining would be neither sent, dropped nor failed.
	if st.Sent+st.Dropped+st.Failed != st.Emitted {
		t.Fatalf("accounting off: sent(%d)+dropped(%d)+failed(%d) != emitted(%d)",
			st.Sent, st.Dropped, st.Failed, st.Emitted)
	}
}

// A blocked Emit must complete once the backlog clears, rather than staying
// blocked after the API recovers.
func TestBlockedEmitCompletesWhenAPIRecovers(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer once.Do(func() { close(release) })

	s := newTestSink(t, Config{IngestURL: srv.URL, BufferSize: 2, BatchSize: 1,
		FlushInterval: 10 * time.Millisecond, HTTPTimeout: 50 * time.Millisecond,
		MaxRetries: 1, Logger: quietLogger()})
	defer s.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			s.Emit(sampleStatement("c-recover"))
		}
	}()

	select {
	case <-done:
		t.Fatal("Emit did not block once the buffer filled")
	case <-time.After(200 * time.Millisecond):
	}

	once.Do(func() { close(release) }) // API recovers

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit stayed blocked after the API recovered")
	}
}

// The default path must still not block while there is buffer space, the
// guarantee the drop-mode test no longer covers now that it sets OnFull.
func TestEmitDoesNotBlockWithBufferSpaceInBlockMode(t *testing.T) {
	cs := &countingServer{}
	srv := httptest.NewServer(cs.handler())
	defer srv.Close()

	s := newTestSink(t, Config{IngestURL: srv.URL, BufferSize: 1024, BatchSize: 50,
		FlushInterval: 10 * time.Millisecond, Logger: quietLogger()})
	defer s.Close()

	const n = 500
	start := time.Now()
	for i := 0; i < n; i++ {
		s.Emit(sampleStatement("c-roomy"))
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Emit blocked with buffer space available: %d events took %v", n, elapsed)
	}
}

// QueueDepth is the figure an operator watches for a backed-up sink: it must
// reach the buffer's capacity when the API stalls, and never exceed it.
func TestQueueDepthFillsToCapacityWhenTheAPIStalls(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	s := newTestSink(t, Config{IngestURL: srv.URL, BufferSize: 4, BatchSize: 2, HTTPTimeout: 50 * time.Millisecond, MaxRetries: 1, OnFull: Drop, Logger: quietLogger()})
	defer s.Close()

	if got := s.QueueDepth(); got != 0 {
		t.Fatalf("QueueDepth on an idle sink = %d, want 0", got)
	}

	for i := 0; i < 50; i++ {
		s.Emit(sampleStatement("c-depth"))
	}

	deadline := time.Now().Add(2 * time.Second)
	for s.QueueDepth() != 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.QueueDepth(); got != 4 {
		t.Fatalf("QueueDepth with a stalled API = %d, want the buffer's capacity (4)", got)
	}
	if s.Stats().Dropped == 0 {
		t.Error("expected drops once the buffer was full")
	}
}

// authHeaders runs one event through a sink configured with token and returns
// the Authorization header each POST carried.
func authHeaders(t *testing.T, token string) []string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	s := newTestSink(t, Config{
		IngestURL:     srv.URL,
		ServiceToken:  token,
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
		Logger:        quietLogger(),
	})
	s.Emit(sampleStatement("auth"))
	s.Close()

	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), seen...)
}

func TestSinkSendsBearerTokenWhenConfigured(t *testing.T) {
	got := authHeaders(t, "test-proxy-token")
	if len(got) == 0 {
		t.Fatal("sink never posted")
	}
	for _, h := range got {
		if h != "Bearer test-proxy-token" {
			t.Errorf("Authorization = %q, want %q", h, "Bearer test-proxy-token")
		}
	}
}

func TestSinkSendsNoAuthorizationWithoutAToken(t *testing.T) {
	got := authHeaders(t, "")
	if len(got) == 0 {
		t.Fatal("sink never posted")
	}
	for _, h := range got {
		if h != "" {
			t.Errorf("Authorization = %q, want none", h)
		}
	}
}
