package reconcile

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

// fakeProvider is a fully in-memory Provider v2. The reconciler must work with
// any provider, so tests use this instead of MockProvider to control snapshot
// contents, trigger failures and call counts precisely.
type fakeProvider struct {
	mu sync.Mutex

	snapshot model.Snapshot
	syncErr  error
	// triggerErr maps image|node to the error TriggerPull must return.
	triggerErr map[string]error
	// triggerFn, when set, overrides the default successful task creation.
	triggerFn func(model.PullRequest) (model.PullTask, error)

	syncActors    []string
	triggerReqs   []model.PullRequest
	snapshotCalls int
	taskSeq       int
}

var _ provider.Provider = (*fakeProvider)(nil)

func newFakeProvider(snap model.Snapshot) *fakeProvider {
	return &fakeProvider{snapshot: snap, triggerErr: make(map[string]error)}
}

func (f *fakeProvider) Snapshot(context.Context) model.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotCalls++
	return f.snapshot
}

func (f *fakeProvider) setSnapshot(snap model.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot = snap
}

func (f *fakeProvider) TriggerPull(_ context.Context, req model.PullRequest) (model.PullTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggerReqs = append(f.triggerReqs, req)
	if err := f.triggerErr[key(req.Image, req.Node)]; err != nil {
		return model.PullTask{}, err
	}
	if f.triggerFn != nil {
		return f.triggerFn(req)
	}
	f.taskSeq++
	return model.PullTask{
		ID:        fmt.Sprintf("task-%d", f.taskSeq),
		Cluster:   req.Cluster,
		Image:     req.Image,
		Node:      req.Node,
		State:     model.TaskRunning,
		Trigger:   model.TriggerAuto,
		Actor:     req.Actor,
		CreatedAt: time.Now(),
	}, nil
}

func (f *fakeProvider) TriggerWorkloadPull(context.Context, model.WorkloadPullRequest) (int, error) {
	return 0, nil
}

func (f *fakeProvider) CancelPull(context.Context, string) error { return nil }

func (f *fakeProvider) SyncNow(_ context.Context, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncActors = append(f.syncActors, actor)
	return f.syncErr
}

func (f *fakeProvider) Health(context.Context) model.Health {
	return model.Health{Online: true}
}

func (f *fakeProvider) PullCounters() []model.PullMetricSample { return nil }

func (f *fakeProvider) snapshotCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotCalls
}

func (f *fakeProvider) triggers() []model.PullRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.PullRequest(nil), f.triggerReqs...)
}

func (f *fakeProvider) syncs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.syncActors...)
}

// fakeClock is a manually advanced clock for deterministic backoff tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// --- fixture builders -------------------------------------------------------

const (
	testImageA = "ghcr.io/acme/api:1.0"
	testImageB = "ghcr.io/acme/worker:2.0"
	testImageC = "ghcr.io/acme/sidecar:3.0"
)

// readyNode returns a Ready node with a healthy agent and low ImageFS usage.
func readyNode(name string) model.NodeInfo {
	return model.NodeInfo{
		Name:                 name,
		Ready:                true,
		AgentUp:              true,
		DiskPauseThreshold:   0.78,
		ImageFSUsedBytes:     10,
		ImageFSCapacityBytes: 100,
	}
}

func testSlot(node string, state model.PullState) model.ImageNodeStatus {
	return model.ImageNodeStatus{Node: node, State: state}
}

func testImage(image string, policy model.PullPolicy, slots ...model.ImageNodeStatus) model.WorkloadImage {
	return model.WorkloadImage{
		Image:         image,
		Ref:           model.ParseImageRef(image),
		ContainerName: "main",
		PullPolicy:    policy,
		Nodes:         slots,
	}
}

func testWorkload(namespace, name string, images ...model.WorkloadImage) model.Workload {
	return model.Workload{
		Namespace: namespace,
		Kind:      "Deployment",
		Name:      name,
		Replicas:  1,
		Images:    images,
	}
}

func testSnapshot(nodes []model.NodeInfo, workloads []model.Workload) model.Snapshot {
	return model.Snapshot{
		ClusterName: "test-cluster",
		APIVersion:  model.APIVersion,
		SyncStatus:  model.SyncOK,
		Nodes:       nodes,
		Workloads:   workloads,
	}
}

// plannedPairs renders Result.Planned as image|node strings for compact asserts.
func plannedPairs(res Result) []string {
	out := make([]string, 0, len(res.Planned))
	for _, req := range res.Planned {
		out = append(out, key(req.Image, req.Node))
	}
	return out
}
