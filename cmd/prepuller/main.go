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
		fmt.Fprintln(os.Stderr, "agent mode will be implemented in Stage 2 (CRI /run/containerd/containerd.sock)")
		os.Exit(1)
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
	_ = fs.Parse(args)

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
		serve(*httpAddr, *metricsAddr, web.NewSpokeServer(localProv, token), metrics.Handler(reg))
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
	serve(*httpAddr, *metricsAddr, webSrv, metrics.Handler(reg))
}

func serve(httpAddr, metricsAddr string, mainHandler, metricsHandler http.Handler) {
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = uiHTTP.Shutdown(ctx)
	_ = metricsHTTP.Shutdown(ctx)
}
