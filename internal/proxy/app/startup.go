package app

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"lustre/internal/audit"
	"lustre/internal/proxy/config"
	"lustre/internal/proxy/runtime"
)

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

// runStaticProxy is the PROXY_CONFIG_FILE source: load the file once at
// startup and never look at it again — the ticket's "Static file
// configuration enabled: current startup-only behavior" requirement,
// unchanged from before this ticket. It blocks until one of the resulting
// proxies' Run returns, which in ordinary operation is never, since nothing
// here ever cancels the context they run under.
func runStaticProxy(configPath string, auditSink audit.Sink, closeSink func(), upstreamReadTimeout, upstreamCommandTimeout time.Duration) {
	servers, err := config.LoadUpstreamConfig(configPath)
	if err != nil {
		log.Fatalf("PROXY_CONFIG_FILE=%s: %v", configPath, err)
	}
	passwords, err := config.ParseKV(config.GetEnv("PROXY_UPSTREAM_PASSWORDS", ""), "PROXY_UPSTREAM_PASSWORDS")
	if err != nil {
		log.Fatal(err)
	}
	proxies, err := runtime.BuildProxies(servers, passwords, auditSink, upstreamReadTimeout, upstreamCommandTimeout)
	if err != nil {
		log.Fatalf("PROXY_CONFIG_FILE=%s: %v", configPath, err)
	}
	names := make([]string, len(proxies))
	for i, p := range proxies {
		names[i] = fmt.Sprintf("%s (%s)", p.UpstreamName, p.ListenAddr)
	}
	log.Printf("PROXY_CONFIG_FILE=%s: routing %s", configPath, strings.Join(names, ", "))

	// context.Background(): nothing cancels this yet — PROXY_CONFIG_FILE is
	// the non-reloading fallback, so there is no reconciliation loop here to
	// ever ask these listeners to stop.
	runErr := runtime.RunAllNotify(context.Background(), proxies, admin.setReady)

	closeSink()
	if runErr != nil {
		log.Fatal(runErr)
	}
}

// runLiveProxy is the API_PROXY_TOKEN source: fetch an initial complete
// snapshot (retrying transient failures up to a startup deadline), validate
// it and bind every listener it describes, then keep polling for new
// revisions and reconciling them for as long as the process runs — the
// ticket's live-reconciliation configuration source, replacing the old
// one-time fetchProxyConfig/fetchProxyConfigWithRetry load this ticket
// exists to remove.
//
// It blocks forever: config.ConfigClient.Poll only returns once its context is
// cancelled, and nothing here ever cancels context.Background() — matching
// runStaticProxy and runtime.RunAll's own equivalent "blocks forever in the normal
// case" behaviour.
func runLiveProxy(apiURL, proxyToken string, auditSink audit.Sink, upstreamReadTimeout, upstreamCommandTimeout time.Duration) {
	client := &config.ConfigClient{
		BaseURL:        apiURL,
		Token:          proxyToken,
		RequestTimeout: config.GetEnvDuration("PROXY_CONFIG_REQUEST_TIMEOUT", config.DefaultConfigRequestTimeout),
		PollInterval:   config.GetEnvDuration("PROXY_CONFIG_POLL_INTERVAL", config.DefaultConfigPollInterval),
		RetryMax:       config.GetEnvDuration("PROXY_CONFIG_RETRY_MAX", config.DefaultConfigRetryMax),
	}
	startupDeadline := config.GetEnvDuration("PROXY_CONFIG_STARTUP_DEADLINE", config.DefaultConfigStartupDeadline)

	ctx := context.Background()

	snap, err := config.FetchInitialSnapshotWithRetry(ctx, client, startupDeadline)
	if err != nil {
		log.Fatalf("Fatal: could not load initial proxy configuration from API: %v", err)
	}

	manager := runtime.NewManager()
	items, err := manager.Plan(snap)
	if err != nil {
		log.Fatalf("Fatal: initial proxy configuration from API (revision %d) failed validation: %v", snap.Revision, err)
	}

	reconciler := runtime.NewReconciler(auditSink)
	reconciler.Logger = log.Default()
	reconciler.UpstreamReadTimeout = upstreamReadTimeout
	reconciler.UpstreamCommandTimeout = upstreamCommandTimeout
	reconciler.DrainTimeout = config.GetEnvDuration("PROXY_DRAIN_TIMEOUT", config.DefaultDrainTimeout)

	results := reconciler.Apply(ctx, items)
	for _, res := range results {
		if res.Err == nil {
			manager.RecordApplied(res.Item)
		}
	}
	if !runtime.AllSucceeded(results) {
		// Startup has no last known-good configuration to fall back to yet
		// — unlike a later poll's partial failure, there is nothing safe to
		// keep running with, so this is fatal rather than a retry-on-next-
		// poll situation. The per-item errors were already logged by
		// Reconciler.Apply.
		log.Fatalf("Fatal: could not bind every listener from the initial proxy configuration (revision %d); see the errors above", snap.Revision)
	}
	manager.RecordRevision(snap.Revision)
	client.CommitETag(snap.ETag)
	// Best effort, same as the poll path: the back-office should show what is
	// running from the moment the proxy starts, not only after a later change.
	if err := client.ReportApplied(ctx, snap.Revision); err != nil {
		log.Printf("could not report applied revision %d to the API: %v", snap.Revision, err)
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		listenAddr := net.JoinHostPort("0.0.0.0", strconv.Itoa(item.Desired.ListenPort))
		upstreamAddr := net.JoinHostPort(item.Desired.UpstreamHost, strconv.Itoa(item.Desired.UpstreamPort))
		names = append(names, fmt.Sprintf("%s (%s -> %s)", item.Desired.Name, listenAddr, upstreamAddr))
	}
	if len(names) == 0 {
		log.Printf("Loaded initial config revision %d from API: no enabled servers yet; waiting for a later revision to add one", snap.Revision)
	} else {
		log.Printf("Loaded initial config revision %d from API: routing %s", snap.Revision, strings.Join(names, ", "))
	}

	admin.setAppliedRevision(manager.AppliedRevision)
	admin.setReady()

	runtime.PollConfig(ctx, client, manager, reconciler)
}
