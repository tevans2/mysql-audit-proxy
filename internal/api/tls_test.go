package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/store"
)

func TestServerTLSContract(t *testing.T) {
	db, err := store.InitDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &API{Store: db}
	a.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte("admin-token")))
	db.CreateSession(tokenHash, "admin", time.Now().Add(time.Hour).Format(time.RFC3339))
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		if path == "/api/proxy/config" {
			r.Header.Set("Authorization", "Bearer "+token)
			a.HandleProxyConfig(w, r)
		} else {
			r.AddCookie(&http.Cookie{Name: "session", Value: token})
			r.Header.Set("X-Requested-With", "XMLHttpRequest")
			a.HandleServers(w, r)
		}
		return w
	}
	base := map[string]any{"name": "db-a", "listen_port": 3307, "upstream_host": "db-a", "upstream_port": 3306, "upstream_user": "bootstrap", "upstream_pass": "secret", "enabled": true}
	send := func(method string) *httptest.ResponseRecorder {
		data, _ := json.Marshal(base)
		return request(method, "/api/servers", string(data), "admin-token")
	}
	if w := send("POST"); w.Code != 200 {
		t.Fatalf("create: %s", w.Body.String())
	}
	read := func(path, token string) audit.Server {
		t.Helper()
		w := request("GET", path, "", token)
		if w.Code != 200 {
			t.Fatalf("GET: %s", w.Body.String())
		}
		var data struct {
			Servers []audit.Server `json:"servers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Servers) != 1 {
			t.Fatal("missing server")
		}
		return data.Servers[0]
	}
	srv := read("/api/servers", "admin-token")
	if srv.TLS != "verify" || srv.TLSCAFile != "" {
		t.Fatalf("wrong defaults: %q %q", srv.TLS, srv.TLSCAFile)
	}
	base["id"] = srv.ID
	base["upstream_pass"] = ""
	base["tls"] = "skip-verify"
	base["tls_ca_file"] = "/certs/test.pem"
	if w := send("PUT"); w.Code != 200 {
		t.Fatalf("update: %s", w.Body.String())
	}
	for _, endpoint := range []struct{ path, token string }{{"/api/servers", "admin-token"}, {"/api/proxy/config", "proxy-token"}} {
		got := read(endpoint.path, endpoint.token)
		if got.TLS != "skip-verify" || got.TLSCAFile != "/certs/test.pem" {
			t.Fatal("TLS fields lost on API response")
		}
	}
	delete(base, "tls")
	delete(base, "tls_ca_file")
	if w := send("PUT"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	srv = read("/api/servers", "admin-token")
	if srv.TLS != "skip-verify" || srv.TLSCAFile != "/certs/test.pem" {
		t.Fatal("omitted update erased TLS fields")
	}
	base["tls"] = "verify"
	base["tls_ca_file"] = ""
	if w := send("PUT"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	srv = read("/api/servers", "admin-token")
	if srv.TLS != "verify" || srv.TLSCAFile != "" {
		t.Fatal("explicit clear failed")
	}
	base["tls"] = "disabled"
	if w := send("PUT"); w.Code != 400 {
		t.Fatalf("plaintext mode accepted: %d", w.Code)
	}
	w := request("GET", "/api/servers", "", "admin-token")
	if bytes.Contains(w.Body.Bytes(), []byte(`"upstream_pass"`)) {
		t.Fatal("UI response exposes password field")
	}
}
