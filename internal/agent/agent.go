// Package agent is the per-node prepuller daemon: it talks to the container
// runtime over CRI, inventories cached images and serves them to the hub over a
// small HTTP/JSON API. Stage 2 workstream WS-A (docs/PLAN.md section 5).
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// Version is reported to the hub in Snapshot.AgentVersion and in the
// prepuller_agent_build_info metric.
const Version = "0.2.0-dev"

// Config is the node-agent configuration derived from flags and environment.
type Config struct {
	NodeName       string
	CRISocket      string
	ListenAddr     string
	DiskPauseRatio float64
	// Mock serves an in-memory CRI backend; CRISocket is then ignored.
	Mock bool
	// Token enables Bearer authentication on the agent API. Empty disables it
	// for local development; full auth/RBAC lands in WS-F.
	Token string
}

// Agent owns the CRI client and the HTTP server.
type Agent struct {
	cfg Config
}

// New validates the configuration; it does not dial CRI yet.
func New(cfg Config) (*Agent, error) {
	if cfg.NodeName == "" {
		return nil, errors.New("agent: node name is required")
	}
	if !cfg.Mock && cfg.CRISocket == "" {
		return nil, errors.New("agent: CRI socket path is required")
	}
	if cfg.ListenAddr == "" {
		return nil, errors.New("agent: listen address is required")
	}
	if cfg.DiskPauseRatio <= 0 || cfg.DiskPauseRatio >= 1 {
		return nil, fmt.Errorf("agent: disk pause ratio %v is outside (0,1)", cfg.DiskPauseRatio)
	}
	return &Agent{cfg: cfg}, nil
}

// Config returns a copy of the effective configuration.
func (a *Agent) Config() Config { return a.cfg }

// Run starts the CRI backend and the agent server until ctx is canceled. In
// mock mode no CRI socket is dialed, so the binary runs without containerd.
func (a *Agent) Run(ctx context.Context) error {
	var (
		cri     CRI
		closer  func() error
		backend string
	)
	if a.cfg.Mock {
		cri = NewMockBackend()
		backend = "mock"
	} else {
		client, err := DialCRI(ctx, a.cfg.CRISocket)
		if err != nil {
			return err
		}
		cri = client
		closer = client.Close
		backend = "cri:" + a.cfg.CRISocket
	}
	if closer != nil {
		defer func() { _ = closer() }()
	}
	srv, err := NewServer(a.cfg, cri)
	if err != nil {
		return err
	}
	log.Printf("agent %s: backend=%s listen=%s api=%s auth=%v",
		a.cfg.NodeName, backend, a.cfg.ListenAddr, WireAPIVersion, a.cfg.Token != "")
	return srv.Serve(ctx)
}
