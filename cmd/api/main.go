/* Package main is the entry point for the back-office API.
 *
 * It serves the endpoints in docs/design/data-model.md §REST API: ingest from
 * the proxy's sink on POST /api/events, and the read endpoints the web UI
 * consumes. Storage is SQLite via internal/store.
 */
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tevans2/mysql-audit-proxy/internal/api"
	"github.com/tevans2/mysql-audit-proxy/internal/retention"
	"github.com/tevans2/mysql-audit-proxy/internal/store"
)

const (
	/*
	 * shutdownTimeout bounds how long in-flight requests get to finish once a
	 * termination signal arrives.
	 */
	shutdownTimeout = 10 * time.Second

	/*
	 * readHeaderTimeout stops a client holding a connection open by trickling
	 * request headers.
	 */
	readHeaderTimeout = 10 * time.Second
)

/*
 * getEnv returns the value of the environment variable named by key, or
 * fallback if the variable is not set.
 */
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

/*
 * parseKV parses "name:password,name:password". Same format as the proxy's
 * PROXY_UPSTREAM_PASSWORDS.
 */
func parseKV(s string) (map[string]string, error) {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, ":")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q is not name:password", pair)
		}
		m[k] = v
	}
	return m, nil
}

// routes wires the API handlers to the paths in §REST API.
func routes(a *api.API) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", a.HandleHealthz)
	mux.HandleFunc("/api/events", a.HandleEvents)
	mux.HandleFunc("/api/login", a.HandleLogin)
	mux.HandleFunc("/api/logout", a.HandleLogout)

	/*
	 * Both patterns: HandleConnections does its own sub-path routing for
	 * /api/connections/{conn_id}, and ServeMux only passes the subtree to a
	 * pattern ending in a slash.
	 */
	mux.HandleFunc("/api/connections", a.HandleConnections)
	mux.HandleFunc("/api/connections/", a.HandleConnections)

	mux.HandleFunc("/api/statements", a.HandleStatements)
	mux.HandleFunc("/api/servers", a.HandleServers)
	mux.HandleFunc("/api/proxy/config", a.HandleProxyConfig)
	mux.HandleFunc("/api/proxy/status", a.HandleProxyStatus)

	mux.HandleFunc("/api/admins", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			a.HandleListAdmins(w, r)
		} else {
			a.HandleCreateAdmin(w, r)
		}
	})

	mux.HandleFunc("/api/actions", a.HandleListActions)
	mux.HandleFunc("/api/summary", a.HandleSummary)

	// Parse /api/admins/{u}/{verb} instead of relying on HasSuffix
	mux.HandleFunc("/api/admins/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/admins/"), "/")
		if len(parts) == 2 {
			verb := parts[1]
			if verb == "disable" {
				a.HandleDisableAdmin(w, r)
				return
			} else if verb == "password" {
				a.HandleChangePassword(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"Not found"}`))
	})

	return mux
}

func main() {
	listenAddr := getEnv("API_LISTEN_ADDR", ":8080")
	dbPath := getEnv("API_DB_PATH", "auditproxy.db")

	st, err := store.InitDB(dbPath)
	if err != nil {
		log.Fatalf("open store %s: %v", dbPath, err)
	}
	defer st.Close()

	apiInstance := &api.API{Store: st}
	adminUser := getEnv("API_ADMIN_USER", "admin")
	adminHash := os.Getenv("API_ADMIN_PASS_HASH")
	if adminHash == "" {
		log.Fatal("API_ADMIN_PASS_HASH is not set — refusing to start with default credentials")
	}

	configKeyStr := os.Getenv("API_CONFIG_KEY")
	if len(configKeyStr) != 32 {
		log.Fatal("API_CONFIG_KEY must be exactly 32 bytes and explicitly set in the environment")
	}

	proxyToken := os.Getenv("API_PROXY_TOKEN")
	if proxyToken == "" {
		log.Fatal("API_PROXY_TOKEN is not set — refusing to start without a secure service token")
	}

	/*
	 * API_ADMIN_USER / API_ADMIN_PASS_HASH bootstrap the FIRST admin only: they
	 * are applied when the admins table is empty and ignored afterwards.
	 */
	stopTicker, err := apiInstance.InitAuth(adminUser, adminHash, []byte(configKeyStr), proxyToken)
	if err != nil {
		log.Fatalf("init auth: %v", err)
	}
	defer stopTicker() // Stops the background purge goroutine on shutdown

	/*
	 * First-boot seeding of the servers table. Fatal on error: if the operator
	 * named a seed file and it can't be applied, starting with an empty table
	 * would silently reproduce the "nothing supplies the config" bug.
	 */
	if seedPath := os.Getenv("API_CONFIG_PATH"); seedPath != "" {
		passwords, err := parseKV(os.Getenv("API_UPSTREAM_PASSWORDS"))
		if err != nil {
			log.Fatalf("API_UPSTREAM_PASSWORDS: %v", err)
		}
		n, err := apiInstance.SeedServers(seedPath, passwords)
		if err != nil {
			log.Fatalf("seed servers from API_CONFIG_PATH=%s: %v", seedPath, err)
		}
		if n > 0 {
			log.Printf("seeded %d server(s) from %s", n, seedPath)
		} else {
			log.Printf("servers table already populated: %s not applied", seedPath)
		}
	}

	/*
	 * Audit-log retention. The policy is validated before anything is served:
	 * an invalid value, or an archive directory that cannot be written, stops
	 * the API here rather than on the first purge. Retention is off unless
	 * API_RETENTION_DAYS is set, and nothing is purged at startup: the first
	 * run is one interval after the API starts. See
	 * docs/design/decisions/0006-audit-log-retention.md.
	 */
	retentionCfg, err := retention.ConfigFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("audit retention: %v", err)
	}
	retentionCtx, stopRetention := context.WithCancel(context.Background())
	retentionDone := make(chan struct{})
	switch {
	case !retentionCfg.Enabled():
		log.Printf("audit retention: off (API_RETENTION_DAYS=0, the default), audit records are kept for ever; %d days is the recommended setting",
			retention.RecommendedDays)
		if retentionCfg.DryRun {
			log.Printf("audit retention: API_RETENTION_DRY_RUN=true has no effect while API_RETENTION_DAYS=0")
		}
		close(retentionDone)
	default:
		switch {
		case retentionCfg.DryRun:
			log.Printf("audit retention: DRY RUN, records older than %d day(s) are only logged as would-be %s, nothing is changed, checked every %s",
				retentionCfg.Days, string(retentionCfg.Action), retentionCfg.Interval)
		case retentionCfg.Action == retention.ActionArchive:
			if err := retention.PrepareArchiveDir(retentionCfg.ArchiveDir); err != nil {
				log.Fatalf("audit retention: %v", err)
			}
			log.Printf("audit retention: records older than %d day(s) are archived to %s and then deleted, checked every %s",
				retentionCfg.Days, retentionCfg.ArchiveDir, retentionCfg.Interval)
		default:
			log.Printf("audit retention: records older than %d day(s) are deleted, checked every %s",
				retentionCfg.Days, retentionCfg.Interval)
		}
		runner := &retention.Runner{Store: st, Cfg: retentionCfg, Logf: log.Printf}
		go func() {
			defer close(retentionDone)
			runner.Loop(retentionCtx)
		}()
	}
	/*
	 * Stop the purge and wait for the pass in progress before the deferred
	 * st.Close(), so the store is never closed under it.
	 */
	defer func() {
		stopRetention()
		<-retentionDone
	}()

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           routes(apiInstance),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("API listening on %s, store at %s", listenAddr, dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		if err != nil {
			log.Fatalf("serve: %v", err)
		}
	case sig := <-stop:
		log.Printf("received %s, shutting down", sig)

		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}
