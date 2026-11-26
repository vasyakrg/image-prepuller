package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

// fakeProvider counts Snapshot invocations so tests can assert that one scrape
// costs exactly one snapshot per cluster.
type fakeProvider struct {
	snap      model.Snapshot
	counters  []model.PullMetricSample
	snapCalls int32
}

func (f *fakeProvider) Snapshot(context.Context) model.Snapshot {
	atomic.AddInt32(&f.snapCalls, 1)
	return f.snap
}

func (f *fakeProvider) TriggerPull(context.Context, model.PullRequest) (model.PullTask, error) {
	return model.PullTask{}, provider.ErrNotImplemented
}

func (f *fakeProvider) TriggerWorkloadPull(context.Context, model.WorkloadPullRequest) (int, error) {
	return 0, provider.ErrNotImplemented
}

func (f *fakeProvider) CancelPull(context.Context, string) error {
	return provider.ErrNotImplemented
}

func (f *fakeProvider) SyncNow(context.Context, string) error {
	return provider.ErrNotImplemented
}

func (f *fakeProvider) Health(context.Context) model.Health {
	return model.Health{Online: true, CheckedAt: time.Now()}
}

func (f *fakeProvider) PullCounters() []model.PullMetricSample {
	return f.counters
}

func newTestRegistry(t *testing.T, clusters map[string]*fakeProvider) *provider.Registry {
	t.Helper()
	reg := provider.NewRegistry()
	for id, p := range clusters {
		reg.Register(provider.ClusterEntry{ID: id, Name: id, Role: "remote", Provider: p})
	}
	return reg
}

func scrape(t *testing.T, reg *provider.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics content-type = %q", got)
	}
	return rec.Body.String()
}

func fixedSnapshot() model.Snapshot {
	return model.Snapshot{
		ClusterName: "alpha",
		APIVersion:  model.APIVersion,
		LastSyncAt:  time.Unix(1700000000, 0).UTC(),
		SyncStatus:  model.SyncOK,
		Nodes: []model.NodeInfo{
			{
				Name:                 "worker-1",
				Ready:                true,
				AgentUp:              true,
				AgentVersion:         "v0.2.0",
				DiskPaused:           true,
				ImageFSUsedBytes:     100,
				ImageFSCapacityBytes: 1000,
				CachedImageCount:     2,
				CachedImages: []model.CachedImage{
					{Image: "ghcr.io/acme/api:1", SizeBytes: 10},
					{Image: "ghcr.io/acme/web:1", SizeBytes: 20},
				},
			},
			{
				Name:             "worker-2",
				Ready:            false,
				AgentUp:          false,
				ImageFSUsedBytes: 50,
				CachedImageCount: 3,
			},
		},
		Tasks: []model.PullTask{
			{ID: "t1", State: model.TaskQueued},
			{ID: "t2", State: model.TaskRunning},
			{ID: "t3", State: model.TaskSucceeded, DurationMs: 1000},
			{ID: "t4", State: model.TaskSucceeded, DurationMs: 4000},
			{ID: "t5", State: model.TaskFailed, DurationMs: 500},
			{ID: "t6", State: model.TaskCanceled},
		},
	}
}

func TestHandlerCallsSnapshotOncePerCluster(t *testing.T) {
	providers := map[string]*fakeProvider{
		"alpha": {snap: fixedSnapshot()},
		"beta":  {snap: fixedSnapshot()},
		"gamma": {snap: fixedSnapshot()},
	}
	reg := newTestRegistry(t, providers)
	scrape(t, reg)

	for id, p := range providers {
		if got := atomic.LoadInt32(&p.snapCalls); got != 1 {
			t.Errorf("cluster %q Snapshot calls = %d, want 1", id, got)
		}
	}
}

func TestNewSeriesContent(t *testing.T) {
	reg := newTestRegistry(t, map[string]*fakeProvider{"alpha": {snap: fixedSnapshot()}})
	body := scrape(t, reg)

	want := []string{
		// pull task states, fixed order and zero-valued states included.
		`prepuller_pull_tasks{cluster="alpha",state="queued"} 1`,
		`prepuller_pull_tasks{cluster="alpha",state="running"} 1`,
		`prepuller_pull_tasks{cluster="alpha",state="succeeded"} 2`,
		`prepuller_pull_tasks{cluster="alpha",state="failed"} 1`,
		`prepuller_pull_tasks{cluster="alpha",state="canceled"} 1`,
		// terminal-task duration average in seconds (2 successes: 1s + 4s).
		`prepuller_pull_task_duration_seconds{cluster="alpha",state="succeeded"} 2.5`,
		`prepuller_pull_task_duration_seconds{cluster="alpha",state="failed"} 0.5`,
		// node signals.
		`prepuller_node_disk_paused{cluster="alpha",node="worker-1"} 1`,
		`prepuller_node_ready{cluster="alpha",node="worker-1"} 1`,
		`prepuller_node_ready{cluster="alpha",node="worker-2"} 0`,
		`prepuller_node_imagefs_free_bytes{cluster="alpha",node="worker-1"} 900`,
		`prepuller_images_cached_total{cluster="alpha",node="worker-1"} 2`,
		// count-only fallback when CachedImages is not populated.
		`prepuller_images_cached_total{cluster="alpha",node="worker-2"} 3`,
		`prepuller_agent_info{cluster="alpha",node="worker-1",version="v0.2.0"} 1`,
		// cluster sync signals: real LastSyncAt, never time.Now().
		`prepuller_cluster_last_sync_timestamp_seconds{cluster="alpha"} 1700000000`,
		`prepuller_cluster_sync_partial{cluster="alpha"} 0`,
		`prepuller_cluster_up{cluster="alpha",role="remote"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("missing series %q", w)
		}
	}

	// Duration is only emitted for terminal states with a known duration.
	for _, unwanted := range []string{
		`prepuller_pull_task_duration_seconds{cluster="alpha",state="queued"}`,
		`prepuller_pull_task_duration_seconds{cluster="alpha",state="running"}`,
		`prepuller_pull_task_duration_seconds{cluster="alpha",state="canceled"}`,
		`prepuller_node_imagefs_free_bytes{cluster="alpha",node="worker-2"}`,
		`prepuller_agent_info{cluster="alpha",node="worker-2"}`,
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("unexpected series %q", unwanted)
		}
	}

	// build_info is always present and carries a version label set.
	if !strings.Contains(body, "# TYPE prepuller_build_info gauge") {
		t.Error("missing prepuller_build_info TYPE")
	}
	if !strings.Contains(body, `prepuller_build_info{version="`) ||
		!strings.Contains(body, `commit="`) ||
		!strings.Contains(body, `go_version="`) {
		t.Error("prepuller_build_info is missing version labels")
	}
}

func TestNewSeriesHaveHelpAndType(t *testing.T) {
	reg := newTestRegistry(t, map[string]*fakeProvider{"alpha": {snap: fixedSnapshot()}})
	body := scrape(t, reg)

	for _, name := range []string{
		"prepuller_pull_tasks",
		"prepuller_pull_task_duration_seconds",
		"prepuller_node_imagefs_free_bytes",
		"prepuller_node_ready",
		"prepuller_images_cached_total",
		"prepuller_agent_info",
		"prepuller_cluster_last_sync_timestamp_seconds",
		"prepuller_cluster_sync_partial",
		"prepuller_build_info",
		// pre-existing families keep their headers.
		"prepuller_cluster_up",
		"prepuller_image_present",
		"prepuller_workload_policy_violation",
		"prepuller_node_agent_up",
		"prepuller_node_disk_paused",
		"prepuller_node_imagefs_used_bytes",
		"prepuller_node_imagefs_capacity_bytes",
		"prepuller_pull_total",
	} {
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Errorf("missing HELP for %s", name)
		}
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("missing TYPE for %s", name)
		}
	}
}

func TestOutputHasNoBlankLinesAndOneTrailingNewline(t *testing.T) {
	reg := newTestRegistry(t, map[string]*fakeProvider{"alpha": {snap: fixedSnapshot()}})
	body := scrape(t, reg)

	if strings.Contains(body, "\n\n") {
		t.Error("metrics output contains blank lines between series")
	}
	if !strings.HasSuffix(body, "\n") {
		t.Error("metrics output does not end with a newline")
	}
	if strings.HasSuffix(body, "\n\n") {
		t.Error("metrics output ends with more than one newline")
	}
	if len(body) == 0 {
		t.Fatal("metrics output is empty")
	}
}

func TestOfflineClusterMetrics(t *testing.T) {
	// RemoteProvider returns an empty snapshot with SyncStatus=offline and
	// Partial=true when the spoke is unreachable; LastSyncAt may be zero.
	snap := model.Snapshot{
		ClusterName: "offline",
		SyncStatus:  model.SyncOffline,
		Partial:     true,
	}
	reg := newTestRegistry(t, map[string]*fakeProvider{"offline": {snap: snap}})
	body := scrape(t, reg)

	want := []string{
		`prepuller_cluster_up{cluster="offline",role="remote"} 0`,
		`prepuller_cluster_sync_partial{cluster="offline"} 1`,
		// Families are still documented and emitted; only unknown values are skipped.
		"# HELP prepuller_pull_tasks ",
		"# HELP prepuller_node_ready ",
		"# HELP prepuller_cluster_last_sync_timestamp_seconds ",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q in offline metrics", w)
		}
	}
	if strings.Contains(body, `prepuller_cluster_last_sync_timestamp_seconds{cluster="offline"}`) {
		t.Error("offline cluster must not report a last sync timestamp when LastSyncAt is zero")
	}
}

func TestOutputStableForMultipleClusters(t *testing.T) {
	providers := map[string]*fakeProvider{
		"alpha": {snap: fixedSnapshot()},
		"beta":  {snap: fixedSnapshot()},
		"gamma": {snap: fixedSnapshot()},
	}
	reg := newTestRegistry(t, providers)
	first := scrape(t, reg)
	second := scrape(t, reg)
	if first != second {
		t.Fatal("metrics output is not stable between scrapes")
	}

	// Clusters are emitted in Registry.AllIDs (sorted) order inside each family.
	positions := []int{
		strings.Index(first, `prepuller_pull_tasks{cluster="alpha",state="queued"}`),
		strings.Index(first, `prepuller_pull_tasks{cluster="beta",state="queued"}`),
		strings.Index(first, `prepuller_pull_tasks{cluster="gamma",state="queued"}`),
	}
	for i, p := range positions {
		if p < 0 {
			t.Fatalf("missing prepuller_pull_tasks for cluster #%d", i)
		}
		if i > 0 && p < positions[i-1] {
			t.Fatalf("cluster order is not stable: positions = %v", positions)
		}
	}
}
