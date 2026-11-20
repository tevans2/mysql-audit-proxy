package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// createTestSession inserts a session into the database for testing auth logic.
func createTestSession(t *testing.T, st *store.Store, username, token string) {
	t.Helper()
	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	expiresAt := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	if err := st.CreateSession(tokenHash, username, expiresAt); err != nil {
		t.Fatalf("failed to create test session for %q: %v", username, err)
	}
}

/*
 * TestAPI_Login verifies that the POST /api/login endpoint correctly authenticates
 * an administrator using their password and returns a valid session token.
 */
func TestAPI_Login(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()

	loginReq := audit.LoginRequest{Username: "admin", Password: "admin123"}
	body, _ := json.Marshal(loginReq)
	req, _ := http.NewRequest("POST", "/api/login", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()
	api.HandleLogin(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from HandleLogin, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp audit.LoginResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}
	if resp.Username != "admin" {
		t.Errorf("username = %q, want admin", resp.Username)
	}
	if strings.Contains(rr.Body.String(), "token") {
		t.Errorf("the token must not appear in the response body: %s", rr.Body.String())
	}

	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "session" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("Expected a session cookie")
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie must be HttpOnly, Secure and SameSite=Strict: %+v", cookie)
	}
	if cookie.Expires.Before(time.Now().Add(23 * time.Hour)) {
		t.Errorf("cookie expires too soon: %v", cookie.Expires)
	}

	// The cookie must be accepted, and only the cookie: its value as a Bearer
	// token is not a credential any more.
	authed := func(setup func(*http.Request)) int {
		r, _ := http.NewRequest("GET", "/api/servers", nil)
		setup(r)
		w := httptest.NewRecorder()
		api.HandleServers(w, r)
		return w.Code
	}
	if code := authed(func(r *http.Request) { r.AddCookie(cookie) }); code != http.StatusOK {
		t.Errorf("session cookie: got %d, want 200", code)
	}
	if code := authed(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+cookie.Value) }); code != http.StatusUnauthorized {
		t.Errorf("session token as Bearer: got %d, want 401", code)
	}
}

/*
 * TestAPI_RoundTrip tests a series of end-to-end API operations including
 * authentication enforcement, event ingestion, connection listing, pagination limits,
 * statement formatting constraints, and UI password sanitization.
 */
func TestAPI_RoundTrip(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{
		Store: testStore,
	}

	dummyKey := make([]byte, 32)
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", dummyKey, "proxy-token")
	defer stop()

	validCiphertext, _ := encryptAES("test-password", dummyKey)
	testStore.AddServer(audit.Server{
		Name: "db-a", ListenPort: 3307, UpstreamHost: "demo-mysql", UpstreamPort: 3306, Enabled: true,
	}, validCiphertext)

	createTestSession(t, testStore, "admin", "test-token-123")

	authReq := func(method, url string, body io.Reader) *http.Request {
		req, _ := http.NewRequest(method, url, body)
		req.AddCookie(&http.Cookie{Name: "session", Value: "test-token-123"})
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		return req
	}

	unauthEndpoints := []string{"/api/connections", "/api/statements", "/api/servers"}
	for _, endpoint := range unauthEndpoints {
		req, _ := http.NewRequest("GET", endpoint, nil)
		rr := httptest.NewRecorder()
		if endpoint == "/api/connections" {
			api.HandleConnections(rr, req)
		} else if endpoint == "/api/statements" {
			api.HandleStatements(rr, req)
		} else if endpoint == "/api/servers" {
			api.HandleServers(rr, req)
		}
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Expected 401 for unauthenticated GET %s, got %d", endpoint, rr.Code)
		}
	}

	reqEvents, _ := http.NewRequest("POST", "/api/events", bytes.NewBufferString("[]"))
	rrEvents := httptest.NewRecorder()
	api.HandleEvents(rrEvents, reqEvents)
	if rrEvents.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for unauthenticated POST /api/events, got %d", rrEvents.Code)
	}

	req, _ := http.NewRequest("POST", "/api/events", bytes.NewBuffer([]byte(`[{bad json`)))
	req.Header.Set("Authorization", "Bearer proxy-token")
	rr := httptest.NewRecorder()
	api.HandleEvents(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for malformed payload, got %d", rr.Code)
	}

	validJSON := []byte(`[
		{"conn_id":"uuid-1", "kind":"auth_fail", "user":"bob", "at":"2026-08-10T10:00:00Z", "auth_result":"failed"},
		{"conn_id":"uuid-2", "kind":"connect", "user":"alice", "at":"2026-08-10T11:00:00.100000Z", "upstream":"db1"},
		{"conn_id":"uuid-2", "kind":"statement", "sql":"SELECT 1", "at":"2026-08-10T11:05:00Z", "stmt_type":"SELECT"},
		{"conn_id":"uuid-2", "kind":"statement", "sql":"SELECT 2", "at":"2026-08-10T11:06:00Z", "stmt_type":"SELECT", "db":"sales", "err":"syntax error"},
		{"conn_id":"uuid-2", "kind":"disconnect", "at":"2026-08-10T12:00:00Z"}
	]`)

	req2, _ := http.NewRequest("POST", "/api/events", bytes.NewBuffer(validJSON))
	req2.Header.Set("Authorization", "Bearer proxy-token")
	rr2 := httptest.NewRecorder()
	api.HandleEvents(rr2, req2)

	if rr2.Code != http.StatusAccepted {
		t.Fatalf("Expected 202 Accepted, got %v", rr2.Code)
	}

	req3 := authReq("GET", "/api/connections", nil)
	rr3 := httptest.NewRecorder()
	api.HandleConnections(rr3, req3)

	var listResp struct {
		Total       int                      `json:"total"`
		Connections []map[string]interface{} `json:"connections"`
	}
	json.NewDecoder(rr3.Body).Decode(&listResp)

	if listResp.Total != 2 {
		t.Fatalf("Expected 2 connections, got %d", listResp.Total)
	}

	// Assert the JSON correctly serializes the 6-digit precision without truncation
	for _, c := range listResp.Connections {
		if c["conn_id"] == "uuid-2" {
			expectedTs := "2026-08-10T11:00:00.100000Z"
			if c["ts_connect"] != expectedTs {
				t.Errorf("API JSON contract failure: got %v, want %q", c["ts_connect"], expectedTs)
			}
		}
	}

	// 4. Verify idempotent retries leave the database unchanged rather than erroring
	req4, _ := http.NewRequest("POST", "/api/events", bytes.NewBuffer(validJSON))
	req4.Header.Set("Authorization", "Bearer proxy-token")
	rr4 := httptest.NewRecorder()
	api.HandleEvents(rr4, req4)
	if rr4.Code != http.StatusAccepted {
		t.Errorf("Idempotency failed: expected 202 on retry, got %d", rr4.Code)
	}

	req5 := authReq("GET", "/api/connections", nil)
	rr5 := httptest.NewRecorder()
	api.HandleConnections(rr5, req5)

	var afterRetry struct {
		Total       int `json:"total"`
		Connections []struct {
			ConnID         string `json:"conn_id"`
			StatementCount int    `json:"statement_count"`
		} `json:"connections"`
	}
	json.NewDecoder(rr5.Body).Decode(&afterRetry)

	if afterRetry.Total != 2 {
		t.Errorf("Retry changed the connection count: got %d, want 2", afterRetry.Total)
	}
	for _, c := range afterRetry.Connections {
		if c.ConnID == "uuid-2" && c.StatementCount != 2 {
			t.Errorf("Retry duplicated statements for %s: statement_count = %d, want 2",
				c.ConnID, c.StatementCount)
		}
	}

	req6 := authReq("GET", "/api/connections?limit=abc", nil)
	rr6 := httptest.NewRecorder()
	api.HandleConnections(rr6, req6)
	if rr6.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for bad limit param, got %d", rr6.Code)
	}

	req7 := authReq("GET", "/api/statements", nil)
	rr7 := httptest.NewRecorder()
	api.HandleStatements(rr7, req7)

	var stmtsResp struct {
		Total      int                      `json:"total"`
		Statements []map[string]interface{} `json:"statements"`
	}
	json.NewDecoder(rr7.Body).Decode(&stmtsResp)

	if stmtsResp.Total != 2 {
		t.Errorf("Expected 2 statement, got total=%d", stmtsResp.Total)
	}

	expectedKeys := map[string]bool{
		"id": true, "conn_id": true, "ts": true, "db_user": true,
		"upstream": true, "db": true, "sql": true, "stmt_type": true, "error": true,
	}

	for _, stmtMap := range stmtsResp.Statements {
		gotKeys := make(map[string]bool)
		for k := range stmtMap {
			gotKeys[k] = true
		}
		if !reflect.DeepEqual(gotKeys, expectedKeys) {
			t.Errorf("Statement shape mismatch.\n got keys: %v\nwant keys: %v", gotKeys, expectedKeys)
		}

		sqlText := stmtMap["sql"].(string)
		if sqlText == "SELECT 1" {
			if stmtMap["db"] != nil {
				t.Errorf("Expected 'db' to be null for SELECT 1, got %v", stmtMap["db"])
			}
			if stmtMap["error"] != nil {
				t.Errorf("Expected 'error' to be null for SELECT 1, got %v", stmtMap["error"])
			}
		} else if sqlText == "SELECT 2" {
			if stmtMap["db"] != "sales" {
				t.Errorf("Expected 'db' to be 'sales' for SELECT 2, got %v", stmtMap["db"])
			}
			if stmtMap["error"] != "syntax error" {
				t.Errorf("Expected 'error' to be 'syntax error' for SELECT 2, got %v", stmtMap["error"])
			}
		}
	}

	req8 := authReq("GET", "/api/servers", nil)
	rr8 := httptest.NewRecorder()
	api.HandleServers(rr8, req8)

	var uiResp map[string]interface{}
	json.NewDecoder(rr8.Body).Decode(&uiResp)
	servers := uiResp["servers"].([]interface{})
	if len(servers) != 1 {
		t.Fatalf("Expected 1 configured server, got %d", len(servers))
	}

	srv := servers[0].(map[string]interface{})
	if srv["name"] != "db-a" {
		t.Errorf("Expected server name 'db-a', got %v", srv["name"])
	}
	if srv["upstream_pass"] != nil {
		t.Errorf("UI endpoint /api/servers MUST NOT return passwords!")
	}
	if srv["has_upstream_password"] != true {
		t.Errorf("UI endpoint missing 'has_upstream_password' boolean flag")
	}
}

/*
 * TestAPI_ProxyConfigAndMutations validates input validation rules (ports, conflicts),
 * service token authentication, revision increments, PUT password preservation behavior,
 * and ETag conditional headers (304 Not Modified responses) for the proxy config endpoint.
 */
func TestAPI_ProxyConfigAndMutations(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	dummyKey := make([]byte, 32)
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", dummyKey, "valid-service-token")
	defer stop()

	createTestSession(t, testStore, "admin", "admin-token")

	authReq := func(method, bodyStr string) *http.Request {
		var body io.Reader
		if bodyStr != "" {
			body = bytes.NewBufferString(bodyStr)
		}
		req, _ := http.NewRequest(method, "/api/servers", body)
		req.AddCookie(&http.Cookie{Name: "session", Value: "admin-token"})
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		return req
	}

	proxyReq := func(etag string) *http.Request {
		req, _ := http.NewRequest("GET", "/api/proxy/config", nil)
		req.Header.Set("Authorization", "Bearer valid-service-token")
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		return req
	}

	req1 := authReq("POST", `{"name": "test", "listen_port": 0, "upstream_host": "db", "upstream_user": "u", "upstream_pass": "p"}`)
	rr1 := httptest.NewRecorder()
	api.HandleServers(rr1, req1)
	if rr1.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for invalid port, got %d", rr1.Code)
	}

	req2 := authReq("POST", `{"name": "srv1", "listen_port": 3306, "upstream_host": "db", "upstream_port": 3306, "upstream_user": "u", "upstream_pass": "secret", "enabled": true}`)
	rr2 := httptest.NewRecorder()
	api.HandleServers(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for valid POST, got %d", rr2.Code)
	}

	req3 := authReq("POST", `{"name": "srv2", "listen_port": 3306, "upstream_host": "db", "upstream_port": 3307, "upstream_user": "u", "upstream_pass": "p"}`)
	rr3 := httptest.NewRecorder()
	api.HandleServers(rr3, req3)
	if rr3.Code != http.StatusConflict {
		t.Errorf("Expected 409 Conflict for duplicate port, got %d", rr3.Code)
	}

	req4, _ := http.NewRequest("GET", "/api/proxy/config", nil)
	req4.AddCookie(&http.Cookie{Name: "session", Value: "admin-token"})
	req4.Header.Set("X-Requested-With", "XMLHttpRequest")
	rr4 := httptest.NewRecorder()
	api.HandleProxyConfig(rr4, req4)
	if rr4.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 using admin UI token for proxy config, got %d", rr4.Code)
	}

	req5 := proxyReq("")
	rr5 := httptest.NewRecorder()
	api.HandleProxyConfig(rr5, req5)
	if rr5.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for valid proxy config fetch, got %d", rr5.Code)
	}
	etag := rr5.Header().Get("ETag")
	if etag == "" {
		t.Errorf("Missing ETag header on config response")
	}

	var proxyResp struct {
		Revision int            `json:"revision"`
		Servers  []audit.Server `json:"servers"`
	}
	json.NewDecoder(rr5.Body).Decode(&proxyResp)
	if proxyResp.Revision != 1 {
		t.Errorf("Expected config revision 1, got %d", proxyResp.Revision)
	}
	if len(proxyResp.Servers) != 1 || proxyResp.Servers[0].UpstreamPass != "secret" {
		t.Errorf("Proxy config failed to return the decrypted password")
	}

	req6 := proxyReq(etag)
	rr6 := httptest.NewRecorder()
	api.HandleProxyConfig(rr6, req6)
	if rr6.Code != http.StatusNotModified {
		t.Errorf("Expected 304 Not Modified for matching ETag, got %d", rr6.Code)
	}

	req7 := authReq("PUT", `{"id": 1, "name": "srv1-renamed", "listen_port": 3306, "upstream_host": "db", "upstream_port": 3306, "upstream_user": "u", "upstream_pass": "", "enabled": false}`)
	rr7 := httptest.NewRecorder()
	api.HandleServers(rr7, req7)
	if rr7.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK on PUT, got %d", rr7.Code)
	}

	req8 := proxyReq("")
	rr8 := httptest.NewRecorder()
	api.HandleProxyConfig(rr8, req8)
	json.NewDecoder(rr8.Body).Decode(&proxyResp)
	if proxyResp.Revision != 2 {
		t.Errorf("Expected revision 2 after PUT, got %d", proxyResp.Revision)
	}
	if proxyResp.Servers[0].Name != "srv1-renamed" || proxyResp.Servers[0].UpstreamPass != "secret" {
		t.Errorf("PUT failed to preserve old password when empty string passed")
	}

	req9, _ := http.NewRequest("DELETE", "/api/servers?id=1", nil)
	req9.AddCookie(&http.Cookie{Name: "session", Value: "admin-token"})
	req9.Header.Set("X-Requested-With", "XMLHttpRequest")
	rr9 := httptest.NewRecorder()
	api.HandleServers(rr9, req9)
	if rr9.Code != http.StatusOK {
		t.Errorf("Expected 200 OK on DELETE, got %d", rr9.Code)
	}

	rev, _ := testStore.GetRevision()
	if rev != 3 {
		t.Errorf("Expected final revision 3 after DELETE, got %d", rev)
	}
}

/*
 * TestAPI_ProxyConfigETagWeakValidatorAndMultiValue guards the If-None-Match parsing
 * flagged in review: a weak validator (W/"...") and a comma-separated multi-value
 * header must still match the strong ETag, or a compliant HTTP cache sends the full
 * config on every poll instead of getting 304s. A genuinely stale etag must still
 * fall through to a fresh 200.
 */
func TestAPI_ProxyConfigETagWeakValidatorAndMultiValue(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	dummyKey := make([]byte, 32)
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", dummyKey, "valid-service-token")
	defer stop()

	proxyReq := func(ifNoneMatch string) *http.Request {
		req, _ := http.NewRequest("GET", "/api/proxy/config", nil)
		req.Header.Set("Authorization", "Bearer valid-service-token")
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		return req
	}

	// Learn the current strong ETag from a normal fetch.
	rr1 := httptest.NewRecorder()
	api.HandleProxyConfig(rr1, proxyReq(""))
	if rr1.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for initial fetch, got %d", rr1.Code)
	}
	etag := rr1.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("Missing ETag header on initial fetch")
	}

	// A weak validator wrapping the same value must still be treated as a match.
	rr2 := httptest.NewRecorder()
	api.HandleProxyConfig(rr2, proxyReq("W/"+etag))
	if rr2.Code != http.StatusNotModified {
		t.Errorf("Expected 304 for weak validator match, got %d", rr2.Code)
	}
	if got := rr2.Header().Get("ETag"); got != etag {
		t.Errorf("304 response missing/mismatched ETag header: got %q, want %q", got, etag)
	}

	// A multi-value header must be matched entry-by-entry, not as one opaque string.
	rr3 := httptest.NewRecorder()
	api.HandleProxyConfig(rr3, proxyReq(`"some-other-etag", `+etag))
	if rr3.Code != http.StatusNotModified {
		t.Errorf("Expected 304 for a match within a multi-value If-None-Match, got %d", rr3.Code)
	}

	// A stale etag alone must still fall through to a fresh 200, not a false 304.
	rr4 := httptest.NewRecorder()
	api.HandleProxyConfig(rr4, proxyReq(`"config-99999"`))
	if rr4.Code != http.StatusOK {
		t.Errorf("Expected 200 for a non-matching etag, got %d", rr4.Code)
	}
}

/*
 * TestAPI_ProxyServiceRejectsEmptyToken guards isProxyService directly against an
 * unset proxyToken ("") being treated as a valid credential: subtle.ConstantTimeCompare
 * ("", "") returns 1, so without an explicit empty-string check, a misconfigured
 * (unset) API_PROXY_TOKEN would let an empty Bearer token through to the full
 * decrypted config. This is asserted on the API package itself rather than relying
 * on cmd/api/main.go's startup log.Fatal to make the empty case unreachable.
 */
func TestAPI_ProxyServiceRejectsEmptyToken(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	dummyKey := make([]byte, 32)
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", dummyKey, "")
	defer stop()

	req, _ := http.NewRequest("GET", "/api/proxy/config", nil)
	req.Header.Set("Authorization", "Bearer ")

	if api.isProxyService(req) {
		t.Fatalf("isProxyService must return false when proxyToken is unset, even against an empty Bearer token")
	}

	rr := httptest.NewRecorder()
	api.HandleProxyConfig(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for empty proxyToken with empty Bearer token, got %d", rr.Code)
	}
}

/*
 * TestAPI_HandleEventsRequiresServiceToken guards POST /api/events, the audit
 * write path: no header, a wrong token and a session token must all be refused,
 * and only the service token is accepted. The wrong-token case is the one a
 * broken comparison would let through.
 */
func TestAPI_HandleEventsRequiresServiceToken(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, err := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "valid-service-token")
	if err != nil {
		t.Fatalf("InitAuth: %v", err)
	}
	defer stop()
	createTestSession(t, testStore, "admin", "session-token")

	post := func(authorization string) int {
		req, _ := http.NewRequest("POST", "/api/events", bytes.NewBufferString("[]"))
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rr := httptest.NewRecorder()
		api.HandleEvents(rr, req)
		return rr.Code
	}

	for name, header := range map[string]string{
		"no header":     "",
		"wrong token":   "Bearer WRONG-TOKEN",
		"empty bearer":  "Bearer ",
		"not bearer":    "Basic valid-service-token",
		"session token": "Bearer session-token",
	} {
		if code := post(header); code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, code)
		}
	}
	if code := post("Bearer valid-service-token"); code != http.StatusAccepted {
		t.Errorf("service token: got %d, want 202", code)
	}
}

func TestAPI_ConnectionsOutcomeValidation(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()

	createTestSession(t, testStore, "admin", "test-token")

	// ?outcome=banana should fail with 400
	reqBad, _ := http.NewRequest("GET", "/api/connections?outcome=banana", nil)
	reqBad.AddCookie(&http.Cookie{Name: "session", Value: "test-token"})
	reqBad.Header.Set("X-Requested-With", "XMLHttpRequest")
	rrBad := httptest.NewRecorder()
	api.HandleConnections(rrBad, reqBad)
	if rrBad.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for ?outcome=banana, got %d", rrBad.Code)
	}

	// ?outcome=ok should succeed with 200
	reqGood, _ := http.NewRequest("GET", "/api/connections?outcome=ok", nil)
	reqGood.AddCookie(&http.Cookie{Name: "session", Value: "test-token"})
	reqGood.Header.Set("X-Requested-With", "XMLHttpRequest")
	rrGood := httptest.NewRecorder()
	api.HandleConnections(rrGood, reqGood)
	if rrGood.Code != http.StatusOK {
		t.Errorf("Expected 200 for valid outcome, got %d", rrGood.Code)
	}
}

/*
 * TestAPI_StatementsHideHandshake covers the query parameter that moves
 * handshake suppression into the SQL query.
 *
 * The behaviour that matters is the reported total: the viewer prints it, so
 * if it counted rows the filter then removed, the page would read "showing 2
 * of 62" while holding 2. It also pins the default — absent parameter means
 * everything is served, because an audit API that silently withholds records
 * is a worse default than a noisy one.
 */
func TestAPI_StatementsHideHandshake(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()

	createTestSession(t, testStore, "admin", "test-token-123")

	events := []audit.Event{{ConnID: "c-1", Kind: "connect", User: "alice", At: "2026-09-24T08:00:00.000000Z"}}
	for i := 0; i < 5; i++ {
		events = append(events, audit.Event{
			ConnID: "c-1", Kind: "statement", StmtType: "SELECT",
			At:  time.Date(2026, 9, 24, 8, 1, i, 0, time.UTC).Format("2006-01-02T15:04:05.000000Z"),
			SQL: "SET NAMES utf8mb4",
		})
	}
	events = append(events, audit.Event{
		ConnID: "c-1", Kind: "statement", StmtType: "SELECT",
		At:  "2026-09-24T08:02:00.000000Z",
		SQL: "SELECT * FROM sales.customers",
	})
	if err := testStore.WriteBatch(events); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	get := func(url string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", url, nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: "test-token-123"})
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		rr := httptest.NewRecorder()
		api.HandleStatements(rr, req)
		var body map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		return rr.Code, body
	}

	code, body := get("/api/statements")
	if code != http.StatusOK {
		t.Fatalf("unfiltered: got %d", code)
	}
	if total := body["total"].(float64); total != 6 {
		t.Errorf("default total = %v, want 6 — absent parameter must serve everything", total)
	}

	for _, spelling := range []string{"1", "true", "yes", "on"} {
		code, body := get("/api/statements?hide_handshake=" + spelling)
		if code != http.StatusOK {
			t.Fatalf("hide_handshake=%s: got %d", spelling, code)
		}
		total := body["total"].(float64)
		stmts := body["statements"].([]any)
		if total != 1 {
			t.Errorf("hide_handshake=%s: total = %v, want 1 (the total is what the viewer prints)", spelling, total)
		}
		if len(stmts) != 1 {
			t.Fatalf("hide_handshake=%s: returned %d statements, want 1", spelling, len(stmts))
		}
		if sql := stmts[0].(map[string]any)["sql"]; sql != "SELECT * FROM sales.customers" {
			t.Errorf("hide_handshake=%s: surviving statement = %v", spelling, sql)
		}
	}

	if _, body := get("/api/statements?hide_handshake=0"); body["total"].(float64) != 6 {
		t.Errorf("hide_handshake=0 should serve everything, got total %v", body["total"])
	}
}

/*
 * TestAPI_ProxyStatus covers the endpoint the proxy reports its applied
 * revision to. The 202 case asserts the store actually changed, not just the
 * status code: a handler that accepts and discards would pass otherwise.
 */
func TestAPI_ProxyStatus(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()

	// A session token is not a service token: the back-office must not be
	// able to claim what the proxy is running.
	createTestSession(t, testStore, "admin", "session-token")

	post := func(token, body string) *httptest.ResponseRecorder {
		req, _ := http.NewRequest("POST", "/api/proxy/status", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		api.HandleProxyStatus(rr, req)
		return rr
	}

	if rr := post("", `{"applied_revision":1}`); rr.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rr.Code)
	}
	if rr := post("session-token", `{"applied_revision":1}`); rr.Code != http.StatusUnauthorized {
		t.Errorf("session token: got %d, want 401", rr.Code)
	}

	req, _ := http.NewRequest("GET", "/api/proxy/status", nil)
	req.Header.Set("Authorization", "Bearer proxy-token")
	rrGet := httptest.NewRecorder()
	api.HandleProxyStatus(rrGet, req)
	if rrGet.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d, want 405", rrGet.Code)
	}

	for name, body := range map[string]string{
		"missing field": `{}`,
		"negative":      `{"applied_revision":-1}`,
		"not json":      `not json`,
	} {
		if rr := post("proxy-token", body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, rr.Code)
		}
	}

	if rr := post("proxy-token", `{"applied_revision":7}`); rr.Code != http.StatusAccepted {
		t.Fatalf("valid report: got %d, want 202", rr.Code)
	}
	rev, at, err := testStore.GetAppliedRevision()
	if err != nil {
		t.Fatalf("GetAppliedRevision: %v", err)
	}
	if rev != 7 {
		t.Errorf("applied_revision = %d after a 202, want 7", rev)
	}
	if at == "" {
		t.Error("applied_at is empty after a 202")
	}
}

/*
 * TestAPI_ServersReturnsRevisions covers the fields the back-office compares
 * to decide whether a saved change is live.
 */
func TestAPI_ServersReturnsRevisions(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()

	createTestSession(t, testStore, "admin", "session-token")

	if err := testStore.AddServer(audit.Server{
		Name: "db-a", ListenPort: 3307, UpstreamHost: "db-a", UpstreamPort: 3306, UpstreamUser: "root", Enabled: true,
	}, "encrypted"); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := testStore.SetAppliedRevision(1); err != nil {
		t.Fatalf("SetAppliedRevision: %v", err)
	}

	req, _ := http.NewRequest("GET", "/api/servers", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "session-token"})
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	rr := httptest.NewRecorder()
	api.HandleServers(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/servers: got %d, want 200", rr.Code)
	}

	var body struct {
		Revision        *int    `json:"revision"`
		AppliedRevision *int    `json:"applied_revision"`
		AppliedAt       *string `json:"applied_at"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Revision == nil || *body.Revision != 1 {
		t.Errorf("revision = %v, want 1", body.Revision)
	}
	if body.AppliedRevision == nil || *body.AppliedRevision != 1 {
		t.Errorf("applied_revision = %v, want 1", body.AppliedRevision)
	}
	if body.AppliedAt == nil || *body.AppliedAt == "" {
		t.Errorf("applied_at = %v, want a timestamp", body.AppliedAt)
	}
}

/*
 * TestAPI_LoginRateLimiting_PerIPAndUser tests that the lockout affects
 * the specific username/IP combination and correctly accepts a successful
 * login below the threshold.
 */
func TestAPI_LoginRateLimiting_PerIPAndUser(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()

	makeReq := func(user, pass, ip string) int {
		req, _ := http.NewRequest("POST", "/api/login", bytes.NewBufferString(fmt.Sprintf(`{"username":"%s", "password":"%s"}`, user, pass)))
		req.Header.Set("X-Forwarded-For", ip)
		rr := httptest.NewRecorder()
		api.HandleLogin(rr, req)
		return rr.Code
	}

	// 5 failures lock out 'admin' from '1.1.1.1'
	for i := 0; i < 5; i++ {
		makeReq("admin", "wrong", "1.1.1.1")
	}
	if code := makeReq("admin", "wrong", "1.1.1.1"); code != http.StatusTooManyRequests {
		t.Errorf("Expected 429 after 5 failures for IP+User, got %d", code)
	}

	// But 'admin' is perfectly fine from '2.2.2.2'
	if code := makeReq("admin", "admin123", "2.2.2.2"); code != http.StatusOK {
		t.Errorf("Expected 200 for user on fresh IP, got %d", code)
	}

	// And another user 'other' is perfectly fine from '1.1.1.1'
	if code := makeReq("other", "wrong", "1.1.1.1"); code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for different user on same IP, got %d", code)
	}
}

func TestAPI_LoginRateLimitingConcurrent(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", "/api/login", bytes.NewBufferString(`{"username":"admin", "password":"wrong-password"}`))
			req.Header.Set("X-Forwarded-For", "192.168.1.100")
			api.HandleLogin(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()
}

func TestAPI_LoginDisabledAccount(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()

	if err := testStore.CreateAdmin("other", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW"); err != nil {
		t.Fatal(err)
	}
	if err := testStore.DisableAdmin("admin", "other"); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("POST", "/api/login", bytes.NewBufferString(`{"username":"other", "password":"password"}`))
	rr := httptest.NewRecorder()
	api.HandleLogin(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for disabled account, got %d", rr.Code)
	}
}

func TestAPI_GetClientIP_XFF(t *testing.T) {
	api := &API{}
	req1, _ := http.NewRequest("POST", "/", nil)
	req1.RemoteAddr = "127.0.0.1:12345"
	req1.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")
	if ip := api.getClientIP(req1); ip != "203.0.113.5" {
		t.Errorf("Expected trusted XFF IP 203.0.113.5, got %s", ip)
	}
}

/*
 * TestAPI_ServerAttribution verifies that successful server modifications create
 * attribution log rows.
 */
func TestAPI_ServerAttribution(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	dummyKey := make([]byte, 32)
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", dummyKey, "proxy-token")
	defer stop()

	createTestSession(t, testStore, "admin", "admin-token")

	authReq := func(method, path, bodyStr string) *http.Request {
		var body io.Reader
		if bodyStr != "" {
			body = bytes.NewBufferString(bodyStr)
		}
		req, _ := http.NewRequest(method, path, body)
		req.AddCookie(&http.Cookie{Name: "session", Value: "admin-token"})
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		return req
	}

	// 1. CREATE_SERVER
	req1 := authReq("POST", "/api/servers", `{"name": "srv1", "listen_port": 3306, "upstream_host": "db", "upstream_port": 3306, "upstream_user": "u", "upstream_pass": "secret", "enabled": true}`)
	api.HandleServers(httptest.NewRecorder(), req1)

	// 2. UPDATE_SERVER
	req2 := authReq("PUT", "/api/servers", `{"id": 1, "name": "srv1-renamed", "listen_port": 3306, "upstream_host": "db", "upstream_port": 3306, "upstream_user": "u", "upstream_pass": "secret", "enabled": true}`)
	api.HandleServers(httptest.NewRecorder(), req2)

	// 3. DELETE_SERVER
	req3 := authReq("DELETE", "/api/servers?id=1", "")
	api.HandleServers(httptest.NewRecorder(), req3)

	actions, _, err := testStore.ListActions(50, 0)
	if err != nil {
		t.Fatalf("ListActions failed: %v", err)
	}

	// Newest first.
	expected := []struct{ action, target string }{
		{"DELETE_SERVER", "srv1-renamed"},
		{"UPDATE_SERVER", "srv1-renamed"},
		{"CREATE_SERVER", "srv1"},
	}

	if len(actions) != len(expected) {
		t.Fatalf("Expected %d actions, got %d", len(expected), len(actions))
	}

	for i, a := range actions {
		if a.Username != "admin" {
			t.Errorf("Action %d: expected admin user, got %q", i, a.Username)
		}
		if a.Action != expected[i].action || a.Target != expected[i].target {
			t.Errorf("Action %d: expected %s on %q, got %s on %q", i, expected[i].action, expected[i].target, a.Action, a.Target)
		}
	}
}

func TestAPI_StatementKindValidation(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()
	createTestSession(t, testStore, "admin", "test-token-123")

	err := testStore.WriteBatch([]audit.Event{
		{ConnID: "c-1", Kind: "connect", User: "alice", Upstream: "db-a", At: "2026-10-12T12:00:00.000000Z"},
		{ConnID: "c-1", Kind: "statement", StmtType: "SELECT", SQL: "SELECT 1", At: "2026-10-12T12:00:01.000000Z"},
		{ConnID: "c-1", Kind: "statement", StmtType: "DROP", SQL: "DROP TABLE t", Err: "denied", At: "2026-10-12T12:00:02.000000Z"},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	get := func(query string) (int, float64) {
		req, _ := http.NewRequest("GET", "/api/statements?"+query, nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: "test-token-123"})
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		rr := httptest.NewRecorder()
		api.HandleStatements(rr, req)
		var body map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		total, _ := body["total"].(float64)
		return rr.Code, total
	}

	for query, want := range map[string]float64{
		"kind=read":                 1,
		"kind=schema":               1,
		"kind=write":                0,
		"has_error=1":               1,
		"upstream=db-a":             2,
		"upstream=nope":             0,
		"kind=schema&has_error=yes": 1,
	} {
		code, total := get(query)
		if code != http.StatusOK || total != want {
			t.Errorf("%s: got %d total %v, want 200 total %v", query, code, total, want)
		}
	}

	if code, _ := get("kind=bogus"); code != http.StatusBadRequest {
		t.Errorf("unknown kind: got %d, want 400", code)
	}
}

func TestAPI_LoginRecordsLastLogin(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, err := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	if err != nil {
		t.Fatalf("InitAuth: %v", err)
	}
	defer stop()

	login := func(password string) int {
		body, _ := json.Marshal(audit.LoginRequest{Username: "admin", Password: password})
		req, _ := http.NewRequest("POST", "/api/login", bytes.NewBuffer(body))
		rr := httptest.NewRecorder()
		api.HandleLogin(rr, req)
		return rr.Code
	}
	lastLogin := func() *string {
		admins, err := testStore.ListAdmins()
		if err != nil || len(admins) != 1 {
			t.Fatalf("ListAdmins = %+v, %v", admins, err)
		}
		return admins[0].LastLoginAt
	}

	if lastLogin() != nil {
		t.Fatal("last_login_at should start null")
	}
	if code := login("wrong-password"); code != http.StatusUnauthorized {
		t.Fatalf("bad password: got %d, want 401", code)
	}
	if lastLogin() != nil {
		t.Error("a failed login must not set last_login_at")
	}
	if code := login("admin123"); code != http.StatusOK {
		t.Fatalf("good password: got %d, want 200", code)
	}
	if lastLogin() == nil {
		t.Error("a successful login must set last_login_at")
	}
}

func TestAPI_ListActionsPagination(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()
	createTestSession(t, testStore, "admin", "test-token-123")

	for i := 1; i <= 7; i++ {
		if err := testStore.LogAction("admin", "TEST_ACTION", fmt.Sprintf("t%d", i)); err != nil {
			t.Fatalf("LogAction: %v", err)
		}
	}

	get := func(query string) (int, struct {
		Total   int `json:"total"`
		Actions []struct {
			Target string `json:"target"`
		} `json:"actions"`
	}) {
		req, _ := http.NewRequest("GET", "/api/actions"+query, nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: "test-token-123"})
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		rr := httptest.NewRecorder()
		api.HandleListActions(rr, req)
		var body struct {
			Total   int `json:"total"`
			Actions []struct {
				Target string `json:"target"`
			} `json:"actions"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		return rr.Code, body
	}

	code, body := get("?limit=3&offset=2")
	if code != http.StatusOK || body.Total != 7 || len(body.Actions) != 3 {
		t.Fatalf("got %d total %d rows %d, want 200, 7, 3", code, body.Total, len(body.Actions))
	}
	if body.Actions[0].Target != "t5" || body.Actions[2].Target != "t3" {
		t.Errorf("page = %v, want t5..t3 (newest first)", body.Actions)
	}

	if code, body = get("?offset=50"); code != http.StatusOK || body.Total != 7 || body.Actions == nil || len(body.Actions) != 0 {
		t.Errorf("past the end: code %d total %d actions %v, want 200, 7, []", code, body.Total, body.Actions)
	}
	for _, q := range []string{"?limit=-1", "?offset=-1", "?limit=abc"} {
		if code, _ := get(q); code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", q, code)
		}
	}
}

func TestAPI_SummaryEndpoint(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()
	createTestSession(t, testStore, "admin", "test-token-123")

	err := testStore.WriteBatch([]audit.Event{
		{ConnID: "c-1", Kind: "connect", User: "alice", Upstream: "db-a", AuthResult: "ok", At: "2026-10-12T10:00:00.000000Z"},
		{ConnID: "c-2", Kind: "auth_fail", User: "mallory", Upstream: "db-a", AuthResult: "fail", Err: "denied", At: "2026-10-12T10:01:00.000000Z"},
		{ConnID: "c-1", Kind: "statement", StmtType: "CREATE", SQL: "CREATE TABLE t (a int)", At: "2026-10-12T10:02:00.000000Z"},
		{ConnID: "c-1", Kind: "statement", StmtType: "SELECT", SQL: "SELECT * FROM missing", Err: "no such table", At: "2026-10-12T10:03:00.000000Z"},
		{ConnID: "c-1", Kind: "statement", StmtType: "SET", SQL: "SET NAMES utf8mb4", At: "2026-10-12T10:04:00.000000Z"},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	do := func(method, token string) *httptest.ResponseRecorder {
		req, _ := http.NewRequest(method, "/api/summary", nil)
		if token != "" {
			req.AddCookie(&http.Cookie{Name: "session", Value: token})
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
		}
		rr := httptest.NewRecorder()
		api.HandleSummary(rr, req)
		return rr
	}

	if rr := do("GET", ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", rr.Code)
	}
	if rr := do("POST", "test-token-123"); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d, want 405", rr.Code)
	}

	rr := do("GET", "test-token-123")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Sessions        int `json:"sessions"`
		OpenSessions    int `json:"open_sessions"`
		FailedLogins    int `json:"failed_logins"`
		StatementErrors int `json:"statement_errors"`
		SchemaChanges   int `json:"schema_changes"`
		ByUpstream      []struct {
			Upstream     string `json:"upstream"`
			OpenSessions int    `json:"open_sessions"`
			Statements   int    `json:"statements"`
		} `json:"by_upstream"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Sessions != 2 || got.OpenSessions != 1 || got.FailedLogins != 1 ||
		got.StatementErrors != 1 || got.SchemaChanges != 1 {
		t.Errorf("summary = %+v", got)
	}
	if len(got.ByUpstream) != 1 || got.ByUpstream[0].Upstream != "db-a" ||
		got.ByUpstream[0].OpenSessions != 1 || got.ByUpstream[0].Statements != 3 {
		t.Errorf("by_upstream = %+v", got.ByUpstream)
	}
}

func TestAPI_LogoutEndsTheSession(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()
	createTestSession(t, testStore, "admin", "live-token")

	servers := func() int {
		r, _ := http.NewRequest("GET", "/api/servers", nil)
		r.AddCookie(&http.Cookie{Name: "session", Value: "live-token"})
		w := httptest.NewRecorder()
		api.HandleServers(w, r)
		return w.Code
	}
	logout := func(csrf bool, method string) *httptest.ResponseRecorder {
		r, _ := http.NewRequest(method, "/api/logout", nil)
		r.AddCookie(&http.Cookie{Name: "session", Value: "live-token"})
		if csrf {
			r.Header.Set("X-Requested-With", "XMLHttpRequest")
		}
		w := httptest.NewRecorder()
		api.HandleLogout(w, r)
		return w
	}

	if servers() != http.StatusOK {
		t.Fatal("session should work before logout")
	}
	if w := logout(false, "POST"); w.Code != http.StatusForbidden {
		t.Errorf("logout without the CSRF header: got %d, want 403", w.Code)
	}
	if w := logout(true, "GET"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/logout: got %d, want 405", w.Code)
	}
	if servers() != http.StatusOK {
		t.Fatal("a refused logout must not end the session")
	}

	w := logout(true, "POST")
	if w.Code != http.StatusOK {
		t.Fatalf("logout: got %d, want 200", w.Code)
	}
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" && c.Value == "" && c.MaxAge < 0 && c.HttpOnly && c.Secure {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("logout must clear the session cookie, got %v", w.Result().Cookies())
	}
	// Server-side, not just in the browser: the old token no longer works.
	if code := servers(); code != http.StatusUnauthorized {
		t.Errorf("token after logout: got %d, want 401", code)
	}
	// Logging out again, with no live session, is harmless.
	if w := logout(true, "POST"); w.Code != http.StatusOK {
		t.Errorf("second logout: got %d, want 200", w.Code)
	}
}

func TestAPI_ExpiredSessionCookieIsRejected(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("old-token")))
	if err := testStore.CreateSession(hash, "admin", time.Now().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r, _ := http.NewRequest("GET", "/api/servers", nil)
	r.AddCookie(&http.Cookie{Name: "session", Value: "old-token"})
	w := httptest.NewRecorder()
	api.HandleServers(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expired session: got %d, want 401", w.Code)
	}
}

// Every route the browser reaches with the session cookie must refuse a
// state-changing request that lacks the CSRF header, and must still serve
// reads without it.
func TestAPI_CSRFHeaderRequiredOnSessionWrites(t *testing.T) {
	testStore, _ := store.InitDB(":memory:")
	api := &API{Store: testStore}
	stop, _ := api.InitAuth("admin", "$2a$10$p/JfpdaXDpAp5XhXeDax6OdUOClgnJGA8zH3X7Gxrf0dv5zvjb/UW", make([]byte, 32), "proxy-token")
	defer stop()
	createTestSession(t, testStore, "admin", "tok")

	call := func(h http.HandlerFunc, method, path, body string, csrf bool) int {
		r, _ := http.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "session", Value: "tok"})
		if csrf {
			r.Header.Set("X-Requested-With", "XMLHttpRequest")
		}
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}

	for _, c := range []struct {
		name         string
		h            http.HandlerFunc
		method, path string
	}{
		{"create server", api.HandleServers, "POST", "/api/servers"},
		{"update server", api.HandleServers, "PUT", "/api/servers"},
		{"delete server", api.HandleServers, "DELETE", "/api/servers?id=1"},
		{"create admin", api.HandleCreateAdmin, "POST", "/api/admins"},
		{"disable admin", api.HandleDisableAdmin, "PUT", "/api/admins/bob/disable"},
		{"change password", api.HandleChangePassword, "PUT", "/api/admins/bob/password"},
	} {
		if code := call(c.h, c.method, c.path, "{}", false); code != http.StatusForbidden {
			t.Errorf("%s without X-Requested-With: got %d, want 403", c.name, code)
		}
		if code := call(c.h, c.method, c.path, "{}", true); code == http.StatusForbidden {
			t.Errorf("%s with X-Requested-With was still refused as CSRF", c.name)
		}
	}

	for name, h := range map[string]http.HandlerFunc{
		"/api/servers": api.HandleServers, "/api/admins": api.HandleListAdmins, "/api/actions": api.HandleListActions,
	} {
		if code := call(h, "GET", name, "", false); code != http.StatusOK {
			t.Errorf("GET %s without the header: got %d, want 200", name, code)
		}
	}
}
