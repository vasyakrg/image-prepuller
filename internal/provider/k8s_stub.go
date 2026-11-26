package provider

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/local/image-prepuller/internal/model"
)

// K8sProvider is the live Kubernetes backend skeleton (WS-B). It owns the
// client-go clientset and will implement Snapshot/TriggerPull/CancelPull on top
// of list+watch and node-agent responses. Until WS-B lands, every live
// operation reports ErrNotImplemented so the hub fails loudly instead of
// silently showing empty clusters.
//
// Dependency boundary (ADR-001): client-go is allowed only here and in
// internal/provider/k8s*.go, never in internal/web, internal/model or
// internal/agent.
type K8sProvider struct {
	clientset kubernetes.Interface
	clusterID string
}

// NewK8sProvider builds the live provider from a kubeconfig or in-cluster
// rest.Config. It validates the clientset wiring in the contract PR.
func NewK8sProvider(clusterID string, cfg *rest.Config) (*K8sProvider, error) {
	if cfg == nil {
		return nil, fmt.Errorf("k8s provider: nil rest config")
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s provider: build clientset: %w", err)
	}
	return &K8sProvider{clientset: clientset, clusterID: clusterID}, nil
}

// Clientset exposes the clientset to the reconciler (WS-C) and tests.
func (p *K8sProvider) Clientset() kubernetes.Interface { return p.clientset }

func (p *K8sProvider) Snapshot(_ context.Context) model.Snapshot {
	return model.Snapshot{
		ClusterName: p.clusterID,
		APIVersion:  model.APIVersion,
		SyncStatus:  model.SyncUnknown,
		Partial:     true,
	}
}

func (p *K8sProvider) TriggerPull(_ context.Context, _ model.PullRequest) (model.PullTask, error) {
	return model.PullTask{}, fmt.Errorf("k8s provider: TriggerPull: %w", ErrNotImplemented)
}

func (p *K8sProvider) TriggerWorkloadPull(_ context.Context, _ model.WorkloadPullRequest) (int, error) {
	return 0, fmt.Errorf("k8s provider: TriggerWorkloadPull: %w", ErrNotImplemented)
}

func (p *K8sProvider) CancelPull(_ context.Context, _ string) error {
	return fmt.Errorf("k8s provider: CancelPull: %w", ErrNotImplemented)
}

func (p *K8sProvider) SyncNow(_ context.Context, _ string) error {
	return fmt.Errorf("k8s provider: SyncNow: %w", ErrNotImplemented)
}

func (p *K8sProvider) Health(_ context.Context) model.Health {
	return model.Health{Online: false, Error: ErrNotImplemented.Error()}
}

func (p *K8sProvider) PullCounters() []model.PullMetricSample { return nil }
