package app

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"lustre/internal/sink"
)

// The admin server is the proxy's operator-facing HTTP surface: a liveness and
// readiness probe and the audit pipeline's counters. It listens on its own
// address (PROXY_ADMIN_ADDR), never the client-facing listeners, and is not
// published by the deployment: nothing on it is meant for clients, and the
// metrics describe how much audit data is being lost.

// adminState is what the admin handlers report. Run fills it in as the proxy
// comes up; the handlers only read it.
type adminState struct {
	mu              sync.RWMutex
	ready           bool
	reason          string
	stats           sink.StatsProvider
	appliedRevision func() int
}

// admin is the process-wide state behind the admin server. A package variable
// rather than a parameter so the two config sources (static file, live API) can
// report readiness without every function in between carrying it.
var admin = &adminState{reason: "starting"}

// setReady marks the proxy as serving: its configuration is applied and every
// listener is bound.
func (a *adminState) setReady() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ready, a.reason = true, ""
}

// setStats gives /metrics the audit sink's counters. A sink with none (the
// logging Fake) is simply not set, and /metrics then leaves those lines out.
func (a *adminState) setStats(s sink.StatsProvider) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stats = s
}

// setAppliedRevision gives /metrics the live config revision the proxy is
// running. Only the API-driven source has one.
func (a *adminState) setAppliedRevision(f func() int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appliedRevision = f
}

func (a *adminState) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.HandleFunc("/metrics", a.handleMetrics)
	return mux
}

// handleHealthz answers 200 once the proxy is serving and 503, with the reason,
// until then. It deliberately says nothing about the audit sink: a sink that is
// dropping or failing is something a restart does not fix, so it belongs in the
// metrics, where it can be alerted on, and not in a probe that would restart a
// healthy proxy.
func (a *adminState) handleHealthz(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	ready, reason := a.ready, a.reason
	a.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "not ready: %s\n", reason)
		return
	}
	fmt.Fprintln(w, "ok")
}

// handleMetrics writes the Prometheus text exposition format by hand: a handful
// of counters does not justify a client library.
func (a *adminState) handleMetrics(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	stats, revFn := a.stats, a.appliedRevision
	a.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metric := func(name, kind, help string, value any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, value)
	}

	if stats != nil {
		st := stats.Stats()
		metric("lustre_audit_queue_depth", "gauge", "Audit events buffered and not yet picked up for delivery.", stats.QueueDepth())
		metric("lustre_audit_emitted_total", "counter", "Audit events handed to the sink.", st.Emitted)
		metric("lustre_audit_sent_total", "counter", "Audit events delivered to the API.", st.Sent)
		metric("lustre_audit_dropped_total", "counter", "Audit events discarded: buffer full in drop mode, or sink shutting down.", st.Dropped)
		metric("lustre_audit_failed_total", "counter", "Audit events lost because delivery failed after retries.", st.Failed)
	}
	if revFn != nil {
		metric("lustre_config_applied_revision", "gauge", "Highest configuration revision fully applied by this proxy.", revFn())
	}
}

// startAdminServer binds addr and serves the admin handlers in the background.
// A bind failure is fatal: an operator who asked for an admin port and did not
// get one would otherwise find out only when the healthcheck started failing.
func startAdminServer(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("PROXY_ADMIN_ADDR=%s: %v", addr, err)
	}
	srv := &http.Server{Handler: admin.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("admin server stopped: %v", err)
		}
	}()
	log.Printf("admin server (healthz, metrics) listening on %s", ln.Addr())
}
