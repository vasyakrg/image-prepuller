package provider

import (
	"context"
	"errors"

	"github.com/local/image-prepuller/internal/model"
)

var (
	// ErrNotImplemented marks live backends that are still under construction
	// (WS-A node agent, WS-B Kubernetes provider).
	ErrNotImplemented = errors.New("not implemented")
	// ErrTaskNotFound is returned by CancelPull for unknown task ids.
	ErrTaskNotFound = errors.New("pull task not found")
)

// Provider abstracts cluster state and pull operations so MockProvider,
// RemoteProvider and K8sProvider share the exact same HTTP API, Web UI and
// Prometheus metrics layer.
//
// Contract v2 (see docs/PLAN.md section 10): adding a method means updating
// MockProvider, RemoteProvider, the K8sProvider stub, the registry and tests in
// the same change.
type Provider interface {
	Snapshot(ctx context.Context) model.Snapshot
	// TriggerPull starts one asynchronous pull and returns its task. It is
	// idempotent: an already running pull for the same image+node returns the
	// existing task without an error.
	TriggerPull(ctx context.Context, req model.PullRequest) (model.PullTask, error)
	TriggerWorkloadPull(ctx context.Context, req model.WorkloadPullRequest) (int, error)
	// CancelPull cancels a queued or running task. Finished tasks are not
	// cancellable and report an error.
	CancelPull(ctx context.Context, taskID string) error
	SyncNow(ctx context.Context, actor string) error
	Health(ctx context.Context) model.Health
	PullCounters() []model.PullMetricSample
}
