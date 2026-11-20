// Package app loads process configuration and wires the proxy runtime.
package app

import (
	"log"
	"os"
	"strings"

	"github.com/tevans2/mysql-audit-proxy/internal/proxy/config"
	"github.com/tevans2/mysql-audit-proxy/internal/sink"
)

// Run loads process configuration and starts the proxy.
func Run() {
	apiURL := config.GetEnv("API_URL", "")
	if apiURL == "" {
		sinkURL := config.GetEnv("SINK_INGEST_URL", "http://127.0.0.1:8080/api/events")
		apiURL = strings.TrimSuffix(sinkURL, "/api/events")
	}

	// The proxy authenticates to the API as a service, not as the
	// administrator: the operator's session token only opens the sanitised
	// back-office endpoints, and those do not carry upstream credentials.
	proxyToken := config.GetEnv("API_PROXY_TOKEN", "")
	configPath := config.GetEnv("PROXY_CONFIG_FILE", "")

	upstreamReadTimeout := config.GetEnvDuration("PROXY_UPSTREAM_READ_TIMEOUT", config.DefaultUpstreamReadTimeout)
	upstreamCommandTimeout := config.GetEnvDuration("PROXY_COMMAND_TIMEOUT", 0)

	// PROXY_USERS used to hold the logins the proxy itself accepted. It no
	// longer exists: clients now authenticate against the database as
	// themselves. Refuse to start rather than ignore it, because a deployment
	// that still sets it believes it is restricting who may connect.
	if _, set := os.LookupEnv("PROXY_USERS"); set {
		log.Fatal("PROXY_USERS is no longer supported: the proxy passes each client's login through to the database, " +
			"which decides who may connect. Remove it, and create the users in the database (see cmd/proxy/README.md).")
	}

	auditSink, closeSink := config.NewAuditSink()

	// The admin server comes up before the configuration is loaded, so a probe
	// can see "not ready, waiting for the API" instead of "connection refused".
	// Unset means no admin server at all.
	if sp, ok := auditSink.(sink.StatsProvider); ok {
		admin.setStats(sp)
	}
	if adminAddr := config.GetEnv("PROXY_ADMIN_ADDR", ""); adminAddr != "" {
		startAdminServer(adminAddr)
	}

	source, err := config.ChooseConfigSource(configPath != "", proxyToken != "")
	if err != nil {
		log.Fatal(err)
	}
	switch source {
	case config.ConfigSourceFile:
		runStaticProxy(configPath, auditSink, closeSink, upstreamReadTimeout, upstreamCommandTimeout)
	case config.ConfigSourceAPI:
		runLiveProxy(apiURL, proxyToken, auditSink, upstreamReadTimeout, upstreamCommandTimeout)
	}
}
