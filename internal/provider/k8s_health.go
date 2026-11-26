package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// Health pings the apiserver version endpoint with a bounded timeout. It reports
// the control-plane view only: node agents are reflected in Snapshot.Nodes
// (AgentUp) instead, because one dead agent must not mark the cluster offline.
func (p *K8sProvider) Health(ctx context.Context) model.Health {
	start := p.now()

	checkCtx, cancel := context.WithTimeout(ctx, p.healthTimeout)
	defer cancel()

	type probe struct {
		version string
		err     error
	}
	result := make(chan probe, 1) // buffered: a timed-out probe must not leak a blocked goroutine
	go func() {
		version, err := p.versionFn(checkCtx)
		result <- probe{version: version, err: err}
	}()

	select {
	case got := <-result:
		health := model.Health{
			Online:    got.err == nil,
			Version:   got.version,
			CheckedAt: p.now(),
			LatencyMs: p.now().Sub(start).Milliseconds(),
		}
		if got.err != nil {
			health.Error = got.err.Error()
		}
		return health
	case <-checkCtx.Done():
		return model.Health{
			Online:    false,
			Error:     fmt.Sprintf("apiserver health check timed out after %s: %v", p.healthTimeout, checkCtx.Err()),
			CheckedAt: p.now(),
			LatencyMs: time.Since(start).Milliseconds(),
		}
	}
}

// apiServerVersion is the production probe behind Health.
func (p *K8sProvider) apiServerVersion(context.Context) (string, error) {
	info, err := p.clientset.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("discovery server version: %w", err)
	}
	if info == nil {
		return "", nil
	}
	return info.GitVersion, nil
}
