package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/local/image-prepuller/internal/model"
)

type ClusterEntry struct {
	ID       string
	Name     string
	Role     string // "local" | "remote"
	Endpoint string
	Provider Provider
}

type ClusterSummary struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Role            string  `json:"role"`
	Endpoint        string  `json:"endpoint,omitempty"`
	Online          bool    `json:"online"`
	ReadyWorkers    int     `json:"readyWorkers"`
	TotalWorkers    int     `json:"totalWorkers"`
	TotalWorkloads  int     `json:"totalWorkloads"`
	CoveragePercent float64 `json:"coveragePercent"`
	PolicyWarnings  int     `json:"policyWarnings"`
	SyncStatus      string  `json:"syncStatus"`
}

type Registry struct {
	mu       sync.RWMutex
	clusters map[string]ClusterEntry
	order    []string
}

func NewRegistry() *Registry {
	return &Registry{
		clusters: make(map[string]ClusterEntry),
	}
}

func (r *Registry) Register(entry ClusterEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.clusters[entry.ID]; !exists {
		r.order = append(r.order, entry.ID)
	}
	r.clusters[entry.ID] = entry
}

func (r *Registry) DefaultID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) == 0 {
		return ""
	}
	return r.order[0]
}

func (r *Registry) Get(id string) (ClusterEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if id == "" && len(r.order) > 0 {
		id = r.order[0]
	}
	e, ok := r.clusters[id]
	return e, ok
}

func (r *Registry) List(ctx context.Context) []ClusterSummary {
	r.mu.RLock()
	entries := make([]ClusterEntry, 0, len(r.order))
	for _, id := range r.order {
		entries = append(entries, r.clusters[id])
	}
	r.mu.RUnlock()

	out := make([]ClusterSummary, 0, len(entries))
	for _, e := range entries {
		snap := e.Provider.Snapshot(ctx)
		online := snap.Summary.TotalWorkers == 0 || snap.Summary.ReadyWorkers > 0
		out = append(out, ClusterSummary{
			ID:              e.ID,
			Name:            e.Name,
			Role:            e.Role,
			Endpoint:        e.Endpoint,
			Online:          online,
			ReadyWorkers:    snap.Summary.ReadyWorkers,
			TotalWorkers:    snap.Summary.TotalWorkers,
			TotalWorkloads:  snap.Summary.TotalWorkloads,
			CoveragePercent: snap.Summary.CoveragePercent,
			PolicyWarnings:  snap.Summary.PolicyWarnings,
			SyncStatus:      string(snap.SyncStatus),
		})
	}
	return out
}

func (r *Registry) AllSnapshots(ctx context.Context) map[string]model.Snapshot {
	r.mu.RLock()
	entries := make([]ClusterEntry, 0, len(r.order))
	for _, id := range r.order {
		entries = append(entries, r.clusters[id])
	}
	r.mu.RUnlock()

	res := make(map[string]model.Snapshot, len(entries))
	for _, e := range entries {
		res[e.ID] = e.Provider.Snapshot(ctx)
	}
	return res
}

func (r *Registry) AllIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := append([]string(nil), r.order...)
	sort.Strings(ids)
	return ids
}

func (r *Registry) TriggerPull(ctx context.Context, clusterID string, req model.PullRequest) (model.PullTask, error) {
	e, ok := r.Get(clusterID)
	if !ok {
		return model.PullTask{}, fmt.Errorf("unknown cluster %q", clusterID)
	}
	req.Cluster = e.ID
	return e.Provider.TriggerPull(ctx, req)
}

func (r *Registry) TriggerWorkloadPull(ctx context.Context, clusterID string, req model.WorkloadPullRequest) (int, error) {
	e, ok := r.Get(clusterID)
	if !ok {
		return 0, fmt.Errorf("unknown cluster %q", clusterID)
	}
	req.Cluster = e.ID
	return e.Provider.TriggerWorkloadPull(ctx, req)
}

func (r *Registry) CancelPull(ctx context.Context, clusterID, taskID string) error {
	e, ok := r.Get(clusterID)
	if !ok {
		return fmt.Errorf("unknown cluster %q", clusterID)
	}
	return e.Provider.CancelPull(ctx, taskID)
}

func (r *Registry) SyncNow(ctx context.Context, clusterID, actor string) error {
	e, ok := r.Get(clusterID)
	if !ok {
		return fmt.Errorf("unknown cluster %q", clusterID)
	}
	return e.Provider.SyncNow(ctx, actor)
}

// Health returns the provider health view for one cluster ("" means default).
func (r *Registry) Health(ctx context.Context, clusterID string) (model.Health, error) {
	e, ok := r.Get(clusterID)
	if !ok {
		return model.Health{}, fmt.Errorf("unknown cluster %q", clusterID)
	}
	return e.Provider.Health(ctx), nil
}
