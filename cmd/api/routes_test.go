package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/tevans2/mysql-audit-proxy/internal/api"
	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/store"
)

const bootstrapPass = "adminpass1"

/*
 * fixture is a real store + API behind the real routes() mux, so every test
 * here goes through the same HTTP routing the server uses.
 */
type fixture struct {
	t   *testing.T
	st  *store.Store
	mux *http.ServeMux
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.InitDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	hash, err := bcrypt.GenerateFromPassword([]byte(bootstrapPass), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &api.API{Store: st}

	// Catch the new stop function and defer it to the test cleanup
	stop, err := a.InitAuth("admin", string(hash), make([]byte, 32), "proxy-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	return &fixture{t: t, st: st, mux: routes(a)}
}

func (f *fixture) do(method, path, body, token string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "session", Value: token})
	}
	r.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *fixture) login(user, pass string) (string, int) {
	f.t.Helper()
	body, _ := json.Marshal(audit.LoginRequest{Username: user, Password: pass})
	w := f.do("POST", "/api/login", string(body), "")
	if w.Code != http.StatusOK {
		return "", w.Code
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" {
			return c.Value, w.Code
		}
	}
	f.t.Fatal("login succeeded but set no session cookie")
	return "", w.Code
}

func (f *fixture) mustLogin(user, pass string) string {
	f.t.Helper()
	tok, code := f.login(user, pass)
	if code != http.StatusOK {
		f.t.Fatalf("login %s: got %d", user, code)
	}
	return tok
}

func adminJSON(user, pass string) string {
	b, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	return string(b)
}

func (f *fixture) addAdmin(token, user, pass string) {
	f.t.Helper()
	if w := f.do("POST", "/api/admins", adminJSON(user, pass), token); w.Code != http.StatusCreated {
		f.t.Fatalf("create %s: %d %s", user, w.Code, w.Body.String())
	}
}

func (f *fixture) sessionExists(token string) bool {
	_, _, err := f.st.GetSession(fmt.Sprintf("%x", sha256.Sum256([]byte(token))))
	return err == nil
}

// tokenWorks reports whether token is accepted by an authenticated endpoint.
func (f *fixture) tokenWorks(token string) bool {
	return f.do("GET", "/api/servers", "", token).Code == http.StatusOK
}

func (f *fixture) actions(action string) []store.AdminAction {
	f.t.Helper()
	all, _, err := f.st.ListActions(1000, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	// ListActions is newest first; the assertions here read oldest first.
	var out []store.AdminAction
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Action == action {
			out = append(out, all[i])
		}
	}
	return out
}

func TestAdminRoutes_AreRegisteredAndRequireAuth(t *testing.T) {
	f := newFixture(t)
	// An unregistered route would answer 404; a registered one rejects the
	// missing token with 401.
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/admins"},
		{"PUT", "/api/admins/bob/disable"},
		{"PUT", "/api/admins/bob/password"},
		{"GET", "/api/actions"},
		{"GET", "/api/summary"},
	} {
		if w := f.do(c.method, c.path, "{}", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: got %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestAdminRoutes_UnknownSubpathIs404(t *testing.T) {
	f := newFixture(t)
	tok := f.mustLogin("admin", bootstrapPass)
	for _, p := range []string{"/api/admins/bob", "/api/admins/bob/frobnicate"} {
		if w := f.do("PUT", p, "{}", tok); w.Code != http.StatusNotFound {
			t.Errorf("PUT %s: got %d, want 404", p, w.Code)
		}
	}
}

func TestCreateAdmin(t *testing.T) {
	f := newFixture(t)
	tok := f.mustLogin("admin", bootstrapPass)

	f.addAdmin(tok, "bob", "bobpass12")
	if _, code := f.login("bob", "bobpass12"); code != http.StatusOK {
		t.Fatalf("new admin cannot log in: %d", code)
	}

	if w := f.do("POST", "/api/admins", adminJSON("bob", "bobpass12"), tok); w.Code != http.StatusConflict {
		t.Errorf("duplicate: got %d, want 409", w.Code)
	}

	// 72 bytes is bcrypt's limit and must be accepted.
	f.addAdmin(tok, "max72", strings.Repeat("a", 72))

	bad := map[string]string{
		"empty username":      adminJSON("", "goodpass12"),
		"blank username":      adminJSON("   ", "goodpass12"),
		"slash in username":   adminJSON("a/b", "goodpass12"),
		"overlong username":   adminJSON(strings.Repeat("u", 65), "goodpass12"),
		"empty password":      adminJSON("carol", ""),
		"short password":      adminJSON("carol", "short"),
		"73-byte password":    adminJSON("carol", strings.Repeat("a", 73)),
		"malformed JSON body": "{not json",
	}
	for name, body := range bad {
		if w := f.do("POST", "/api/admins", body, tok); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, w.Code)
		}
	}

	// Only the two successful creates are attributed; rejected ones are not.
	got := f.actions("CREATE_ADMIN")
	if len(got) != 2 || got[0].Username != "admin" || got[0].Action != "CREATE_ADMIN" || got[0].Target != "bob" {
		t.Errorf("attribution rows: %+v", got)
	}

	if got[0].Ts == "" {
		t.Error("attribution row has no timestamp")
	}
}

func TestDisableAdmin(t *testing.T) {
	f := newFixture(t)
	adminTok := f.mustLogin("admin", bootstrapPass)
	f.addAdmin(adminTok, "bob", "bobpass12")
	bobTok := f.mustLogin("bob", "bobpass12")
	if !f.tokenWorks(bobTok) {
		t.Fatal("bob's session should work before disable")
	}

	if w := f.do("PUT", "/api/admins/admin/disable", "", adminTok); w.Code != http.StatusForbidden {
		t.Errorf("self-disable: got %d, want 403", w.Code)
	}
	if !f.tokenWorks(adminTok) {
		t.Error("self-disable attempt must not log the admin out")
	}
	if w := f.do("PUT", "/api/admins/ghost/disable", "", adminTok); w.Code != http.StatusNotFound {
		t.Errorf("unknown admin: got %d, want 404", w.Code)
	}

	if w := f.do("PUT", "/api/admins/bob/disable", "", adminTok); w.Code != http.StatusOK {
		t.Fatalf("disable: got %d: %s", w.Code, w.Body.String())
	}
	if f.sessionExists(bobTok) {
		t.Error("disabled admin's session row should be deleted")
	}
	if f.tokenWorks(bobTok) {
		t.Error("disabled admin's token must stop working")
	}
	if _, code := f.login("bob", "bobpass12"); code != http.StatusUnauthorized {
		t.Errorf("disabled admin login: got %d, want 401", code)
	}
	if !f.tokenWorks(adminTok) {
		t.Error("the acting admin's session must be unaffected")
	}

	// Disabling again is a harmless no-op.
	if w := f.do("PUT", "/api/admins/bob/disable", "", adminTok); w.Code != http.StatusOK {
		t.Errorf("repeat disable: got %d, want 200", w.Code)
	}

	got := f.actions("DISABLE_ADMIN")
	if len(got) < 1 || got[0].Username != "admin" || got[0].Action != "DISABLE_ADMIN" || got[0].Target != "bob" {
		t.Errorf("attribution rows: %+v", got)
	}
	if n := len(f.actions("DISABLE_ADMIN")); n != 2 {
		t.Errorf("want 2 DISABLE_ADMIN rows (rejected requests write none), got %d", n)
	}
}

func TestDisableAdmin_LastActiveAdminRefused(t *testing.T) {
	f := newFixture(t)
	err := f.st.DisableAdmin("someone", "admin")
	if !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("got %v, want ErrLastAdmin", err)
	}
	if _, disabled, _ := f.st.GetAdmin("admin"); disabled != 0 {
		t.Error("the last admin must stay enabled")
	}
	if n := len(f.actions("DISABLE_ADMIN")); n != 0 {
		t.Errorf("refused disable must not be attributed, got %d rows", n)
	}
}

// Two admins disabling each other at once must never leave zero active admins.
func TestDisableAdmin_MutualDisableLeavesOneActive(t *testing.T) {
	f := newFixture(t)
	if err := f.st.CreateAdmin("bob", "x"); err != nil {
		t.Fatal(err)
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = f.st.DisableAdmin("bob", "admin") }()
	go func() { defer wg.Done(); errs[1] = f.st.DisableAdmin("admin", "bob") }()
	wg.Wait()

	ok, last := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, store.ErrLastAdmin):
			last++
		default:
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if ok != 1 || last != 1 {
		t.Fatalf("want exactly one success and one ErrLastAdmin, got ok=%d last=%d", ok, last)
	}
	active := 0
	for _, u := range []string{"admin", "bob"} {
		if _, disabled, _ := f.st.GetAdmin(u); disabled == 0 {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("want exactly 1 active admin, got %d", active)
	}
}

func TestChangePassword(t *testing.T) {
	f := newFixture(t)
	adminTok := f.mustLogin("admin", bootstrapPass)
	f.addAdmin(adminTok, "bob", "bobpass12")
	bobTok := f.mustLogin("bob", "bobpass12")

	for name, body := range map[string]string{
		"empty":     `{"password":""}`,
		"short":     `{"password":"short"}`,
		"73 bytes":  `{"password":"` + strings.Repeat("a", 73) + `"}`,
		"bad JSON":  "{nope",
		"no fields": `{}`,
	} {
		if w := f.do("PUT", "/api/admins/bob/password", body, adminTok); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, w.Code)
		}
	}
	if !f.tokenWorks(bobTok) {
		t.Error("rejected change must not invalidate sessions")
	}
	if w := f.do("PUT", "/api/admins/ghost/password", `{"password":"newpass123"}`, adminTok); w.Code != http.StatusNotFound {
		t.Errorf("unknown admin: got %d, want 404", w.Code)
	}

	if w := f.do("PUT", "/api/admins/bob/password", `{"password":"newbobpass1"}`, adminTok); w.Code != http.StatusOK {
		t.Fatalf("change: got %d: %s", w.Code, w.Body.String())
	}
	if _, code := f.login("bob", "bobpass12"); code != http.StatusUnauthorized {
		t.Errorf("old password: got %d, want 401", code)
	}
	if _, code := f.login("bob", "newbobpass1"); code != http.StatusOK {
		t.Errorf("new password: got %d, want 200", code)
	}
	if f.sessionExists(bobTok) || f.tokenWorks(bobTok) {
		t.Error("existing sessions must be invalidated by a password change")
	}
	if !f.tokenWorks(adminTok) {
		t.Error("the acting admin's session must be unaffected")
	}

	got := f.actions("CHANGE_PASSWORD")
	if len(got) != 1 || got[0].Username != "admin" || got[0].Action != "CHANGE_PASSWORD" || got[0].Target != "bob" {
		t.Errorf("attribution rows: %+v", got)
	}
}

func (f *fixture) loginFrom(remote, xff, user, pass string) int {
	f.t.Helper()
	body, _ := json.Marshal(audit.LoginRequest{Username: user, Password: pass})
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(string(body)))
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w.Code
}

/*
 * A client that is not a trusted proxy cannot dodge the limiter by rotating
 * X-Forwarded-For; the lockout is per IP+username, so another IP is unaffected.
 */
func TestLoginLockout_UntrustedPeerCannotSpoofXFF(t *testing.T) {
	f := newFixture(t)
	for i := 1; i <= 10; i++ {
		xff := fmt.Sprintf("203.0.113.%d", i)
		user := fmt.Sprintf("admin%d", i%4) // Rotate users to avoid the 5-failure user limit
		if code := f.loginFrom("198.51.100.7:4321", xff, user, "wrong-password"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i, code)
		}
	}
	if code := f.loginFrom("198.51.100.7:4321", "203.0.113.99", "admin1", "wrong-password"); code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt with a fresh XFF: got %d, want 429", code)
	}
	if code := f.loginFrom("198.51.100.8:4321", "", "admin1", "wrong-password"); code != http.StatusUnauthorized {
		t.Fatalf("different IP must not be locked out: got %d, want 401", code)
	}
}

/*
 * Behind a trusted proxy, XFF identifies the client: one client's lockout
 * must not lock out another client using the same proxy.
 */
func TestLoginLockout_TrustedProxyUsesXFF(t *testing.T) {
	f := newFixture(t)
	const proxy = "127.0.0.1:5555"
	for i := 1; i <= 5; i++ {
		if code := f.loginFrom(proxy, "203.0.113.5", "admin", "wrong-password"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401", i, code)
		}
	}
	if code := f.loginFrom(proxy, "203.0.113.5", "admin", "wrong-password"); code != http.StatusTooManyRequests {
		t.Fatalf("client A 6th attempt: got %d, want 429", code)
	}
	if code := f.loginFrom(proxy, "203.0.113.6", "bob", "wrong-password"); code != http.StatusUnauthorized {
		t.Fatalf("client B behind the same proxy: got %d, want 401", code)
	}
}

/*
 * TestAPI_AdminRouting verifies that the newly parsed routes hit the correct handlers.
 */
func TestAPI_AdminRouting(t *testing.T) {
	apiInstance := &api.API{}
	mux := routes(apiInstance)

	// Missing token returns 401 from the handler, meaning the route actually connected
	req1, _ := http.NewRequest("PUT", "/api/admins/someuser/disable", nil)
	rr1 := httptest.NewRecorder()
	mux.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 (hit HandleDisableAdmin), got %d", rr1.Code)
	}

	req2, _ := http.NewRequest("PUT", "/api/admins/someuser/password", nil)
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 (hit HandleChangePassword), got %d", rr2.Code)
	}

	req3, _ := http.NewRequest("PUT", "/api/admins/someuser/invalid", nil)
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusNotFound {
		t.Errorf("Expected 404 for invalid verb, got %d", rr3.Code)
	}
}

func TestLogoutRouteIsRegistered(t *testing.T) {
	f := newFixture(t)
	tok := f.mustLogin("admin", bootstrapPass)
	if !f.tokenWorks(tok) {
		t.Fatal("fresh session should work")
	}
	if w := f.do("POST", "/api/logout", "", tok); w.Code != http.StatusOK {
		t.Fatalf("POST /api/logout: got %d, want 200 (404 means the route is missing)", w.Code)
	}
	if f.tokenWorks(tok) {
		t.Error("the session survived logout")
	}
}
