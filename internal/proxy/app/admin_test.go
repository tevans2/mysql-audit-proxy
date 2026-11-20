package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tevans2/mysql-audit-proxy/internal/sink"
)

type fakeStats struct {
	st    sink.Stats
	depth int
}

func (f fakeStats) Stats() sink.Stats { return f.st }
func (f fakeStats) QueueDepth() int   { return f.depth }

func get(t *testing.T, a *adminState, path string) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	a.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr.Code, rr.Body.String()
}

// Until the proxy is serving, /healthz must say so: a probe that answered 200
// while the proxy was still waiting for its first configuration would mark it
// healthy before it could route a single connection.
func TestHealthzIsUnavailableUntilReady(t *testing.T) {
	a := &adminState{reason: "waiting for initial config"}

	code, body := get(t, a, "/healthz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("before ready: got %d, want 503", code)
	}
	if !strings.Contains(body, "waiting for initial config") {
		t.Errorf("before ready: body %q does not give the reason", body)
	}

	a.setReady()
	code, body = get(t, a, "/healthz")
	if code != http.StatusOK || !strings.Contains(body, "ok") {
		t.Errorf("after ready: got %d %q, want 200 ok", code, body)
	}
}

// A sink that is dropping and failing must not make the proxy unhealthy: a
// restart does not fix an unreachable API, and the signal belongs in /metrics.
func TestHealthzIgnoresTheSink(t *testing.T) {
	a := &adminState{}
	a.setReady()
	a.setStats(fakeStats{st: sink.Stats{Emitted: 100, Dropped: 100, Failed: 100}, depth: 1024})

	if code, _ := get(t, a, "/healthz"); code != http.StatusOK {
		t.Errorf("got %d with a failing sink, want 200", code)
	}
}

// The drop count is the point of the endpoint: silent data loss is the worst
// failure of an audit tool, so it has to be readable without the logs.
func TestMetricsExposeQueueDepthAndDropCount(t *testing.T) {
	a := &adminState{}
	a.setStats(fakeStats{st: sink.Stats{Emitted: 10, Sent: 6, Dropped: 3, Failed: 1}, depth: 7})

	code, body := get(t, a, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("got %d, want 200", code)
	}
	for _, want := range []string{
		"auditproxy_audit_queue_depth 7\n",
		"auditproxy_audit_emitted_total 10\n",
		"auditproxy_audit_sent_total 6\n",
		"auditproxy_audit_dropped_total 3\n",
		"auditproxy_audit_failed_total 1\n",
		"# TYPE auditproxy_audit_dropped_total counter\n",
		"# TYPE auditproxy_audit_queue_depth gauge\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "auditproxy_config_applied_revision") {
		t.Error("applied revision reported with no source for it")
	}
}

// With no sink counters (the logging sink) /metrics still works, and says
// nothing about queues it does not have, rather than reporting false zeros.
func TestMetricsWithoutASinkOmitsAuditLines(t *testing.T) {
	a := &adminState{}
	a.setAppliedRevision(func() int { return 5 })

	code, body := get(t, a, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("got %d, want 200", code)
	}
	if strings.Contains(body, "auditproxy_audit_") {
		t.Errorf("audit metrics reported with no sink behind them:\n%s", body)
	}
	if !strings.Contains(body, "auditproxy_config_applied_revision 5\n") {
		t.Errorf("applied revision missing:\n%s", body)
	}
}
