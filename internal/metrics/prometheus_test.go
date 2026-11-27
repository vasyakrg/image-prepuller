package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// newRegistry registers arbitrary providers (the error-counter capability is
// optional, so tests need providers that are not plain *fakeProvider).
func newRegistry(t *testing.T, clusters map[string]provider.Provider) *provider.Registry {
	t.Helper()
	reg := provider.NewRegistry()
	for id, p := range clusters {
		reg.Register(provider.ClusterEntry{ID: id, Name: id, Role: "remote", Provider: p})
	}
	return reg
}

// errorCountingProvider is a fakeProvider that also implements the optional
// provider.AgentErrorCounter capability, like K8sProvider does.
type errorCountingProvider struct {
	*fakeProvider
	errors map[provider.AgentErrorKey]uint64
}

func (p *errorCountingProvider) AgentErrors() map[provider.AgentErrorKey]uint64 {
	out := make(map[provider.AgentErrorKey]uint64, len(p.errors))
	for key, count := range p.errors {
		out[key] = count
	}
	return out
}

// blockingProvider models a provider stuck in a network call: Snapshot ignores
// the context until the test releases it. calls counts invocations before the
// block, so the one-call-per-scrape invariant is still observable.
type blockingProvider struct {
	*fakeProvider
	block chan struct{}
	gate  atomic.Bool
	calls atomic.Int32
}

func (p *blockingProvider) Snapshot(ctx context.Context) model.Snapshot {
	p.calls.Add(1)
	if p.gate.Load() {
		<-p.block
	}
	return p.fakeProvider.Snapshot(ctx)
}

// seriesValue returns the numeric value of the first sample whose line starts
// with prefix.
func seriesValue(t *testing.T, body, prefix string) (float64, bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("malformed sample %q", line)
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("parse sample %q: %v", line, err)
		}
		return value, true
	}
	return 0, false
}

func scrape(t *testing.T, reg *provider.Registry) string {
	t.Helper()
	return scrapeHandler(t, Handler(reg))
}

// scrapeHandler serves one scrape through an existing handler. The collector is
// stateful, so partition tests must reuse the handler created once at startup.
func scrapeHandler(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
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
				LastSeen:             time.Unix(1700000500, 0).UTC(),
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
		"prepuller_node_last_seen_timestamp_seconds",
		"prepuller_node_agent_errors_total",
		"prepuller_snapshot_age_seconds",
		"prepuller_snapshot_scrape_timeout_total",
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
	if strings.Contains(body, `prepuller_snapshot_age_seconds{cluster="offline"}`) {
		t.Error("cluster that never synced must not report a snapshot age")
	}
	if strings.Contains(body, `prepuller_node_last_seen_timestamp_seconds{cluster="offline"}`) {
		t.Error("cluster without agent timestamps must not report last_seen series")
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
	// prepuller_snapshot_age_seconds legitimately grows between two scrapes, so
	// it is excluded from the byte-for-byte comparison; everything else must be
	// stable, including the family/series order.
	first = withoutSnapshotAge(first)
	second = withoutSnapshotAge(second)
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

// withoutSnapshotAge drops the one family whose value grows with wall-clock time.
func withoutSnapshotAge(body string) string {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "prepuller_snapshot_age_seconds{") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func TestNodeLastSeenSeries(t *testing.T) {
	reg := newTestRegistry(t, map[string]*fakeProvider{"alpha": {snap: fixedSnapshot()}})
	body := scrape(t, reg)

	want := `prepuller_node_last_seen_timestamp_seconds{cluster="alpha",node="worker-1"} 1700000500`
	if !strings.Contains(body, want) {
		t.Fatalf("missing series %q", want)
	}
	// worker-2 has no LastSeen: a zero timestamp must be omitted, not emitted as 0.
	if strings.Contains(body, `prepuller_node_last_seen_timestamp_seconds{cluster="alpha",node="worker-2"}`) {
		t.Fatal("zero LastSeen must be omitted")
	}
	if !strings.Contains(body, "# TYPE prepuller_node_last_seen_timestamp_seconds gauge") {
		t.Fatal("missing TYPE for prepuller_node_last_seen_timestamp_seconds")
	}
}

func TestNodeAgentErrorsSeries(t *testing.T) {
	counting := &errorCountingProvider{
		fakeProvider: &fakeProvider{snap: fixedSnapshot()},
		errors: map[provider.AgentErrorKey]uint64{
			{Node: "worker-1", Reason: provider.ReasonUnavailable}:  3,
			{Node: "worker-1", Reason: provider.ReasonUnauthorized}: 1,
			{Node: "worker-2", Reason: provider.ReasonTimeout}:      2,
		},
	}
	reg := newRegistry(t, map[string]provider.Provider{"alpha": counting})
	body := scrape(t, reg)

	// Only observed reasons are emitted, in the documented stable order.
	want := []string{
		`prepuller_node_agent_errors_total{cluster="alpha",node="worker-1",reason="unauthorized"} 1`,
		`prepuller_node_agent_errors_total{cluster="alpha",node="worker-1",reason="unavailable"} 3`,
		`prepuller_node_agent_errors_total{cluster="alpha",node="worker-2",reason="timeout"} 2`,
	}
	positions := make([]int, len(want))
	for i, series := range want {
		positions[i] = strings.Index(body, series)
		if positions[i] < 0 {
			t.Fatalf("missing series %q", series)
		}
	}
	// Stable reason order: unauthorized, protocol, timeout, unavailable.
	if positions[0] > positions[1] {
		t.Fatalf("reason order is not stable: %v", positions)
	}
	for _, unwanted := range []string{
		`reason="protocol"`,
	} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("unexpected sample %q: reasons that were never observed must be omitted", unwanted)
		}
	}
}

func TestNodeAgentErrorsAbsentForProvidersWithoutCapability(t *testing.T) {
	// MockProvider/RemoteProvider do not implement AgentErrorCounter: the family
	// header is present (self-documenting) but no samples are emitted.
	reg := newTestRegistry(t, map[string]*fakeProvider{"alpha": {snap: fixedSnapshot()}})
	body := scrape(t, reg)

	if !strings.Contains(body, "# TYPE prepuller_node_agent_errors_total counter") {
		t.Fatal("missing TYPE for prepuller_node_agent_errors_total")
	}
	if strings.Contains(body, "prepuller_node_agent_errors_total{") {
		t.Fatal("provider without AgentErrorCounter must not emit agent-error samples")
	}
}

func TestSnapshotAgeSeries(t *testing.T) {
	snap := fixedSnapshot()
	snap.LastSyncAt = time.Now().Add(-90 * time.Second)
	reg := newTestRegistry(t, map[string]*fakeProvider{"alpha": {snap: snap}})
	body := scrape(t, reg)

	age, ok := seriesValue(t, body, `prepuller_snapshot_age_seconds{cluster="alpha"}`)
	if !ok {
		t.Fatal("missing prepuller_snapshot_age_seconds for a synced cluster")
	}
	if age < 88 || age > 95 {
		t.Fatalf("snapshot age = %v, want about 90s", age)
	}
}

func TestSnapshotScrapeTimeoutServesCachedSnapshot(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	alpha := &blockingProvider{fakeProvider: &fakeProvider{snap: fixedSnapshot()}, block: block}
	// never blocks from the very first scrape, so the collector has no snapshot
	// to fall back to for it.
	never := &blockingProvider{fakeProvider: &fakeProvider{snap: fixedSnapshot()}, block: block}
	never.gate.Store(true)

	reg := newRegistry(t, map[string]provider.Provider{"alpha": alpha, "never": never})
	h := Handler(reg)

	// First scrape: alpha answers, never burns the deadline.
	first := scrapeHandler(t, h)
	for _, want := range []string{
		`prepuller_cluster_up{cluster="alpha",role="remote"} 1`,
		`prepuller_node_ready{cluster="alpha",node="worker-1"} 1`,
		`prepuller_snapshot_scrape_timeout_total{cluster="alpha"} 0`,
		`prepuller_snapshot_scrape_timeout_total{cluster="never"} 1`,
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("first scrape: missing %q", want)
		}
	}
	// A cluster that never answered gets no data series at all: zeroes would
	// render a blind hub as healthy.
	for _, unwanted := range []string{
		`prepuller_cluster_up{cluster="never"`,
		`prepuller_node_ready{cluster="never"`,
		`prepuller_snapshot_age_seconds{cluster="never"}`,
	} {
		if strings.Contains(first, unwanted) {
			t.Fatalf("first scrape: unexpected series %q for a never-snapshotted cluster", unwanted)
		}
	}

	// Second scrape: the partition now hits alpha too. The scrape stays bounded
	// by snapshotDeadline and serves alpha's last successful snapshot.
	alpha.gate.Store(true)
	started := time.Now()
	second := scrapeHandler(t, h)
	elapsed := time.Since(started)
	if elapsed < snapshotDeadline-500*time.Millisecond {
		t.Fatalf("scrape returned after %v, want it to wait for the %v deadline", elapsed, snapshotDeadline)
	}
	if elapsed > snapshotDeadline+4*time.Second {
		t.Fatalf("scrape took %v, want it bounded by snapshotDeadline=%v", elapsed, snapshotDeadline)
	}
	t.Logf("scrape under a dead link returned in %v", elapsed)

	for _, want := range []string{
		// Cached previous series, including the last sync timestamp.
		`prepuller_cluster_up{cluster="alpha",role="remote"} 1`,
		`prepuller_node_ready{cluster="alpha",node="worker-1"} 1`,
		`prepuller_node_last_seen_timestamp_seconds{cluster="alpha",node="worker-1"} 1700000500`,
		`prepuller_cluster_last_sync_timestamp_seconds{cluster="alpha"} 1700000000`,
		`prepuller_snapshot_scrape_timeout_total{cluster="alpha"} 1`,
		`prepuller_snapshot_scrape_timeout_total{cluster="never"} 2`,
	} {
		if !strings.Contains(second, want) {
			t.Fatalf("second scrape: missing %q", want)
		}
	}
	if age, ok := seriesValue(t, second, `prepuller_snapshot_age_seconds{cluster="alpha"}`); !ok || age <= 0 {
		t.Fatalf("second scrape: stale snapshot must still report a rising age, got %v (present=%v)", age, ok)
	}
	if strings.Contains(second, `prepuller_cluster_up{cluster="never"`) {
		t.Fatal("second scrape: a cluster that never synced must not produce placeholder series")
	}

	// Exactly one Snapshot call per cluster per scrape, blocked ones included.
	if got := alpha.calls.Load(); got != 2 {
		t.Fatalf("alpha Snapshot calls = %d, want 2 (one per scrape)", got)
	}
	if got := never.calls.Load(); got != 2 {
		t.Fatalf("never Snapshot calls = %d, want 2 (one per scrape)", got)
	}
}
