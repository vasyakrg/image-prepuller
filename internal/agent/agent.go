// Package agent is the per-node prepuller daemon: it talks to the container
// runtime over CRI, inventories cached images and serves them to the hub over a
// small HTTP/JSON API. Stage 2 workstream WS-A (docs/PLAN.md section 5).
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// Version is reported to the hub in Snapshot.AgentVersion and in the
// prepuller_agent_build_info metric. It follows the release line
// (Chart.yaml appVersion): main carries the version of the upcoming release.
const Version = "1.1.0"

// Pull timing defaults. They are the answer to "a pull into a dead registry
// must not hold a slot for ten minutes": the registry probe fails a black-holed
// endpoint in seconds, and the stall watchdog cancels a pull that stops making
// progress after a minute. A legitimately slow pull keeps the full PullTimeout.
const (
	// DefaultPullTimeout bounds one whole CRI pull.
	DefaultPullTimeout = 10 * time.Minute
	// DefaultPullStallTimeout is the no-progress window of the stall watchdog.
	DefaultPullStallTimeout = time.Minute
	// stallPollInterval is the ImageFS sampling period of the watchdog; shorter
	// stall windows shrink it proportionally (see Server.pullImage).
	stallPollInterval = 5 * time.Second
)

// Config is the node-agent configuration derived from flags and environment.
type Config struct {
	NodeName       string
	CRISocket      string
	ListenAddr     string
	DiskPauseRatio float64
	// MetricsAddr is the separate, unauthenticated listen address for the
	// Prometheus exposition. Empty disables the metrics server.
	MetricsAddr string
	// Mock serves an in-memory CRI backend; CRISocket is then ignored.
	Mock bool
	// Token enables Bearer authentication on the agent API. Empty disables it
	// for local development; full auth/RBAC lands in WS-F.
	Token string

	// PullTimeout bounds one CRI pull; zero means DefaultPullTimeout.
	PullTimeout time.Duration
	// RegistryProbeTimeout bounds the preflight TCP dial to the image registry;
	// zero means DefaultRegistryProbeTimeout.
	RegistryProbeTimeout time.Duration
	// PullStallTimeout is the window without image filesystem growth after
	// which a running pull is cancelled as stalled; zero means
	// DefaultPullStallTimeout.
	PullStallTimeout time.Duration
	// DisableRegistryProbe turns the preflight dial off. Needed when containerd
	// pulls through a registry mirror: the mirror is reachable while the
	// upstream host named in the image reference is not.
	DisableRegistryProbe bool
	// DisablePullStallWatch turns the no-progress watchdog off, for example when
	// the runtime keeps its content store on a filesystem the agent cannot see.
	DisablePullStallWatch bool
}

// withDefaults fills the pull timing fields with their documented defaults.
func (c Config) withDefaults() Config {
	if c.PullTimeout <= 0 {
		c.PullTimeout = DefaultPullTimeout
	}
	if c.RegistryProbeTimeout <= 0 {
		c.RegistryProbeTimeout = DefaultRegistryProbeTimeout
	}
	if c.PullStallTimeout <= 0 {
		c.PullStallTimeout = DefaultPullStallTimeout
	}
	return c
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
	if cfg.PullTimeout < 0 || cfg.RegistryProbeTimeout < 0 || cfg.PullStallTimeout < 0 {
		return nil, errors.New("agent: pull timings must not be negative")
	}
	return &Agent{cfg: cfg.withDefaults()}, nil
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
	metricsAddr := a.cfg.MetricsAddr
	if metricsAddr == "" {
		metricsAddr = "disabled"
	}
	log.Printf("agent %s: backend=%s listen=%s metrics=%s api=%s auth=%v",
		a.cfg.NodeName, backend, a.cfg.ListenAddr, metricsAddr, WireAPIVersion, a.cfg.Token != "")
	return srv.Serve(ctx)
}
