package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/metrics"
	"github.com/local/image-prepuller/internal/provider"
	"github.com/local/image-prepuller/internal/reconcile"
	"github.com/local/image-prepuller/internal/spokeclient"
	"github.com/local/image-prepuller/internal/stats"
	"github.com/local/image-prepuller/internal/tasks"
	"github.com/local/image-prepuller/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: prepuller <controller|agent> [flags]")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "controller":
		runController(os.Args[2:])
	case "agent":
		runAgent(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (expected controller or agent)\n", os.Args[1])
		os.Exit(2)
	}
}

func runController(args []string) {
	fs := flag.NewFlagSet("controller", flag.ExitOnError)
	role := fs.String("role", "hub", "Controller role: hub (Web UI + local/remote clusters) or spoke (headless backend API)")
	httpAddr := fs.String("http-addr", ":8080", "HTTP listen address (Web UI in hub mode, internal API in spoke mode)")
	metricsAddr := fs.String("metrics-addr", ":9090", "Prometheus metrics listen address")
	mockMode := fs.Bool("mock", false, "Run with interactive in-memory mock data (no Kubernetes required)")
	clusterName := fs.String("cluster-name", "talos-prod-eu", "Local cluster name")
	autoWarm := fs.Bool("auto-warm", false, "Enable the auto-pull reconciler for local clusters (Stage 2/3)")
	workloadSelector := fs.String("workload-selector", os.Getenv("PREPULLER_WORKLOAD_SELECTOR"), "Kubernetes label selector of the workloads this controller manages (default: every workload). Workloads outside it are not listed, shown or pulled: e.g. prepull=true")
	nodeSelector := fs.String("node-selector", os.Getenv("PREPULLER_NODE_SELECTOR"), "Kubernetes label selector of the nodes in scope (default: every node), e.g. !node-role.kubernetes.io/control-plane when agents run on workers only")
	hubURL := fs.String("hub-url", os.Getenv("PREPULLER_HUB_URL"), "Spoke only: hub base URL (usually the hub ingress). Set it to run the spoke in outbound mode, where the spoke dials the hub instead of being reachable from it")
	hubSync := fs.Duration("hub-sync-interval", hubSyncIntervalFromEnv(), "Spoke only: how often the outbound spoke pushes cluster state to the hub")
	pullHistoryFile := fs.String("pull-history-file", os.Getenv("PREPULLER_PULL_HISTORY_FILE"), "Hub only: JSONL file keeping a bounded pull-task history across restarts (empty = memory only). Point it at a writable volume, e.g. /var/lib/prepuller/pull-tasks.jsonl")
	pullHistoryLimit := fs.Int("pull-history-limit", pullHistoryLimitFromEnv(), "Hub only: maximum number of pull tasks kept in the history (default 2000, PREPULLER_PULL_HISTORY_LIMIT)")
	statsDSN := fs.String("stats-dsn", os.Getenv("PREPULLER_STATS_DSN"), "Hub only: PostgreSQL DSN of the statistics pipeline (image lifetimes, collector activity, re-pulls, pull-task log). Empty disables it")
	statsInterval := fs.Duration("stats-interval", durationFromEnv("PREPULLER_STATS_INTERVAL", 30*time.Second), "Hub only: how often every cluster is sampled for the statistics pipeline")
	statsTimeout := fs.Duration("stats-timeout", durationFromEnv("PREPULLER_STATS_TIMEOUT", 15*time.Second), "Hub only: per-cluster snapshot timeout of one statistics sample")
	statsRetention := fs.Duration("stats-retention", durationFromEnv("PREPULLER_STATS_RETENTION", 90*24*time.Hour), "Hub only: how long statistics rows are kept (older rows are pruned)")
	statsPrune := fs.Duration("stats-prune-interval", durationFromEnv("PREPULLER_STATS_PRUNE_INTERVAL", 24*time.Hour), "Hub only: how often the statistics retention prune runs")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := provider.NewRegistry()

	if *role == "spoke" {
		var localProv provider.Provider
		if *mockMode {
			localProv = provider.NewMockProvider(*clusterName)
		} else {
			liveProv, err := provider.NewK8sProviderFromEnv(*clusterName,
				provider.WithAgentToken(os.Getenv("PREPULLER_AGENT_TOKEN")),
				provider.WithLabelSelector(*workloadSelector),
				provider.WithNodeSelector(*nodeSelector))
			if err != nil {
				log.Fatalf("live Kubernetes provider: %v", err)
			}
			log.Printf("live Kubernetes provider for cluster %s (node-agent API v1 on :%d)", *clusterName, provider.DefaultAgentPort)
			log.Printf("cluster scope: workloads %s, nodes %s", scopeLogValue(*workloadSelector, "all"), scopeLogValue(*nodeSelector, "all"))
			localProv = liveProv
		}
		reg.Register(provider.ClusterEntry{ID: *clusterName, Name: *clusterName, Role: "local", Provider: localProv})
		token := os.Getenv("PREPULLER_CLUSTER_TOKEN")
		spokeHandler := web.NewSpokeServer(localProv, token)

		// Outbound mode: the hub cannot reach this cluster (NAT, no ingress
		// here), so the spoke dials the hub and keeps the channel open. The
		// local listener stays for probes and metrics only.
		if target := strings.TrimSpace(*hubURL); target != "" {
			client, err := spokeclient.New(spokeclient.Config{
				HubURL:       target,
				Cluster:      *clusterName,
				Token:        token,
				Handler:      spokeHandler,
				Provider:     localProv,
				SyncInterval: *hubSync,
			})
			if err != nil {
				log.Fatalf("outbound spoke channel: %v", err)
			}
			go client.Run(ctx)
			log.Printf("spoke OUTBOUND MODE: dialing hub %s as cluster %q, state every %s (no inbound route to this cluster is needed)", target, *clusterName, *hubSync)
		} else {
			log.Printf("spoke DIRECT MODE: the hub must be able to reach this cluster's API over HTTP")
		}

		log.Printf("starting prepuller SPOKE (headless) cluster=%s on %s", *clusterName, *httpAddr)
		serve(ctx, *httpAddr, *metricsAddr, spokeHandler, metrics.Handler(reg))
		return
	}

	// Hub mode (Master cluster with Web UI + Auth + multi-cluster registry)
	users, err := auth.UsersFromEnv()
	if err != nil {
		log.Fatalf("auth config error: %v", err)
	}
	rateLimitCfg, err := auth.RateLimitConfigFromEnv()
	if err != nil {
		log.Fatalf("auth config error: %v", err)
	}
	authMgr, err := auth.NewManagerFromConfig(
		os.Getenv("PREPULLER_SESSION_SECRET"),
		users,
		auth.WithAuditor(auth.NewTeeAuditor(
			auth.NewJSONAuditor(os.Stderr),
			auth.NewMemoryAuditor(500),
		)),
		auth.WithRateLimit(rateLimitCfg),
		auth.WithSecureCookie(strings.EqualFold(os.Getenv("PREPULLER_COOKIE_SECURE"), "true")),
	)
	if err != nil {
		log.Fatalf("auth config error: %v", err)
	}
	defer func() { _ = authMgr.Close() }()

	if *mockMode {
		log.Printf("starting prepuller HUB in MOCK MODE (3 clusters)")
		reg.Register(provider.ClusterEntry{ID: *clusterName, Name: *clusterName, Role: "local", Provider: provider.NewMockProvider(*clusterName)})
		reg.Register(provider.ClusterEntry{ID: "talos-prod-kz", Name: "talos-prod-kz", Role: "remote", Endpoint: "https://prepuller-spoke.kz.internal", Provider: provider.NewMockProvider("talos-prod-kz")})
		reg.Register(provider.ClusterEntry{ID: "talos-stage-ru", Name: "talos-stage-ru", Role: "remote", Endpoint: "https://prepuller-spoke.ru.internal", Provider: provider.NewMockProvider("talos-stage-ru")})
	} else {
		log.Printf("starting prepuller HUB with the live Kubernetes provider (cluster %s)", *clusterName)
		liveProv, err := provider.NewK8sProviderFromEnv(*clusterName,
			provider.WithAgentToken(os.Getenv("PREPULLER_AGENT_TOKEN")),
			provider.WithLabelSelector(*workloadSelector),
			provider.WithNodeSelector(*nodeSelector))
		if err != nil {
			log.Fatalf("live Kubernetes provider: %v", err)
		}
		log.Printf("cluster scope: workloads %s, nodes %s", scopeLogValue(*workloadSelector, "all"), scopeLogValue(*nodeSelector, "all"))
		reg.Register(provider.ClusterEntry{ID: *clusterName, Name: *clusterName, Role: "local", Provider: liveProv})
	}

	// Optional extra remote spokes via PREPULLER_REMOTE_CLUSTERS="name=url=token,name2=url2=token2"
	if raw := strings.TrimSpace(os.Getenv("PREPULLER_REMOTE_CLUSTERS")); raw != "" {
		for _, item := range strings.Split(raw, ",") {
			parts := strings.SplitN(strings.TrimSpace(item), "=", 3)
			if len(parts) < 2 {
				continue
			}
			name, url := parts[0], parts[1]
			tok := ""
			if len(parts) == 3 {
				tok = parts[2]
			}
			reg.Register(provider.ClusterEntry{
				ID:       name,
				Name:     name,
				Role:     "remote",
				Endpoint: url,
				Provider: provider.NewRemoteProvider(name, url, tok),
			})
		}
	}

	// Managed clusters that connect outbound: the hub accepts their state and
	// hands out commands instead of calling them over HTTP.
	var serverOpts []web.Option
	if raw := strings.TrimSpace(os.Getenv("PREPULLER_MANAGED_CLUSTERS")); raw != "" {
		clusters, err := parseManagedClusters(raw)
		if err != nil {
			log.Fatalf("PREPULLER_MANAGED_CLUSTERS: %v", err)
		}
		pushHub := provider.NewSpokeHub(provider.SpokeHubConfig{})
		for _, cluster := range clusters {
			if err := pushHub.AddCluster(cluster.name, cluster.token); err != nil {
				log.Fatalf("PREPULLER_MANAGED_CLUSTERS: %v", err)
			}
			managed, err := pushHub.Provider(cluster.name)
			if err != nil {
				log.Fatalf("PREPULLER_MANAGED_CLUSTERS: %v", err)
			}
			reg.Register(provider.ClusterEntry{
				ID:       cluster.name,
				Name:     cluster.name,
				Role:     "remote",
				Endpoint: "outbound",
				Provider: managed,
			})
			log.Printf("managed cluster %q registered for the outbound spoke channel (it dials this hub)", cluster.name)
		}
		serverOpts = append(serverOpts, web.WithSpokeHub(pushHub))
	}

	// Bounded hub-side pull-task history: a longer, paginated task list than any
	// provider snapshot, optionally persisted across restarts. An unwritable
	// path must not prevent startup (read-only root filesystem, missing volume),
	// so it degrades to a memory-only store with a warning.
	historyLimit := *pullHistoryLimit
	if historyLimit <= 0 {
		historyLimit = tasks.DefaultCapacity
	}
	historyFile := strings.TrimSpace(*pullHistoryFile)
	taskHistory, err := tasks.Open(historyFile, historyLimit)
	persisted := err == nil && historyFile != ""
	if err != nil {
		log.Printf("pull-task history file disabled (%v), keeping the history in memory only", err)
		taskHistory, err = tasks.Open("", historyLimit)
		if err != nil {
			log.Fatalf("pull-task history: %v", err)
		}
	}
	defer func() {
		if closeErr := taskHistory.Close(); closeErr != nil {
			log.Printf("close pull-task history: %v", closeErr)
		}
	}()
	if persisted {
		log.Printf("pull-task history: %s (limit %d tasks)", historyFile, historyLimit)
	} else {
		log.Printf("pull-task history: memory only, limit %d tasks (set -pull-history-file to persist it)", historyLimit)
	}
	serverOpts = append(serverOpts, web.WithTaskHistory(taskHistory))

	// Optional PostgreSQL statistics pipeline: image lifetimes on nodes, the
	// image collector's cleanups, re-pull frequency/lag and the pull-task log.
	// Disabled without a DSN; the hub never blocks on it (best effort).
	startStatistics(ctx, reg, *statsDSN, stats.Config{
		Interval:        *statsInterval,
		SnapshotTimeout: *statsTimeout,
		Retention:       *statsRetention,
		PruneInterval:   *statsPrune,
	})

	webSrv, err := web.NewServer(reg, authMgr, serverOpts...)
	if err != nil {
		log.Fatalf("init web server: %v", err)
	}
	startReconcilers(ctx, reg, *autoWarm)
	serve(ctx, *httpAddr, *metricsAddr, webSrv, metrics.Handler(reg))
}

// startReconcilers runs one auto-warm reconciler per local cluster. Remote
// clusters are skipped: their provider talks to a spoke which runs its own
// reconciler next to the cluster.
func startReconcilers(ctx context.Context, reg *provider.Registry, enabled bool) {
	if !enabled {
		return
	}
	for _, id := range reg.AllIDs() {
		entry, ok := reg.Get(id)
		if !ok || entry.Role != "local" {
			continue
		}
		go func(clusterID string, p provider.Provider) {
			log.Printf("auto-warm reconciler started for cluster %s", clusterID)
			rec := reconcile.New(reconcile.Config{})
			rec.Start(ctx, p, clusterID, func(res reconcile.Result) {
				if len(res.Triggered) == 0 && len(res.Failures) == 0 {
					return
				}
				log.Printf("auto-warm %s: examined=%d planned=%d triggered=%d failures=%d duration=%s",
					clusterID, res.Examined, len(res.Planned), len(res.Triggered), len(res.Failures),
					res.Duration.Round(time.Millisecond))
			})
		}(id, entry.Provider)
	}
}

func serve(ctx context.Context, httpAddr, metricsAddr string, mainHandler, metricsHandler http.Handler) {
	uiHTTP := &http.Server{Addr: httpAddr, Handler: mainHandler, ReadHeaderTimeout: 10 * time.Second}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", metricsHandler)
	metricsHTTP := &http.Server{Addr: metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		log.Printf("Prometheus metrics listening on http://%s/metrics", metricsAddr)
		if err := metricsHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("metrics server error: %v", err)
		}
	}()
	go func() {
		log.Printf("HTTP server listening on http://%s", httpAddr)
		if err := uiHTTP.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = uiHTTP.Shutdown(shutdownCtx)
	_ = metricsHTTP.Shutdown(shutdownCtx)
}

// hubSyncIntervalFromEnv resolves the outbound state-push interval: the
// PREPULLER_HUB_SYNC_INTERVAL env var wins over the built-in default.
func hubSyncIntervalFromEnv() time.Duration {
	const fallback = 3 * time.Second
	raw := strings.TrimSpace(os.Getenv("PREPULLER_HUB_SYNC_INTERVAL"))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("PREPULLER_HUB_SYNC_INTERVAL=%q is not a positive duration, using %s", raw, fallback)
		return fallback
	}
	return d
}

// pullHistoryLimitFromEnv resolves the bounded pull-task history capacity:
// PREPULLER_PULL_HISTORY_LIMIT wins over the built-in default.
func pullHistoryLimitFromEnv() int {
	const fallback = 2000
	raw := strings.TrimSpace(os.Getenv("PREPULLER_PULL_HISTORY_LIMIT"))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("PREPULLER_PULL_HISTORY_LIMIT=%q is not a positive integer, using %d", raw, fallback)
		return fallback
	}
	return n
}

// startStatistics runs the optional PostgreSQL statistics pipeline: every
// cluster registered on the hub is sampled on an interval, diffed into image
// presence intervals, GC events, re-pulls and a pull-task log, and written to
// the database. No DSN means no statistics and no extra goroutine.
//
// The hub must not depend on the database: a failed connect is retried with
// backoff, a failed write is logged and retried by the next sample (the
// collector only commits a diff after a successful write), and a write outage
// never blocks the UI, the metrics scrape or a pull.
func startStatistics(ctx context.Context, reg *provider.Registry, dsn string, cfg stats.Config) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return
	}
	ids := reg.AllIDs()
	clusters := make([]stats.Cluster, 0, len(ids))
	for _, id := range ids {
		entry, ok := reg.Get(id)
		if !ok {
			continue
		}
		clusters = append(clusters, stats.Cluster{ID: id, Snapshot: entry.Provider.Snapshot})
	}
	if len(clusters) == 0 {
		log.Printf("statistics: no clusters registered, pipeline not started")
		return
	}
	go func() {
		backoff := time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			store, err := stats.OpenPostgres(ctx, dsn)
			if err != nil {
				log.Printf("statistics: PostgreSQL unavailable (%v), retrying in %s", err, backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < time.Minute {
					backoff *= 2
				}
				continue
			}
			backoff = time.Second
			log.Printf("statistics: writing to PostgreSQL every %s, retention %s, clusters: %s",
				cfg.Interval, cfg.Retention, strings.Join(ids, ","))
			// Run owns the store: it closes it when the context is canceled.
			stats.Run(ctx, cfg, clusters, store, log.Printf)
			if ctx.Err() != nil {
				return
			}
			log.Printf("statistics: writer stopped, reconnecting")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}()
}

// durationFromEnv resolves a positive duration from an environment variable,
// falling back to the built-in default with a warning on a malformed value.
func durationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("%s=%q is not a positive duration, using %s", name, raw, fallback)
		return fallback
	}
	return d
}

// managedCluster is one entry of PREPULLER_MANAGED_CLUSTERS.
type managedCluster struct {
	name  string
	token string
}

// parseManagedClusters parses "name=token,name2=token2": the managed clusters
// whose spokes dial this hub outbound. Each token is the cluster token of that
// spoke release (PREPULLER_CLUSTER_TOKEN).
func parseManagedClusters(raw string) ([]managedCluster, error) {
	var out []managedCluster
	seen := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("entry %q: expected name=token", item)
		}
		name := strings.TrimSpace(parts[0])
		token := strings.TrimSpace(parts[1])
		if name == "" || token == "" {
			return nil, fmt.Errorf("entry %q: name and token are required", item)
		}
		if seen[name] {
			return nil, fmt.Errorf("cluster %q is listed twice", name)
		}
		seen[name] = true
		out = append(out, managedCluster{name: name, token: token})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no name=token entries")
	}
	return out, nil
}

// scopeLogValue renders a label selector for the startup log ("all" when unset).
func scopeLogValue(selector, empty string) string {
	if v := strings.TrimSpace(selector); v != "" {
		return v
	}
	return empty
}
