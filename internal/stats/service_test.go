package stats

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// fakeQuerier is the read side used by the service tests: every method records
// the call and returns canned data. The writer side (Apply/Prune/Close) comes
// from fakeStore in runner_test.go; fakeReadStore embeds both.
type fakeQuerier struct {
	pingErr       error
	overview      model.StatsOverview
	overviewQuery *model.StatsOverviewQuery
	nodesCluster  string
	imagesQuery   *model.StatsImageQuery
	tasksQuery    *model.StatsTaskQuery
	pingCalls     int
	overviewCall  int
}

func (f *fakeQuerier) Ping(context.Context) error {
	f.pingCalls++
	return f.pingErr
}

func (f *fakeQuerier) Overview(_ context.Context, q model.StatsOverviewQuery) (model.StatsOverview, error) {
	f.overviewCall++
	f.overviewQuery = &q
	return f.overview, nil
}

func (f *fakeQuerier) Nodes(_ context.Context, cluster string) ([]model.StatsNodeSample, error) {
	f.nodesCluster = cluster
	return []model.StatsNodeSample{}, nil
}

func (f *fakeQuerier) Images(_ context.Context, q model.StatsImageQuery) (model.StatsImagePage, error) {
	f.imagesQuery = &q
	return model.StatsImagePage{Rows: []model.StatsImage{}}, nil
}

func (f *fakeQuerier) GCEvents(context.Context, model.StatsEventQuery) (model.StatsGCPage, error) {
	return model.StatsGCPage{Rows: []model.StatsGCEvent{}}, nil
}

func (f *fakeQuerier) Repulls(context.Context, model.StatsEventQuery) (model.StatsRepullPage, error) {
	return model.StatsRepullPage{Rows: []model.StatsRepull{}}, nil
}

func (f *fakeQuerier) Tasks(_ context.Context, q model.StatsTaskQuery) (model.StatsTaskPage, error) {
	f.tasksQuery = &q
	return model.StatsTaskPage{Rows: []model.StatsTask{}}, nil
}

// fakeReadStore is a Store that can also answer reads, like postgresStore.
type fakeReadStore struct {
	*fakeStore
	*fakeQuerier
}

func newFakeReadStore() *fakeReadStore {
	return &fakeReadStore{fakeStore: &fakeStore{}, fakeQuerier: &fakeQuerier{}}
}

func testClusters() []Cluster {
	return []Cluster{{ID: "talos-hub"}, {ID: "talos-kz"}}
}

// TestServiceDisabled is the "administrator disabled statistics" state: no DSN
// means Enabled=false, the info is honest and every read reports unavailable.
func TestServiceDisabled(t *testing.T) {
	svc := NewService("", Config{}, testClusters())
	if svc.Enabled() {
		t.Fatal("service with an empty DSN reports enabled")
	}
	info := svc.Info(context.Background())
	if info.Enabled || info.OK || info.Error != "" {
		t.Fatalf("disabled info = %+v", info)
	}
	if info.Clusters == nil {
		t.Fatal("disabled info clusters = nil, want []")
	}
	if _, err := svc.Overview(context.Background(), model.StatsOverviewQuery{Window: time.Hour}); !errors.Is(err, ErrStatsUnavailable) {
		t.Fatalf("overview without a store = %v, want ErrStatsUnavailable", err)
	}
}

// TestServiceUnavailableBeforeConnect covers a configured pipeline that has not
// connected yet: enabled, not ok, friendly error instead of a panic.
func TestServiceUnavailableBeforeConnect(t *testing.T) {
	svc := NewService("postgres://example/db", Config{}, testClusters())
	if !svc.Enabled() {
		t.Fatal("service with a DSN reports disabled")
	}
	info := svc.Info(context.Background())
	if !info.Enabled || info.OK {
		t.Fatalf("info before connect = %+v", info)
	}
	if info.Error == "" {
		t.Fatalf("info before connect has no error: %+v", info)
	}
	if len(info.Clusters) != 2 || info.Clusters[0] != "talos-hub" {
		t.Fatalf("info clusters = %v", info.Clusters)
	}
}

// TestServiceInfoPingsDatabase checks the ok/unavailable split after connect and
// that the read methods are delegated to the attached querier.
func TestServiceInfoPingsDatabase(t *testing.T) {
	store := newFakeReadStore()
	svc := NewService("postgres://example/db", Config{Interval: 30 * time.Second, Retention: 90 * 24 * time.Hour}, testClusters())
	svc.attach(store)
	t.Cleanup(svc.detach)

	info := svc.Info(context.Background())
	if !info.Enabled || !info.OK || info.Error != "" {
		t.Fatalf("connected info = %+v", info)
	}
	if store.pingCalls != 1 {
		t.Fatalf("ping calls = %d, want 1", store.pingCalls)
	}
	if info.IntervalSeconds != 30 || info.RetentionSeconds != int64((90*24*time.Hour)/time.Second) {
		t.Fatalf("info config = %+v", info)
	}
	if info.ConnectedAt == nil {
		t.Fatal("connected info has no connectedAt")
	}

	if _, err := svc.Overview(context.Background(), model.StatsOverviewQuery{Window: 24 * time.Hour, Cluster: "talos-kz"}); err != nil {
		t.Fatalf("overview: %v", err)
	}
	if store.overviewCall != 1 {
		t.Fatalf("overview calls = %d, want 1", store.overviewCall)
	}
	if store.overviewQuery == nil || store.overviewQuery.Window != 24*time.Hour || store.overviewQuery.Cluster != "talos-kz" {
		t.Fatalf("overview query = %+v, want the window and cluster passed through", store.overviewQuery)
	}
	if _, err := svc.Nodes(context.Background(), "talos-hub"); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if store.nodesCluster != "talos-hub" {
		t.Fatalf("nodes cluster = %q, want talos-hub", store.nodesCluster)
	}

	query := model.StatsImageQuery{Window: time.Hour, State: "open", Limit: 10}
	if _, err := svc.Images(context.Background(), query); err != nil {
		t.Fatalf("images: %v", err)
	}
	if store.imagesQuery == nil || store.imagesQuery.State != "open" {
		t.Fatalf("images query not delegated: %+v", store.imagesQuery)
	}

	store.pingErr = errors.New("connection refused")
	info = svc.Info(context.Background())
	if !info.Enabled || info.OK || info.Error == "" {
		t.Fatalf("info with failing ping = %+v", info)
	}
}

// TestServiceRunAttachesStore drives the real retry loop with a fake opener: it
// must attach the store, log the configuration and disconnect when ctx is done.
func TestServiceRunAttachesStore(t *testing.T) {
	store := newFakeReadStore()
	original := openStore
	openStore = func(context.Context, string) (Store, error) { return store, nil }
	t.Cleanup(func() { openStore = original })

	svc := NewService("postgres://example/db", Config{Interval: time.Hour}, testClusters())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx, nil)
		close(done)
	}()

	waitFor(t, "the statistics store to be attached", func() bool {
		info := svc.Info(context.Background())
		return info.OK
	})
	cancelAndWait(t, cancel, done)
	if q, _, err := svc.reader(); err == nil || q != nil {
		t.Fatal("service kept the store attached after shutdown")
	}
}
