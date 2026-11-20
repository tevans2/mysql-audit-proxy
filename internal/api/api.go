package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tevans2/mysql-audit-proxy/internal/audit"
	"github.com/tevans2/mysql-audit-proxy/internal/store"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	maxLoginFailuresPerUserIP = 5
	maxLoginFailuresPerIP     = 10
	loginWindow               = 15 * time.Minute
	lockoutDuration           = 15 * time.Minute
)

// RateLimit tracks failed logins for IP and username.
type RateLimit struct {
	Failures    int
	LastAttempt time.Time
	LockoutEnd  time.Time
}

type AdminRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// API encapsulates the dependencies, shared state, and HTTP handlers for the back-office service.
type API struct {
	Store *store.Store

	adminUser     string
	adminPassHash string
	configKey     []byte
	proxyToken    string

	limiterMu  sync.Mutex
	ipLimits   map[string]RateLimit
	userLimits map[string]RateLimit

	dummyHash []byte
}

/*
 * InitAuth configures the API credentials from the environment, setting up the
 * administrator user, encryption key, and proxy service token. The admin user
 * and hash only bootstrap the FIRST admin (see Store.SeedAdminIfEmpty); it
 * returns an error if that bootstrap fails so the caller can refuse to start.
 * Returns a func() to cleanly stop the background purge goroutine.
 */
func (a *API) InitAuth(user, passHash string, key []byte, proxyToken string) (func(), error) {
	a.adminUser = user
	a.adminPassHash = passHash
	a.configKey = key
	a.proxyToken = proxyToken
	a.ipLimits = make(map[string]RateLimit)
	a.userLimits = make(map[string]RateLimit)

	if !validUsername(user) {
		return nil, fmt.Errorf("invalid default admin username: %q", user)
	}

	dummy, err := bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("generate dummy hash: %w", err)
	}
	a.dummyHash = dummy

	if a.Store != nil {
		if err := a.Store.SeedAdminIfEmpty(user, passHash); err != nil {
			return nil, fmt.Errorf("seed initial admin: %w", err)
		}
	}

	// Start the background sweeper, returning a func to stop it cleanly
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.purgeExpiredRateLimits()
				if a.Store != nil {
					if err := a.Store.DeleteExpiredSessions(time.Now().UTC().Format(time.RFC3339)); err != nil {
						log.Printf("Failed to delete expired sessions: %v", err)
					}
				}
			case <-done:
				return
			}
		}
	}()

	return func() { close(done) }, nil
}

/*
 * encryptAES encrypts the given plaintext using AES-256-GCM and a secure random nonce.
 * It returns the hex-encoded ciphertext.
 */
func encryptAES(plaintext string, key []byte) (string, error) {
	c, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(c)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(sealed), nil
}

/*
 * decryptAES decrypts a hex-encoded ciphertext using AES-256-GCM.
 * It separates the nonce from the sealed payload and returns the original plaintext.
 */
func decryptAES(ciphertextHex string, key []byte) (string, error) {
	ciphertext, err := hex.DecodeString(ciphertextHex)
	if err != nil {
		return "", err
	}
	c, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(c)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	return string(plaintext), err
}

var defaultTrustedProxies = mustParseCIDRs(
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7",
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

func isTrustedProxy(ip net.IP) bool {
	for _, n := range defaultTrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

/*
 * getClientIP returns the client IP. X-Forwarded-For is only honoured when
 * the direct peer is a trusted proxy; the chain is walked right to left and
 * the first untrusted hop is the client.
 */
func (a *API) getClientIP(r *http.Request) string {
	remote := "127.0.0.1"
	if r.RemoteAddr != "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			remote = host
		} else {
			remote = r.RemoteAddr
		}
	}

	peer := net.ParseIP(remote)
	if peer == nil || !isTrustedProxy(peer) {
		return remote
	}

	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remote
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			return remote
		}
		if !isTrustedProxy(ip) {
			return ip.String()
		}
	}
	return remote
}

/*
 * purgeExpiredRateLimits periodically clears out expired rate limiting entries
 * to prevent the in-memory limits map from growing indefinitely over time.
 */
func (a *API) purgeExpiredRateLimits() {
	a.limiterMu.Lock()
	defer a.limiterMu.Unlock()
	now := time.Now()
	for k, v := range a.ipLimits {
		if now.After(v.LockoutEnd) && now.Sub(v.LastAttempt) > loginWindow {
			delete(a.ipLimits, k)
		}
	}
	for k, v := range a.userLimits {
		if now.After(v.LockoutEnd) && now.Sub(v.LastAttempt) > loginWindow {
			delete(a.userLimits, k)
		}
	}
}

/*
 * isValidPassword checks if the provided password meets the required bounds
 * of at least 8 characters and no more than 72 bytes (bcrypt's maximum limit).
 */
func isValidPassword(p string) bool {
	return len(p) >= 8 && len(p) <= 72
}

/*
 * validUsername rejects empty names, overlong names and names containing "/",
 * which would be ambiguous in /api/admins/{username}/... paths.
 */
func validUsername(u string) bool {
	return u != "" && len(u) <= 64 && !strings.Contains(u, "/")
}

// sessionCookie is the name of the cookie that carries the session token.
const sessionCookie = "session"

// sessionTTL is how long a session, and its cookie, lasts.
const sessionTTL = 24 * time.Hour

/*
 * checkCSRF reports whether a request may change state. The session lives in a
 * cookie the browser attaches on its own, so a mutating request must also carry
 * a header a cross-site form or image cannot set. SameSite=Strict on the cookie
 * is the first defence and this is the second.
 *
 * It applies only to routes authenticated by the session cookie. The service
 * routes the proxy calls authenticate with a Bearer token instead, which a
 * browser never sends by itself, so they need no such header.
 */
func checkCSRF(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}

// setSessionCookie issues (or, with an empty value and a past expiry, clears)
// the session cookie. HttpOnly keeps it out of reach of scripts; Secure and
// SameSite=Strict keep it off plain HTTP and off cross-site requests.
func setSessionCookie(w http.ResponseWriter, value string, expires time.Time) {
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
	if value == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

/*
 * HandleLogin processes POST /api/login requests.
 * It verifies the provided plaintext credentials against the stored bcrypt hash
 * and, on success, stores a session row and sets the session cookie. The token
 * is never returned in the response body, so page scripts cannot read it.
 */
func (a *API) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clientIP := a.getClientIP(r)

	var req audit.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	now := time.Now()
	userIPKey := clientIP + "|" + req.Username // Key by username+IP to prevent lockout DoS

	a.limiterMu.Lock()
	ipLimit := a.ipLimits[clientIP]
	userLimit := a.userLimits[userIPKey]

	if now.Before(ipLimit.LockoutEnd) || now.Before(userLimit.LockoutEnd) {
		a.limiterMu.Unlock()
		jsonError(w, "Too many failed attempts. Try again later.", http.StatusTooManyRequests)
		return
	}

	if now.Sub(ipLimit.LastAttempt) > loginWindow {
		ipLimit.Failures = 0
	}
	if now.Sub(userLimit.LastAttempt) > loginWindow {
		userLimit.Failures = 0
	}

	ipLimit.Failures++
	userLimit.Failures++
	ipLimit.LastAttempt = now
	userLimit.LastAttempt = now

	if ipLimit.Failures >= maxLoginFailuresPerIP {
		ipLimit.LockoutEnd = now.Add(lockoutDuration)
	}
	if userLimit.Failures >= maxLoginFailuresPerUserIP {
		userLimit.LockoutEnd = now.Add(lockoutDuration)
	}

	a.ipLimits[clientIP] = ipLimit
	a.userLimits[userIPKey] = userLimit
	a.limiterMu.Unlock()

	realHash, disabled, err := a.Store.GetAdmin(req.Username)

	hashToCompare := a.dummyHash
	userExists := err == nil
	if userExists {
		hashToCompare = []byte(realHash)
	}

	bcryptErr := bcrypt.CompareHashAndPassword(hashToCompare, []byte(req.Password))

	if bcryptErr != nil || !userExists || disabled == 1 {
		log.Printf("Login failed for user %q from IP %s", req.Username, clientIP)
		jsonError(w, "Invalid username or password", http.StatusUnauthorized)
		return
	}

	a.limiterMu.Lock()
	delete(a.ipLimits, clientIP)
	delete(a.userLimits, userIPKey)
	a.limiterMu.Unlock()

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		jsonError(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	token := hex.EncodeToString(tokenBytes)
	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	expires := time.Now().UTC().Add(sessionTTL)

	if err := a.Store.CreateSession(tokenHash, req.Username, expires.Format(time.RFC3339)); err != nil {
		log.Printf("Failed to insert session: %v", err)
		jsonError(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if err := a.Store.DeleteExpiredSessions(time.Now().Format(time.RFC3339)); err != nil {
		log.Printf("Failed to delete expired sessions: %v", err)
	}

	// The sign-in has succeeded; a failure to stamp it must not undo that.
	if err := a.Store.RecordLogin(req.Username); err != nil {
		log.Printf("Failed to record login for %q: %v", req.Username, err)
	}

	setSessionCookie(w, token, expires)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(audit.LoginResponse{Username: req.Username})
}

/*
 * HandleLogout processes POST /api/logout requests. It deletes the session row,
 * so the token stops working server-side rather than merely being forgotten by
 * the browser, and clears the cookie. Logging out without a session is not an
 * error.
 */
func (a *API) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !checkCSRF(r) {
		jsonError(w, "CSRF token missing or invalid", http.StatusForbidden)
		return
	}

	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(c.Value)))
		if err := a.Store.DeleteSession(tokenHash); err != nil {
			log.Printf("Failed to delete session on logout: %v", err)
		}
	}

	setSessionCookie(w, "", time.Unix(0, 0))
	w.WriteHeader(http.StatusOK)
}

/*
 * isAuthenticated validates the session cookie.
 * It enforces 24-hour expiration limits and purges expired tokens dynamically.
 */
func (a *API) isAuthenticated(r *http.Request) (bool, string) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false, ""
	}
	token := c.Value
	tokenHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))

	username, expiresAt, err := a.Store.GetSession(tokenHash)
	if err == nil {
		exp, _ := time.Parse(time.RFC3339, expiresAt)
		if time.Now().After(exp) {
			if err := a.Store.DeleteSession(tokenHash); err != nil {
				log.Printf("Failed to delete expired session %q: %v", tokenHash, err)
			}
			return false, ""
		}
		return true, username
	}

	return false, ""
}

/*
 * jsonError is a helper method that formats HTTP errors to match
 * the documented {"error":"message"} JSON contract for the frontend.
 */
func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

/*
 * isTruthy reports whether a query parameter should be read as "on".
 * Accepts the spellings a hand-written URL or a fetch() caller is likely to
 * use; anything else, including absent, is off.
 */
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

/*
 * parsePagination extracts and validates the limit and offset query parameters.
 * It returns a 400 Bad Request error if the inputs are negative or malformed.
 */
func parsePagination(r *http.Request) (int, int, error) {
	q := r.URL.Query()
	limit, offset := 50, 0
	if l := q.Get("limit"); l != "" {
		var err error
		if limit, err = strconv.Atoi(l); err != nil {
			return 0, 0, err
		}
		if limit < 0 {
			return 0, 0, errors.New("limit cannot be negative")
		}
	}
	if o := q.Get("offset"); o != "" {
		var err error
		if offset, err = strconv.Atoi(o); err != nil {
			return 0, 0, err
		}
		if offset < 0 {
			return 0, 0, errors.New("offset cannot be negative")
		}
	}
	return limit, offset, nil
}

/*
 * HandleEvents processes POST /api/events requests from the proxy sink.
 * It authenticates via the service token, so an unauthenticated caller cannot
 * write audit rows, then decodes a batch of audit events and persists them to the store via an idempotent upsert.
 */
func (a *API) HandleEvents(w http.ResponseWriter, r *http.Request) {
	if !a.isProxyService(r) {
		jsonError(w, "Unauthorized service token", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var events []audit.Event
	if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.Store.WriteBatch(events); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

/*
 * HandleConnections acts as a router for the /api/connections endpoints.
 * It handles authentication and directs traffic to either listConnections or getConnection.
 */
func (a *API) HandleConnections(w http.ResponseWriter, r *http.Request) {
	valid, _ := a.isAuthenticated(r)
	if !valid {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodGet {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimSuffix(r.URL.Path, "/")

	if path == "/api/connections" {
		a.listConnections(w, r)
	} else if strings.HasPrefix(path, "/api/connections/") {
		connID := strings.TrimPrefix(path, "/api/connections/")

		if strings.Contains(connID, "/") {
			jsonError(w, "Not found", http.StatusNotFound)
			return
		}
		a.getConnection(w, r, connID)
	} else {
		jsonError(w, "Not found", http.StatusNotFound)
	}
}

/*
 * listConnections parses filters from the request query and returns a paginated
 * list of aggregated connection metadata.
 */
func (a *API) listConnections(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePagination(r)
	if err != nil {
		jsonError(w, "invalid limit or offset", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	outcome := q.Get("outcome")
	if outcome != "" && outcome != string(audit.AuthOK) && outcome != string(audit.AuthFail) {
		jsonError(w, "outcome must be 'ok' or 'fail'", http.StatusBadRequest)
		return
	}
	filter := store.Filter{
		User:     q.Get("user"),
		Upstream: q.Get("upstream"),
		Outcome:  q.Get("outcome"),
		From:     q.Get("from"),
		To:       q.Get("to"),
		Limit:    limit,
		Offset:   offset,
		Open:     isTruthy(q.Get("open")),
	}

	conns, total, err := a.Store.GetConnections(filter)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":       total,
		"connections": conns,
	})
}

// getConnection retrieves a single connection by its ID along with its associated statements.
func (a *API) getConnection(w http.ResponseWriter, r *http.Request, connID string) {
	conn, stmts, err := a.Store.GetConnection(connID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conn == nil {
		jsonError(w, "Connection not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"connection": conn,
		"statements": stmts,
	})
}

/*
 * HandleStatements processes GET /api/statements requests.
 * It parses filters and pagination limits to return targeted individual statement logs.
 */
func (a *API) HandleStatements(w http.ResponseWriter, r *http.Request) {
	valid, _ := a.isAuthenticated(r)
	if !valid {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method != http.MethodGet {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit, offset, err := parsePagination(r)
	if err != nil {
		jsonError(w, "invalid limit or offset", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	if !store.ValidKind(q.Get("kind")) {
		jsonError(w, "kind must be read, write or schema", http.StatusBadRequest)
		return
	}
	filter := store.StatementFilter{
		ConnID: q.Get("conn_id"),
		User:   q.Get("user"),
		Type:   q.Get("type"),
		Q:      q.Get("q"),
		From:   q.Get("from"),
		To:     q.Get("to"),
		Limit:  limit,
		Offset: offset,
		// Opt-in, and only ever a view concern: the statements are still
		// stored and still served without this parameter.
		HideHandshake: isTruthy(q.Get("hide_handshake")),

		HasError: isTruthy(q.Get("has_error")),
		Kind:     q.Get("kind"),
		Upstream: q.Get("upstream"),
	}

	stmts, total, err := a.Store.GetStatements(filter)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total":      total,
		"statements": stmts,
	})
}

/*
 * ServerConfigReq is a flattened request structure for server mutations.
 * Flattening prevents JSON tag collisions during unmarshalling that occur when
 * embedding structs that share json tag definitions.
 */
type ServerConfigReq struct {
	TLS          *string `json:"tls"`
	TLSCAFile    *string `json:"tls_ca_file"`
	ID           int     `json:"id"`
	Name         string  `json:"name"`
	ListenPort   int     `json:"listen_port"`
	UpstreamHost string  `json:"upstream_host"`
	UpstreamPort int     `json:"upstream_port"`
	UpstreamUser string  `json:"upstream_user"`
	UpstreamPass string  `json:"upstream_pass"`
	Enabled      bool    `json:"enabled"`
}

/*
 * HandleServers processes GET, POST, PUT, and DELETE operations for /api/servers.
 * It enforces back-office UI sanitization by stripping cleartext passwords
 * on GET requests, replacing them with a boolean presence flag.
 */
func (a *API) HandleServers(w http.ResponseWriter, r *http.Request) {
	valid, activeUser := a.isAuthenticated(r)
	if !valid {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !checkCSRF(r) {
		jsonError(w, "CSRF token missing or invalid", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet:
		servers, err := a.Store.GetServers()
		if err != nil {
			jsonError(w, "Failed to fetch servers", http.StatusInternalServerError)
			return
		}

		rev, err := a.Store.GetRevision()
		if err != nil {
			jsonError(w, "Failed to fetch configuration revision", http.StatusInternalServerError)
			return
		}
		appliedRev, appliedAt, err := a.Store.GetAppliedRevision()
		if err != nil {
			jsonError(w, "Failed to fetch applied revision", http.StatusInternalServerError)
			return
		}

		uiServers := make([]map[string]interface{}, len(servers))
		for i, s := range servers {
			uiServers[i] = map[string]interface{}{
				"id":                    s.ID,
				"name":                  s.Name,
				"listen_port":           s.ListenPort,
				"upstream_host":         s.UpstreamHost,
				"upstream_port":         s.UpstreamPort,
				"upstream_user":         s.UpstreamUser,
				"has_upstream_password": s.UpstreamPass != "",
				"enabled":               s.Enabled,
				"tls":                   s.TLS,
				"tls_ca_file":           s.TLSCAFile,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"servers":          uiServers,
			"revision":         rev,
			"applied_revision": appliedRev,
			"applied_at":       appliedAt,
		})

	case http.MethodPost, http.MethodPut:
		var req ServerConfigReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.Name == "" || req.UpstreamHost == "" || req.UpstreamUser == "" {
			jsonError(w, "name, upstream_host, and upstream_user are required", http.StatusBadRequest)
			return
		}
		if req.ListenPort < 1 || req.ListenPort > 65535 || req.UpstreamPort < 1 || req.UpstreamPort > 65535 {
			jsonError(w, "ports must be between 1 and 65535", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost && req.UpstreamPass == "" {
			jsonError(w, "upstream_pass is required for new servers", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPut && req.ID <= 0 {
			jsonError(w, "valid server ID required for update", http.StatusBadRequest)
			return
		}

		mode := audit.TLSVerify
		if req.TLS != nil {
			var err error
			mode, err = audit.NormalizeTLS(*req.TLS)
			if err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		caFile := ""
		if req.TLSCAFile != nil {
			caFile = *req.TLSCAFile
		}
		var encryptedPass string
		if req.UpstreamPass != "" {
			var err error
			encryptedPass, err = encryptAES(req.UpstreamPass, a.configKey)
			if err != nil {
				jsonError(w, "Encryption failed", http.StatusInternalServerError)
				return
			}
		}

		serverEntity := audit.Server{
			TLS:          mode,
			TLSCAFile:    caFile,
			ID:           req.ID,
			Name:         req.Name,
			ListenPort:   req.ListenPort,
			UpstreamHost: req.UpstreamHost,
			UpstreamPort: req.UpstreamPort,
			UpstreamUser: req.UpstreamUser,
			UpstreamPass: encryptedPass,
			Enabled:      req.Enabled,
		}

		if r.Method == http.MethodPost {
			if err := a.Store.AddServer(serverEntity, encryptedPass); err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					jsonError(w, "Server name or listen port already exists", http.StatusConflict)
					return
				}
				jsonError(w, "Failed to create server", http.StatusInternalServerError)
				return
			}

			// TODO(follow-up): Write attribution in the same transaction as the change.
			if err := a.Store.LogAction(activeUser, "CREATE_SERVER", serverEntity.Name); err != nil {
				log.Printf("attribution write failed (CREATE_SERVER %s by %q): %v", serverEntity.Name, activeUser, err)
			}

		} else {
			if err := a.Store.UpdateServer(req.ID, serverEntity, encryptedPass, store.TLSUpdate{Mode: req.TLS, CAFile: req.TLSCAFile}); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					jsonError(w, "Server not found", http.StatusNotFound)
					return
				}
				if strings.Contains(err.Error(), "UNIQUE") {
					jsonError(w, "Server name or listen port already exists", http.StatusConflict)
					return
				}
				jsonError(w, "Failed to update server", http.StatusInternalServerError)
				return
			}

			// TODO(follow-up): Write attribution in the same transaction as the change.
			if err := a.Store.LogAction(activeUser, "UPDATE_SERVER", serverEntity.Name); err != nil {
				log.Printf("attribution write failed (UPDATE_SERVER %s by %q): %v", serverEntity.Name, activeUser, err)
			}
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		idStr := r.URL.Query().Get("id")
		id, err := strconv.Atoi(idStr)
		if err != nil || id <= 0 {
			jsonError(w, "Valid server ID parameter required", http.StatusBadRequest)
			return
		}

		// Fetch the name before the row is deleted using the exported method
		serverName := a.Store.GetServerName(id)

		if err := a.Store.DeleteServer(id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				jsonError(w, "Server already deleted", http.StatusNotFound)
				return
			}
			jsonError(w, "Failed to delete server", http.StatusInternalServerError)
			return
		}

		if serverName == "" {
			serverName = fmt.Sprintf("Server ID %d", id)
		}

		// TODO(follow-up): Write attribution in the same transaction as the change.
		if err := a.Store.LogAction(activeUser, "DELETE_SERVER", serverName); err != nil {
			log.Printf("attribution write failed (DELETE_SERVER %s by %q): %v", serverName, activeUser, err)
		}
		w.WriteHeader(http.StatusOK)

	default:
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

/*
 * isProxyService safely verifies the static proxy token using constant-time
 * comparison to prevent timing attacks.
 */
func (a *API) isProxyService(r *http.Request) bool {
	if a.proxyToken == "" {
		return false
	}
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(token), []byte(a.proxyToken)) == 1
}

/*
 * HandleProxyConfig serves GET /api/proxy/config requests for the proxy daemon.
 * It authenticates via the service token and serves fully decrypted passwords in-place.
 * It supports HTTP ETags for bandwidth-efficient polling.
 */
func (a *API) HandleProxyConfig(w http.ResponseWriter, r *http.Request) {
	if !a.isProxyService(r) {
		jsonError(w, "Unauthorized service token", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	servers, rev, err := a.Store.GetServersAndRevision()
	if err != nil {
		jsonError(w, "Failed to fetch configuration", http.StatusInternalServerError)
		return
	}

	etag := fmt.Sprintf(`"config-%d"`, rev)
	w.Header().Set("ETag", etag) // 304

	matchHeader := r.Header.Get("If-None-Match")
	for _, match := range strings.Split(matchHeader, ",") {
		if strings.TrimPrefix(strings.TrimSpace(match), "W/") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	for i := range servers {
		if servers[i].UpstreamPass == "" {
			continue
		}
		decrypted, err := decryptAES(servers[i].UpstreamPass, a.configKey)
		switch {
		case err != nil:
			log.Printf("proxy config: cannot decrypt password for server %q: %v", servers[i].Name, err)
			servers[i].UpstreamPass = ""
		case decrypted == "":
			servers[i].UpstreamPass = ""
		default:
			servers[i].UpstreamPass = decrypted
		}
	}

	payload := map[string]interface{}{
		"revision": rev,
		"servers":  servers,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(payload)
}

// HandleProxyStatus records which config revision the proxy reports it is running.
func (a *API) HandleProxyStatus(w http.ResponseWriter, r *http.Request) {
	if !a.isProxyService(r) {
		jsonError(w, "Unauthorized service token", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		AppliedRevision *int `json:"applied_revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if payload.AppliedRevision == nil || *payload.AppliedRevision < 0 {
		jsonError(w, "applied_revision must be present and non-negative", http.StatusBadRequest)
		return
	}

	if err := a.Store.SetAppliedRevision(*payload.AppliedRevision); err != nil {
		jsonError(w, "Failed to record applied revision", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// HandleCreateAdmin processes POST /api/admins requests.
func (a *API) HandleCreateAdmin(w http.ResponseWriter, r *http.Request) {
	valid, activeUser := a.isAuthenticated(r)
	if !valid {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !checkCSRF(r) {
		jsonError(w, "CSRF token missing or invalid", http.StatusForbidden)
		return
	}

	var req AdminRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	if !validUsername(req.Username) {
		jsonError(w, "Username must be 1-64 characters and must not contain '/'", http.StatusBadRequest)
		return
	}
	if !isValidPassword(req.Password) {
		jsonError(w, "Password must be between 8 and 72 bytes", http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		jsonError(w, "Failed to hash password", http.StatusInternalServerError)
		return
	}

	if err := a.Store.CreateAdminAs(activeUser, req.Username, string(hash)); err != nil {
		if errors.Is(err, store.ErrAdminExists) {
			jsonError(w, "Admin already exists", http.StatusConflict)
			return
		}
		log.Printf("create admin %q: %v", req.Username, err)
		jsonError(w, "Failed to create admin", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// HandleDisableAdmin processes PUT /api/admins/{username}/disable requests.
func (a *API) HandleDisableAdmin(w http.ResponseWriter, r *http.Request) {
	valid, activeUser := a.isAuthenticated(r)
	if !valid {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPut {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !checkCSRF(r) {
		jsonError(w, "CSRF token missing or invalid", http.StatusForbidden)
		return
	}

	target := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/admins/"), "/disable")
	if target == "" {
		jsonError(w, "Username required", http.StatusBadRequest)
		return
	}
	if target == activeUser {
		jsonError(w, "You cannot disable your own account", http.StatusForbidden)
		return
	}

	if err := a.Store.DisableAdmin(activeUser, target); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			jsonError(w, "Admin not found", http.StatusNotFound)
		case errors.Is(err, store.ErrLastAdmin):
			jsonError(w, "Cannot disable the last active admin", http.StatusConflict)
		default:
			log.Printf("disable admin %q: %v", target, err)
			jsonError(w, "Failed to disable admin", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
}

// HandleChangePassword processes PUT /api/admins/{username}/password requests.
func (a *API) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	valid, activeUser := a.isAuthenticated(r)
	if !valid {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPut {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !checkCSRF(r) {
		jsonError(w, "CSRF token missing or invalid", http.StatusForbidden)
		return
	}

	target := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/admins/"), "/password")
	if target == "" {
		jsonError(w, "Username required", http.StatusBadRequest)
		return
	}

	var req AdminRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if !isValidPassword(req.Password) {
		jsonError(w, "Password must be between 8 and 72 bytes", http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		jsonError(w, "Failed to hash password", http.StatusInternalServerError)
		return
	}

	if err := a.Store.ChangeAdminPassword(activeUser, target, string(hash)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, "Admin not found", http.StatusNotFound)
			return
		}
		log.Printf("change password %q: %v", target, err)
		jsonError(w, "Failed to change password", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

/*
 * SeedServers populates the servers table from the JSON file at path
 * ({"servers":[...]}) on first boot: only when the table is empty, in one
 * transaction, so restarts neither duplicate rows nor overwrite operator edits.
 */
func (a *API) SeedServers(path string, passwords map[string]string) (int, error) {
	populated, err := a.Store.HasServers()
	if err != nil {
		return 0, fmt.Errorf("check servers table: %w", err)
	}
	if populated {
		return 0, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read seed file: %w", err)
	}
	var config struct {
		Servers []audit.Server `json:"servers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(config.Servers) == 0 {
		return 0, fmt.Errorf("%s lists no servers", path)
	}

	prepared := make([]audit.Server, 0, len(config.Servers))
	for _, srv := range config.Servers {
		mode, modeErr := audit.NormalizeTLS(srv.TLS)
		if modeErr != nil {
			return 0, fmt.Errorf("server %q: %w", srv.Name, modeErr)
		}
		srv.TLS = mode
		if srv.Name == "" || srv.UpstreamHost == "" || srv.UpstreamUser == "" {
			return 0, fmt.Errorf("server %q (%s:%d): name, upstream_host and upstream_user are required",
				srv.Name, srv.UpstreamHost, srv.UpstreamPort)
		}
		if srv.ListenPort < 1 || srv.ListenPort > 65535 || srv.UpstreamPort < 1 || srv.UpstreamPort > 65535 {
			return 0, fmt.Errorf("server %q: ports must be between 1 and 65535", srv.Name)
		}

		pass := srv.UpstreamPass
		if pass == "" {
			pass = passwords[srv.Name]
		}
		if pass != "" {
			if srv.UpstreamPass, err = encryptAES(pass, a.configKey); err != nil {
				return 0, fmt.Errorf("server %q: %w", srv.Name, err)
			}
		}
		prepared = append(prepared, srv)
	}
	return a.Store.SeedServersIfEmpty(prepared)
}

// HandleSummary processes GET /api/summary?from=&to= requests.
func (a *API) HandleSummary(w http.ResponseWriter, r *http.Request) {
	valid, _ := a.isAuthenticated(r)
	if !valid {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	summary, err := a.Store.GetSummary(q.Get("from"), q.Get("to"))
	if err != nil {
		log.Printf("Failed to build summary: %v", err)
		jsonError(w, "Failed to fetch summary", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(summary)
}

// HandleListAdmins processes GET /api/admins requests.
func (a *API) HandleListAdmins(w http.ResponseWriter, r *http.Request) {
	valid, _ := a.isAuthenticated(r)
	if !valid {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	admins, err := a.Store.ListAdmins()
	if err != nil {
		jsonError(w, "Failed to fetch admins", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"admins": admins})
}

// HandleListActions processes GET /api/actions requests.
func (a *API) HandleListActions(w http.ResponseWriter, r *http.Request) {
	valid, _ := a.isAuthenticated(r)
	if !valid {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit, offset, err := parsePagination(r)
	if err != nil {
		jsonError(w, "invalid limit or offset", http.StatusBadRequest)
		return
	}
	if limit > 500 {
		limit = 500
	}
	actions, total, err := a.Store.ListActions(limit, offset)
	if err != nil {
		jsonError(w, "Failed to fetch actions", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"total": total, "actions": actions})
}

/*
 * HandleHealthz processes GET /healthz requests.
 * It is used as a liveness probe by Docker and Kubernetes environments.
 */
func (a *API) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
