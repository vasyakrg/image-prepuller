package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/local/image-prepuller/internal/agent"
)

// runAgent parses node-agent flags and starts the CRI agent. The gRPC server is
// implemented in WS-A (stage 2); until then the binary fails loudly instead of
// pretending to run. This file belongs to WS-A after the contract freeze.
func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	nodeName := fs.String("node-name", envOr("PREPULLER_NODE_NAME", hostname()), "Kubernetes node name")
	criSocket := fs.String("cri-socket", envOr("PREPULLER_CRI_SOCKET", "/run/containerd/containerd.sock"), "CRI runtime socket path")
	listenAddr := fs.String("listen-addr", envOr("PREPULLER_AGENT_ADDR", ":9443"), "gRPC listen address for the hub")
	diskPause := fs.Float64("disk-pause-ratio", 0.78, "Pause auto-prepull when ImageFS usage exceeds this ratio")
	_ = fs.Parse(args)

	cfg := agent.Config{
		NodeName:       *nodeName,
		CRISocket:      *criSocket,
		ListenAddr:     *listenAddr,
		DiskPauseRatio: *diskPause,
	}
	a, err := agent.New(cfg)
	if err != nil {
		log.Fatalf("agent config error: %v", err)
	}
	log.Printf("starting prepuller NODE AGENT node=%s cri=%s listen=%s", cfg.NodeName, cfg.CRISocket, cfg.ListenAddr)
	if err := a.Run(context.Background()); err != nil {
		log.Fatalf("agent: %v", err)
	}
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
