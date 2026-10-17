package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lustre/internal/audit"
)

// Same shape as deploy/upstreams.json: no passwords.
const seedFile = `{"servers":[
 {"id":1,"name":"db-a","listen_port":3307,"upstream_host":"db-a","upstream_port":3306,"upstream_user":"root","enabled":true},
 {"id":2,"name":"db-b","listen_port":3308,"upstream_host":"db-b","upstream_port":3306,"upstream_user":"root","enabled":true}
]}`

const (
	e2eProxyToken = "e2e-secret-token-123"
	e2eAdminHash  = "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW"
)

func apiEnv(port int, dbPath, seedPath string) []string {
	return append(os.Environ(),
		fmt.Sprintf("API_LISTEN_ADDR=127.0.0.1:%d", port),
		"API_DB_PATH="+dbPath,
		"API_ADMIN_PASS_HASH="+e2eAdminHash,
		"API_CONFIG_KEY=0123456789abcdef0123456789abcdef",
		"API_PROXY_TOKEN="+e2eProxyToken,
		"API_CONFIG_PATH="+seedPath,
		"API_UPSTREAM_PASSWORDS=db-a:pw-a,db-b:pw-b",
	)
}

// runAPI starts the API and returns its base URL and a stop func.
func runAPI(t *testing.T, apiBin, dbPath, seedPath string) (string, func()) {
	t.Helper()
	port := freePort(t)
	cmd := exec.Command(apiBin)
	cmd.Env = apiEnv(port, dbPath, seedPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting api: %v", err)
	}
	stop := func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
	t.Cleanup(stop)
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitHealthz(t, url)
	return url, stop
}

// login extracts the HttpOnly session cookie instead of a JSON response token.
func login(t *testing.T, url string) string {
	t.Helper()
	body, _ := json.Marshal(audit.LoginRequest{Username: "admin", Password: "admin123"})
	resp, err := http.Post(url+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The likely failure is a changed admin hash, and on that path err is
		// nil -- so reporting only err would say "<nil>" and name nothing.
		reason, _ := io.ReadAll(resp.Body)
		t.Fatalf("login failed with status %d: %s", resp.StatusCode, reason)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "session" {
			return c.Value
		}
	}
	t.Fatalf("session cookie not found in login response")
	return ""
}

// getInto sends an authenticated GET request using the provided session cookie value.
func getInto(t *testing.T, url, token string, out any) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: token})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		reason, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %d: %s", url, resp.StatusCode, reason)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
}

type uiServers struct {
	Servers []struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		Host        string `json:"upstream_host"`
		HasPassword bool   `json:"has_upstream_password"`
	} `json:"servers"`
}

/*
 * A fresh database must come up with the seeded servers, credentials included,
 * with nothing POSTed by hand; restarting must neither duplicate them nor
 * overwrite an operator's edit.
 */
func TestAPISeedsServersFromConfigPath(t *testing.T) {
	_, apiBin := buildBinaries(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "lustre.db")
	seedPath := filepath.Join(dir, "upstreams.json")
	if err := os.WriteFile(seedPath, []byte(seedFile), 0o600); err != nil {
		t.Fatal(err)
	}

	url, stop := runAPI(t, apiBin, dbPath, seedPath)
	token := login(t, url)

	var ui uiServers
	getInto(t, url+"/api/servers", token, &ui)
	if len(ui.Servers) != 2 || ui.Servers[0].Name != "db-a" || ui.Servers[1].Name != "db-b" {
		t.Fatalf("fresh DB: want db-a and db-b seeded, got %+v", ui.Servers)
	}
	for _, s := range ui.Servers {
		if !s.HasPassword {
			t.Errorf("%s seeded without a password", s.Name)
		}
	}

	// What the proxy would fetch: decrypted credentials, one revision bump.
	var pc struct {
		Revision int `json:"revision"`
		Servers  []struct {
			Name string `json:"name"`
			Pass string `json:"upstream_pass"`
		} `json:"servers"`
	}

	// Service token is intentionally sent as Bearer, as it is untouched by the CSRF/cookie migration
	reqProxy, _ := http.NewRequest("GET", url+"/api/proxy/config", nil)
	reqProxy.Header.Set("Authorization", "Bearer "+e2eProxyToken)
	respProxy, err := http.DefaultClient.Do(reqProxy)
	if err != nil {
		t.Fatalf("GET /api/proxy/config: %v", err)
	}
	defer respProxy.Body.Close()
	json.NewDecoder(respProxy.Body).Decode(&pc)

	if pc.Revision != 1 || len(pc.Servers) != 2 || pc.Servers[0].Pass != "pw-a" || pc.Servers[1].Pass != "pw-b" {
		t.Fatalf("proxy config after seed = %+v, want revision 1 and pw-a/pw-b", pc)
	}

	// Operator edit: repoint db-a. No password sent, so the stored one is kept.
	edit, _ := json.Marshal(map[string]any{
		"id": ui.Servers[0].ID, "name": "db-a", "listen_port": 3307,
		"upstream_host": "edited-host", "upstream_port": 3306,
		"upstream_user": "root", "enabled": true,
	})
	req, _ := http.NewRequest(http.MethodPut, url+"/api/servers", bytes.NewReader(edit))
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	req.Header.Set("X-Requested-With", "XMLHttpRequest") // Add CSRF protection header

	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /api/servers: %v", err)
	}
	if putResp.StatusCode != http.StatusOK {
		reason, _ := io.ReadAll(putResp.Body)
		t.Fatalf("PUT /api/servers: status %d: %s", putResp.StatusCode, reason)
	}
	putResp.Body.Close()

	stop()
	time.Sleep(200 * time.Millisecond)
	url, _ = runAPI(t, apiBin, dbPath, seedPath)
	token = login(t, url)

	getInto(t, url+"/api/servers", token, &ui)
	if len(ui.Servers) != 2 {
		t.Fatalf("after restart: %d servers, want 2 (no duplicates)", len(ui.Servers))
	}
	if ui.Servers[0].Host != "edited-host" {
		t.Errorf("restart overwrote the operator's edit: db-a host = %q", ui.Servers[0].Host)
	}

	reqProxy2, _ := http.NewRequest("GET", url+"/api/proxy/config", nil)
	reqProxy2.Header.Set("Authorization", "Bearer "+e2eProxyToken)
	respProxy2, err := http.DefaultClient.Do(reqProxy2)
	if err != nil {
		t.Fatalf("GET /api/proxy/config: %v", err)
	}
	defer respProxy2.Body.Close()
	json.NewDecoder(respProxy2.Body).Decode(&pc)

	if pc.Revision != 2 {
		t.Errorf("revision after restart = %d, want 2 (seed + edit, no reseed)", pc.Revision)
	}
}

// A seed file that can't be read must stop the API, not start it empty.
func TestAPIRefusesUnreadableSeedFile(t *testing.T) {
	_, apiBin := buildBinaries(t)
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "missing.json")
	cmd := exec.Command(apiBin)
	cmd.Env = apiEnv(freePort(t), filepath.Join(dir, "lustre.db"), seedPath)

	// Capture the output, because a non-zero exit on its own proves nothing:
	// any startup failure produces one -- a lost race for the port, a future
	// required env var -- so without checking what the API actually
	// complained about, this test stays green with the seed guard removed.
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("API exited 0 with an unreadable seed file; want non-zero. output:\n%s", out.String())
		}
		if !strings.Contains(out.String(), seedPath) {
			t.Errorf("API exited non-zero without naming the seed file, so it failed for some other reason.\nwant a mention of %s, got:\n%s", seedPath, out.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("API kept running with an unreadable seed file. output:\n%s", out.String())
	}
}

// Once the table is populated the seed file has done its job, so the API must
// no longer depend on it. Otherwise a later edit to upstreams.json, or a host
// file renamed out from under the bind mount, takes the back-office down with
// everything it needs already in the database.
func TestAPIStartsWithPopulatedTableAndBrokenSeedFile(t *testing.T) {
	_, apiBin := buildBinaries(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "lustre.db")
	seedPath := filepath.Join(dir, "upstreams.json")
	if err := os.WriteFile(seedPath, []byte(seedFile), 0o644); err != nil {
		t.Fatal(err)
	}

	url, stop := runAPI(t, apiBin, dbPath, seedPath)
	var ui struct {
		Servers []struct {
			Name string `json:"name"`
		} `json:"servers"`
	}
	getInto(t, url+"/api/servers", login(t, url), &ui)
	if len(ui.Servers) != 2 {
		t.Fatalf("first boot seeded %d servers, want 2", len(ui.Servers))
	}
	stop()
	time.Sleep(200 * time.Millisecond)

	// Every way the file can now go wrong, against the same populated database.
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T)
	}{
		{"removed", func(t *testing.T) { os.Remove(seedPath) }},
		{"unparseable", func(t *testing.T) { os.WriteFile(seedPath, []byte("{not json"), 0o644) }},
		{"emptied", func(t *testing.T) { os.WriteFile(seedPath, []byte(`{"servers":[]}`), 0o644) }},
		{"a directory", func(t *testing.T) { os.Remove(seedPath); os.Mkdir(seedPath, 0o755) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.RemoveAll(seedPath)
			tc.break_(t)

			url, stop := runAPI(t, apiBin, dbPath, seedPath) // fails the test if it never reports healthy
			defer stop()

			var after struct {
				Servers []struct {
					Name string `json:"name"`
				} `json:"servers"`
			}
			getInto(t, url+"/api/servers", login(t, url), &after)
			if len(after.Servers) != 2 {
				t.Errorf("servers after restart = %d, want the 2 already in the database", len(after.Servers))
			}
			stop()
			time.Sleep(200 * time.Millisecond)
		})
	}
}
