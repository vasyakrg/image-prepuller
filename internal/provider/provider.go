package provider

import (
	"context"

	"github.com/local/image-prepuller/internal/model"
)

// Provider abstracts cluster state and pull operations so MockProvider and K8sProvider
// share the exact same HTTP API, Web UI, and Prometheus metrics layer.
type Provider interface {
	Snapshot(ctx context.Context) model.Snapshot
	TriggerPull(ctx context.Context, image, node, actor string) error
	TriggerWorkloadPull(ctx context.Context, namespace, kind, name, actor string) (int, error)
	SyncNow(ctx context.Context, actor string) error
	PullCounters() []model.PullMetricSample
}
