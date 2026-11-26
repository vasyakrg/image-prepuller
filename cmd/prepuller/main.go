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
	"strings"
	"syscall"
	"time"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/metrics"
	"github.com/local/image-prepuller/internal/provider"
	"github.com/local/image-prepuller/internal/reconcile"
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
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := provider.NewRegistry()

	if *role == "spoke" {
		var localProv provider.Provider
		if *mockMode {
			localProv = provider.NewMockProvider(*clusterName)
		} else {
			log.Fatalf("live Kubernetes provider is Stage 3; run with --mock for Stage 1")
		}
		reg.Register(provider.ClusterEntry{ID: *clusterName, Name: *clusterName, Role: "local", Provider: localProv})
		token := os.Getenv("PREPULLER_CLUSTER_TOKEN")
		log.Printf("starting prepuller SPOKE (headless) cluster=%s on %s", *clusterName, *httpAddr)
		serve(ctx, *httpAddr, *metricsAddr, web.NewSpokeServer(localProv, token), metrics.Handler(reg))
		return
	}

	// Hub mode (Master cluster with Web UI + Auth + multi-cluster registry)
	authMgr, err := auth.NewManager(auth.Config{
		Username:      os.Getenv("PREPULLER_AUTH_USER"),
		Password:      os.Getenv("PREPULLER_AUTH_PASS"),
		SessionSecret: []byte(os.Getenv("PREPULLER_SESSION_SECRET")),
	})
	if err != nil {
		log.Fatalf("auth config error: %v", err)
	}

	if *mockMode {
		log.Printf("starting prepuller HUB in MOCK MODE (3 clusters)")
		reg.Register(provider.ClusterEntry{ID: *clusterName, Name: *clusterName, Role: "local", Provider: provider.NewMockProvider(*clusterName)})
		reg.Register(provider.ClusterEntry{ID: "talos-prod-kz", Name: "talos-prod-kz", Role: "remote", Endpoint: "https://prepuller-spoke.kz.internal", Provider: provider.NewMockProvider("talos-prod-kz")})
		reg.Register(provider.ClusterEntry{ID: "talos-stage-ru", Name: "talos-stage-ru", Role: "remote", Endpoint: "https://prepuller-spoke.ru.internal", Provider: provider.NewMockProvider("talos-stage-ru")})
	} else {
		log.Fatalf("live Kubernetes provider is Stage 3; run with --mock for Stage 1")
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

	webSrv, err := web.NewServer(reg, authMgr)
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
