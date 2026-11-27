package stats

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

type fakeStore struct {
	mu         sync.Mutex
	batches    []Batch
	prunes     []time.Time
	closeCalls int
	applyCalls int
	failOn     int // 1-based Apply call that returns an error
	applyErr   error
	alwaysFail bool
}

func (s *fakeStore) Apply(_ context.Context, batch Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyCalls++
	s.batches = append(s.batches, batch)
	if s.alwaysFail || (s.failOn > 0 && s.applyCalls == s.failOn) {
		if s.applyErr != nil {
			return s.applyErr
		}
		return fmt.Errorf("fake apply failure")
	}
	return nil
}

func (s *fakeStore) Prune(_ context.Context, horizon time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunes = append(s.prunes, horizon)
	return nil
}

func (s *fakeStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return nil
}

func (s *fakeStore) stats() (applyCalls, closeCalls int, batches []Batch, prunes []time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyCalls, s.closeCalls, append([]Batch(nil), s.batches...), append([]time.Time(nil), s.prunes...)
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func cancelAndWait(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestRunRetriesFailedBatchWithoutCommit(t *testing.T) {
	present := baseTime
	absent := baseTime.Add(time.Minute)
	snapshots := []model.Snapshot{
		{UpdatedAt: present, Nodes: []model.NodeInfo{{Name: "n1", AgentUp: true, CachedImages: []model.CachedImage{{Image: "nginx:1", SizeBytes: 10}}}}},
		{UpdatedAt: absent, Nodes: []model.NodeInfo{{Name: "n1", AgentUp: true}}},
		{UpdatedAt: absent, Nodes: []model.NodeInfo{{Name: "n1", AgentUp: true}}},
	}
	var calls int32
	snapshot := func(context.Context) model.Snapshot {
		i := int(atomic.AddInt32(&calls, 1)) - 1
		if i >= len(snapshots) {
			i = len(snapshots) - 1
		}
		return snapshots[i]
	}

	store := &fakeStore{failOn: 2}
	var logs []string
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Interval: 5 * time.Millisecond, PruneInterval: time.Hour}, []Cluster{{ID: "c1", Snapshot: snapshot}}, store, logf)
		close(done)
	}()

	waitFor(t, "three Apply calls", func() bool {
		applyCalls, _, _, _ := store.stats()
		return applyCalls >= 3
	})
	cancelAndWait(t, cancel, done)

	applyCalls, closeCalls, batches, _ := store.stats()
	if applyCalls < 3 {
		t.Fatalf("apply calls = %d, want >= 3", applyCalls)
	}
	if closeCalls != 1 {
		t.Fatalf("store Close calls = %d, want 1", closeCalls)
	}
	// The failed Apply must not have committed the diff: the next batch repeats
	// the same removal instead of treating the image as already gone.
	if len(batches[1].PresenceRemoved) != 1 {
		t.Fatalf("second batch has no removal: %+v", batches[1].PresenceRemoved)
	}
	if len(batches[2].PresenceRemoved) != 1 {
		t.Fatalf("retry batch lost the removal: %+v", batches[2].PresenceRemoved)
	}
	if batches[2].PresenceRemoved[0].Image != "nginx:1" {
		t.Fatalf("retry removed wrong image: %+v", batches[2].PresenceRemoved[0])
	}
	logMu.Lock()
	logged := strings.Join(logs, "\n")
	logMu.Unlock()
	if !strings.Contains(logged, "apply") {
		t.Fatalf("apply failure was not logged: %q", logged)
	}
}

func TestRunPruneCadence(t *testing.T) {
	store := &fakeStore{}
	cluster := Cluster{ID: "c1", Snapshot: func(context.Context) model.Snapshot {
		return model.Snapshot{UpdatedAt: baseTime}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Interval: time.Hour, PruneInterval: 5 * time.Millisecond, Retention: time.Hour}, []Cluster{cluster}, store, nil)
		close(done)
	}()

	waitFor(t, "two prune calls", func() bool {
		_, _, _, prunes := store.stats()
		return len(prunes) >= 2
	})
	cancelAndWait(t, cancel, done)

	_, closeCalls, _, prunes := store.stats()
	if closeCalls != 1 {
		t.Fatalf("store Close calls = %d, want 1", closeCalls)
	}
	for _, horizon := range prunes {
		if time.Since(horizon) < 30*time.Minute {
			t.Fatalf("prune horizon %v is not now-retention", horizon)
		}
	}
}

func TestRunContextCancel(t *testing.T) {
	store := &fakeStore{}
	cluster := Cluster{ID: "c1", Snapshot: func(context.Context) model.Snapshot {
		return model.Snapshot{UpdatedAt: baseTime}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Interval: time.Hour, PruneInterval: time.Hour}, []Cluster{cluster}, store, nil)
		close(done)
	}()
	cancelAndWait(t, cancel, done)
	if _, closeCalls, _, _ := store.stats(); closeCalls != 1 {
		t.Fatalf("store Close calls = %d, want 1", closeCalls)
	}
}

func TestRunDBDownAtStartKeepsRunning(t *testing.T) {
	store := &fakeStore{alwaysFail: true, applyErr: fmt.Errorf("database is down")}
	var snapshots int32
	cluster := Cluster{ID: "c1", Snapshot: func(context.Context) model.Snapshot {
		atomic.AddInt32(&snapshots, 1)
		return model.Snapshot{UpdatedAt: baseTime}
	}}
	var logs []string
	var logMu sync.Mutex
	logf := func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Interval: 5 * time.Millisecond, PruneInterval: time.Hour}, []Cluster{cluster}, store, logf)
		close(done)
	}()

	waitFor(t, "retries while database is down", func() bool {
		applyCalls, _, _, _ := store.stats()
		return applyCalls >= 3
	})
	cancelAndWait(t, cancel, done)

	applyCalls, closeCalls, _, _ := store.stats()
	if applyCalls < 3 {
		t.Fatalf("apply calls = %d, want >= 3", applyCalls)
	}
	if closeCalls != 1 {
		t.Fatalf("store Close calls = %d, want 1", closeCalls)
	}
	if atomic.LoadInt32(&snapshots) < 3 {
		t.Fatalf("snapshot calls = %d, want >= 3", snapshots)
	}
	logMu.Lock()
	logged := strings.Join(logs, "\n")
	logMu.Unlock()
	if !strings.Contains(logged, "database is down") {
		t.Fatalf("database failure was not logged: %q", logged)
	}
}

func TestRunNilStoreDoesNotPanic(t *testing.T) {
	done := make(chan struct{})
	go func() {
		Run(context.Background(), Config{Interval: time.Millisecond}, nil, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with nil store did not return")
	}
}

func TestRunSnapshotTimeout(t *testing.T) {
	store := &fakeStore{}
	var snapshots int32
	cluster := Cluster{ID: "c1", Snapshot: func(ctx context.Context) model.Snapshot {
		atomic.AddInt32(&snapshots, 1)
		<-ctx.Done()
		return model.Snapshot{}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Interval: 5 * time.Millisecond, SnapshotTimeout: 5 * time.Millisecond, PruneInterval: time.Hour}, []Cluster{cluster}, store, nil)
		close(done)
	}()
	waitFor(t, "timed out snapshots", func() bool {
		return atomic.LoadInt32(&snapshots) >= 2
	})
	cancelAndWait(t, cancel, done)
}

func TestRunOneSnapshotPerClusterPerTick(t *testing.T) {
	store := &fakeStore{}
	var callsA, callsB int32
	clusterA := Cluster{ID: "a", Snapshot: func(context.Context) model.Snapshot {
		atomic.AddInt32(&callsA, 1)
		return model.Snapshot{UpdatedAt: baseTime}
	}}
	clusterB := Cluster{ID: "b", Snapshot: func(context.Context) model.Snapshot {
		atomic.AddInt32(&callsB, 1)
		return model.Snapshot{UpdatedAt: baseTime}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Interval: 5 * time.Millisecond, PruneInterval: time.Hour}, []Cluster{clusterA, clusterB}, store, nil)
		close(done)
	}()
	waitFor(t, "two ticks per cluster", func() bool {
		return atomic.LoadInt32(&callsA) >= 2 && atomic.LoadInt32(&callsB) >= 2
	})
	cancelAndWait(t, cancel, done)
	a, b := atomic.LoadInt32(&callsA), atomic.LoadInt32(&callsB)
	if a != b {
		t.Fatalf("snapshot calls differ between clusters: %d vs %d", a, b)
	}
	applyCalls, _, batches, _ := store.stats()
	if applyCalls != int(a)*2 {
		t.Fatalf("apply calls = %d, want %d (two clusters per tick)", applyCalls, a*2)
	}
	for _, batch := range batches {
		if batch.Cluster != "a" && batch.Cluster != "b" {
			t.Fatalf("unexpected batch cluster %q", batch.Cluster)
		}
	}
}

func TestRunSnapshotTimeoutIgnoresProviderContext(t *testing.T) {
	release := make(chan struct{})
	var snapshots int32
	cluster := Cluster{ID: "blocked", Snapshot: func(context.Context) model.Snapshot {
		atomic.AddInt32(&snapshots, 1)
		<-release
		return model.Snapshot{}
	}}
	store := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Interval: 5 * time.Millisecond, SnapshotTimeout: 5 * time.Millisecond, PruneInterval: time.Hour}, []Cluster{cluster}, store, nil)
		close(done)
	}()
	waitFor(t, "snapshot timeout retries even when provider ignores context", func() bool {
		return atomic.LoadInt32(&snapshots) >= 2
	})
	cancelAndWait(t, cancel, done)
	close(release)
}

func TestRunZeroRetentionSkipsPrune(t *testing.T) {
	store := &fakeStore{}
	cluster := Cluster{ID: "c", Snapshot: func(context.Context) model.Snapshot {
		return model.Snapshot{UpdatedAt: baseTime}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Interval: 5 * time.Millisecond, PruneInterval: 5 * time.Millisecond}, []Cluster{cluster}, store, nil)
		close(done)
	}()
	waitFor(t, "a few ticks", func() bool {
		applyCalls, _, _, _ := store.stats()
		return applyCalls >= 3
	})
	cancelAndWait(t, cancel, done)
	_, _, _, prunes := store.stats()
	if len(prunes) != 0 {
		t.Fatalf("prune ran without retention: %d calls", len(prunes))
	}
}
