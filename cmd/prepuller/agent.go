package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/local/image-prepuller/internal/agent"
)

// runAgent parses node-agent flags and serves the agent HTTP/JSON API for the
// hub until SIGINT/SIGTERM. This file belongs to WS-A.
func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	nodeName := fs.String("node-name", envOr("PREPULLER_NODE_NAME", hostname()), "Kubernetes node name")
	criSocket := fs.String("cri-socket", envOr("PREPULLER_CRI_SOCKET", "/run/containerd/containerd.sock"), "CRI runtime socket path")
	listenAddr := fs.String("listen-addr", envOr("PREPULLER_AGENT_ADDR", ":9443"), "agent API listen address for the hub")
	diskPause := fs.Float64("disk-pause-ratio", 0.78, "Pause auto-prepull when ImageFS usage exceeds this ratio")
	mock := fs.Bool("mock", false, "serve in-memory images instead of talking to containerd")
	token := fs.String("token", envOr("PREPULLER_AGENT_TOKEN", ""), "Bearer token for the agent API (empty disables auth until WS-F)")
	showVersion := fs.Bool("version", false, "print agent version and exit")
	_ = fs.Parse(args)

	if *showVersion {
		fmt.Println(agent.Version)
		return
	}

	cfg := agent.Config{
		NodeName:       *nodeName,
		CRISocket:      *criSocket,
		ListenAddr:     *listenAddr,
		DiskPauseRatio: *diskPause,
		Mock:           *mock,
		Token:          *token,
	}
	a, err := agent.New(cfg)
	if err != nil {
		log.Fatalf("agent config error: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("starting prepuller NODE AGENT node=%s cri=%s listen=%s mock=%v",
		cfg.NodeName, cfg.CRISocket, cfg.ListenAddr, cfg.Mock)
	if err := a.Run(ctx); err != nil {
		log.Fatalf("agent: %v", err)
	}
	log.Printf("agent %s: stopped", cfg.NodeName)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}
