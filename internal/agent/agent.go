// Package agent is the per-node prepuller daemon: it talks to the container
// runtime over CRI, inventories cached images and serves pull requests to the
// hub over gRPC. Stage 2 workstream WS-A (docs/PLAN.md section 5).
package agent

import (
	"context"
	"errors"
	"fmt"
)

// Version is reported to the hub in NodeInfo.AgentVersion and in the
// prepuller_agent_build_info metric.
const Version = "0.2.0-dev"

// ErrNotImplemented marks agent features that WS-A still has to build.
var ErrNotImplemented = errors.New("agent: not implemented yet")

// Config is the node-agent configuration derived from flags and environment.
type Config struct {
	NodeName       string
	CRISocket      string
	ListenAddr     string
	DiskPauseRatio float64
}

// Agent owns the CRI client and the gRPC server.
type Agent struct {
	cfg Config
}

// New validates the configuration; it does not dial CRI yet.
func New(cfg Config) (*Agent, error) {
	if cfg.NodeName == "" {
		return nil, errors.New("agent: node name is required")
	}
	if cfg.CRISocket == "" {
		return nil, errors.New("agent: CRI socket path is required")
	}
	if cfg.ListenAddr == "" {
		return nil, errors.New("agent: gRPC listen address is required")
	}
	if cfg.DiskPauseRatio <= 0 || cfg.DiskPauseRatio >= 1 {
		return nil, fmt.Errorf("agent: disk pause ratio %v is outside (0,1)", cfg.DiskPauseRatio)
	}
	return &Agent{cfg: cfg}, nil
}

// Config returns a copy of the effective configuration.
func (a *Agent) Config() Config { return a.cfg }

// Run starts the CRI client and the gRPC server until ctx is canceled.
// Implemented in WS-A; the skeleton keeps the entrypoint honest.
func (a *Agent) Run(_ context.Context) error {
	return fmt.Errorf("agent %s: gRPC server: %w", a.cfg.NodeName, ErrNotImplemented)
}
