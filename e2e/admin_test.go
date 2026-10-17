package e2e

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// metricValue reads one metric from the admin server's text exposition, or -1
// if the metric is not there.
func (s *stack) metricValue(t *testing.T, name string) int64 {
	t.Helper()
	resp, err := http.Get(s.adminURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, name+" ") {
			v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, name+" ")), 10, 64)
			if err != nil {
				t.Fatalf("metric %s: %v", name, err)
			}
			return v
		}
	}
	return -1
}

// TestProxyAdminReportsHealthAndAuditCounters: the proxy answers its health
// probe once serving, and its audit counters move when it audits something —
// without anyone reading its logs.
func TestProxyAdminReportsHealthAndAuditCounters(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	resp, err := http.Get(s.adminURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d once the listeners are up, want 200", resp.StatusCode)
	}

	for _, m := range []string{
		"lustre_audit_queue_depth", "lustre_audit_emitted_total", "lustre_audit_sent_total",
		"lustre_audit_dropped_total", "lustre_audit_failed_total",
	} {
		if s.metricValue(t, m) < 0 {
			t.Errorf("/metrics has no %s", m)
		}
	}
	if got := s.metricValue(t, "lustre_config_applied_revision"); got < 1 {
		t.Errorf("lustre_config_applied_revision = %d, want at least 1 once the config is applied", got)
	}

	before := s.metricValue(t, "lustre_audit_emitted_total")
	db := s.openThroughProxy(t, a.name, aliceUser, alicePass(t), "")
	if err := db.Ping(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := db.Exec("SELECT 1"); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}

	deadline := time.Now().Add(auditTimeout)
	for s.metricValue(t, "lustre_audit_sent_total") < 1 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if after := s.metricValue(t, "lustre_audit_emitted_total"); after <= before {
		t.Errorf("emitted_total did not move after a login and a query: %d -> %d", before, after)
	}
	if got := s.metricValue(t, "lustre_audit_sent_total"); got < 1 {
		t.Errorf("sent_total = %d, want events delivered to the API", got)
	}
	if got := s.metricValue(t, "lustre_audit_dropped_total"); got != 0 {
		t.Errorf("dropped_total = %d in a healthy run, want 0", got)
	}
}
