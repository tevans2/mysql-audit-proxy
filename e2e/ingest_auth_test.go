// Tests for POST /api/events authentication, end to end: the API refuses
// callers without the service token, and a real proxy still delivers its audit
// events because the sink presents one.
package e2e

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"lustre/internal/audit"
)

func postEvents(t *testing.T, url, authorization string, events []audit.Event) int {
	t.Helper()
	body, _ := json.Marshal(events)
	req, err := http.NewRequest(http.MethodPost, url+"/api/events", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// An unauthenticated caller must not be able to write audit rows, and must not
// be able to forge a connection into the log.
func TestEventsEndpointRejectsCallersWithoutTheServiceToken(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)

	forged := []audit.Event{{ConnID: "forged-conn", Kind: "connect", At: "2026-10-14T10:00:00.000000Z", User: "mallory", Upstream: "db-a"}}
	for name, header := range map[string]string{
		"no header":    "",
		"wrong token":  "Bearer not-the-token",
		"session auth": "Bearer " + s.token,
	} {
		if code := postEvents(t, s.apiURL, header, forged); code != http.StatusUnauthorized {
			t.Errorf("%s: POST /api/events = %d, want 401", name, code)
		}
	}

	var resp struct {
		Connections []audit.Connection `json:"connections"`
	}
	s.getJSON(t, "/api/connections", &resp)
	for _, c := range resp.Connections {
		if c.DBUser == "mallory" {
			t.Fatalf("a rejected POST still wrote a connection: %+v", c)
		}
	}
	if code := postEvents(t, s.apiURL, "Bearer "+e2eProxyToken, nil); code != http.StatusAccepted {
		t.Errorf("service token: POST /api/events = %d, want 202", code)
	}
}

// A file-mode proxy cannot be given API_PROXY_TOKEN (together with
// PROXY_CONFIG_FILE it is an ambiguous config), so SINK_TOKEN has to carry the
// credential on its own and the audit log still has to fill.
func TestFileModeProxyAuditsWithSinkToken(t *testing.T) {
	a, b := upstreams(t)
	s := startStack(t, a, b)
	proxyBin, _ := buildBinaries(t)

	host, portText, _ := net.SplitHostPort(a.addr)
	upPort, _ := strconv.Atoi(portText)
	listen := freePort(t)
	cfg := struct {
		Servers []audit.Server `json:"servers"`
	}{[]audit.Server{{
		Name: a.name, ListenPort: listen, UpstreamHost: host, UpstreamPort: upPort,
		UpstreamUser: bootstrapUser, Enabled: true,
		TLS: getEnv("E2E_TLS", "skip-verify"), TLSCAFile: os.Getenv("E2E_TLS_CA_FILE"),
	}}}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "upstreams.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	start(t, "file-mode proxy", proxyBin, []string{
		"PROXY_CONFIG_FILE=" + path,
		"PROXY_UPSTREAM_PASSWORDS=" + a.name + ":" + bootstrapPass(t, a),
		"SINK_INGEST_URL=" + s.apiURL + "/api/events",
		"SINK_TOKEN=" + e2eProxyToken,
		"SINK_BATCH_SIZE=1",
		"SINK_FLUSH_INTERVAL=200ms",
	})
	addr := fmt.Sprintf("127.0.0.1:%d", listen)
	waitTCP(t, "file-mode proxy", addr)

	db, err := sql.Open("mysql", aliceUser+":"+alicePass(t)+"@tcp("+addr+")/?tls=skip-verify&timeout=10s")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	const marker = "filemode_sink_token_marker"
	if _, err := db.Exec("SELECT 1 AS " + marker); err != nil {
		t.Fatal(err)
	}
	s.waitForStatement(t, marker)
}
