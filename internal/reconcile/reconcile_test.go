package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

func singleMissingSnapshot() model.Snapshot {
	return testSnapshot(
		[]model.NodeInfo{readyNode("n1")},
		[]model.Workload{testWorkload("prod", "api",
			testImage(testImageA, model.PullPolicyIfNotPresent, testSlot("n1", model.StateMissing)))},
	)
}

func TestRunOnceRequestShape(t *testing.T) {
	fake := newFakeProvider(singleMissingSnapshot())
	rec := New(Config{Actor: "auto-bot"})

	res, err := rec.RunOnce(context.Background(), fake, "cluster-x")
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if res.Cluster != "cluster-x" {
		t.Fatalf("result cluster = %q, want cluster-x", res.Cluster)
	}
	reqs := fake.triggers()
	if len(reqs) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Cluster != "cluster-x" || req.Image != testImageA || req.Node != "n1" {
		t.Fatalf("request = %+v, want cluster-x/%s/n1", req, testImageA)
	}
	if req.Trigger != model.TriggerAuto {
		t.Fatalf("trigger = %q, want auto", req.Trigger)
	}
	if req.Actor != "auto-bot" {
		t.Fatalf("actor = %q, want auto-bot", req.Actor)
	}
	if req.Policy != model.PullPolicyIfNotPresent {
		t.Fatalf("policy = %q, want IfNotPresent", req.Policy)
	}
	syncs := fake.syncs()
	if len(syncs) != 1 || syncs[0] != "auto-bot" {
		t.Fatalf("sync actors = %v, want [auto-bot]", syncs)
	}
	if len(res.Triggered) != 1 || res.Triggered[0].Actor != "auto-bot" {
		t.Fatalf("triggered = %+v, want one task with actor auto-bot", res.Triggered)
	}
}

func TestRunOnceFallsBackToSnapshotClusterName(t *testing.T) {
	fake := newFakeProvider(singleMissingSnapshot())
	rec := New(Config{})
	res, err := rec.RunOnce(context.Background(), fake, "")
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if res.Cluster != "test-cluster" {
		t.Fatalf("result cluster = %q, want test-cluster", res.Cluster)
	}
	if got := fake.triggers(); len(got) != 1 || got[0].Cluster != "test-cluster" {
		t.Fatalf("request cluster = %+v, want test-cluster", got)
	}
}

func TestRunOnceSyncErrorDoesNotStopTheRun(t *testing.T) {
	fake := newFakeProvider(singleMissingSnapshot())
	fake.syncErr = errors.New("spoke unreachable")
	rec := New(Config{})

	res, err := rec.RunOnce(context.Background(), fake, "test")
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if res.SyncError != "spoke unreachable" {
		t.Fatalf("sync error = %q, want spoke unreachable", res.SyncError)
	}
	if len(res.Triggered) != 1 {
		t.Fatalf("triggered = %d, want 1", len(res.Triggered))
	}
}

func TestRunOnceDisableSyncBeforeSnapshot(t *testing.T) {
	fake := newFakeProvider(singleMissingSnapshot())
	rec := New(Config{DisableSyncBeforeSnapshot: true})
	if _, err := rec.RunOnce(context.Background(), fake, "test"); err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got := fake.syncs(); len(got) != 0 {
		t.Fatalf("sync calls = %d, want 0", len(got))
	}
}

func TestRunOnceNilProvider(t *testing.T) {
	rec := New(Config{})
	if _, err := rec.RunOnce(context.Background(), nil, "test"); err == nil {
		t.Fatal("RunOnce(nil) error = nil, want error")
	}
}

func TestRunOnceCanceledContext(t *testing.T) {
	fake := newFakeProvider(singleMissingSnapshot())
	rec := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := rec.RunOnce(ctx, fake, "test")
	if err == nil {
		t.Fatal("RunOnce(canceled) error = nil, want error")
	}
	if l := len(res.Triggered); l != 0 {
		t.Fatalf("triggered after cancel = %d, want 0", l)
	}
	if got := fake.snapshotCount(); got != 0 {
		t.Fatalf("snapshot calls after cancel = %d, want 0", got)
	}
}

func TestResultHelpers(t *testing.T) {
	res := Result{Skipped: map[SkipReason]int{SkipPresent: 2, SkipOfflineNode: 1}}
	if got := res.SkippedTotal(); got != 3 {
		t.Fatalf("SkippedTotal() = %d, want 3", got)
	}
	if got := (Result{}).SkippedTotal(); got != 0 {
		t.Fatalf("SkippedTotal(empty) = %d, want 0", got)
	}
}

// TestRunOnceWithMockProvider is a smoke test against the contract MockProvider
// (the real Provider v2 implementation from contract-v2), proving the policy
// works without a cluster and without a hand-rolled provider interface.
func TestRunOnceWithMockProvider(t *testing.T) {
	p := provider.NewMockProvider("smoke")
	rec := New(Config{})

	res, err := rec.RunOnce(context.Background(), p, "smoke")
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(res.Planned) == 0 {
		t.Fatal("planned pulls = 0, want at least one (demo cluster has a failed slot)")
	}
	if len(res.Triggered) != len(res.Planned) {
		t.Fatalf("triggered = %d, want %d", len(res.Triggered), len(res.Planned))
	}
	if len(res.Planned) > 4 {
		t.Fatalf("planned = %d, want <= maxConcurrentTotal (4)", len(res.Planned))
	}

	perNode := make(map[string]int)
	seen := make(map[string]struct{})
	for _, req := range res.Planned {
		if req.Trigger != model.TriggerAuto || req.Actor != "reconciler" || req.Cluster != "smoke" {
			t.Errorf("request %+v: want trigger=auto actor=reconciler cluster=smoke", req)
		}
		k := key(req.Image, req.Node)
		if _, dup := seen[k]; dup {
			t.Errorf("duplicate planned request for %s", k)
		}
		seen[k] = struct{}{}
		perNode[req.Node]++
		if perNode[req.Node] > 2 {
			t.Errorf("node %s has %d planned pulls, want <= 2", req.Node, perNode[req.Node])
		}
		if req.Node == "talos-worker-03" || req.Node == "talos-worker-04" {
			t.Errorf("planned pull on offline/disk-paused demo node %s", req.Node)
		}
		if strings.Contains(req.Image, ":latest") {
			t.Errorf("planned pull for policy-violating image %s", req.Image)
		}
	}
	if res.Skipped[SkipPresent] == 0 {
		t.Error("present skips = 0, want > 0 for the demo cluster")
	}
	if res.Skipped[SkipOfflineNode] == 0 {
		t.Error("offline skips = 0, want > 0 (talos-worker-04 is NotReady)")
	}
	if res.Skipped[SkipDiskPaused] == 0 {
		t.Error("disk skips = 0, want > 0 (talos-worker-03 is at 80%)")
	}
	// Policy violations are skipped before planning, but in the demo cluster the
	// only :latest image is already present everywhere it could be pulled, so the
	// policy rule itself is covered by the table-driven tests above.

	// A second immediate run must not re-plan a pair that already has an
	// active task or a pulling slot.
	res2, err := rec.RunOnce(context.Background(), p, "smoke")
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	for _, req := range res2.Planned {
		if _, dup := seen[key(req.Image, req.Node)]; dup {
			t.Errorf("second run re-planned %s while it is already in flight", key(req.Image, req.Node))
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := New(Config{}).Config()
	if cfg.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want 30s", cfg.Interval)
	}
	if cfg.MaxConcurrentPerNode != 2 || cfg.MaxConcurrentTotal != 4 {
		t.Errorf("limits = %d/%d, want 2/4", cfg.MaxConcurrentPerNode, cfg.MaxConcurrentTotal)
	}
	if cfg.DiskPauseThreshold != 0.78 {
		t.Errorf("DiskPauseThreshold = %v, want 0.78", cfg.DiskPauseThreshold)
	}
	if cfg.BackoffBase != 30*time.Second || cfg.BackoffMax != 15*time.Minute || cfg.BackoffFactor != 2 {
		t.Errorf("backoff defaults = %v/%v/%v", cfg.BackoffBase, cfg.BackoffMax, cfg.BackoffFactor)
	}
	if cfg.Actor != "reconciler" {
		t.Errorf("Actor = %q, want reconciler", cfg.Actor)
	}
	if len(cfg.NamespacePriority) != 3 || cfg.NamespacePriority[0] != "prod" {
		t.Errorf("NamespacePriority = %v", cfg.NamespacePriority)
	}
	if cfg.Now == nil {
		t.Error("Now is nil")
	}
	if cfg.AllowPolicyViolations {
		t.Error("AllowPolicyViolations must default to false")
	}
}

func TestStartRunsImmediately(t *testing.T) {
	fake := newFakeProvider(singleMissingSnapshot())
	rec := New(Config{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan Result, 1)
	done := make(chan struct{})
	go func() {
		rec.Start(ctx, fake, "test", func(res Result) { results <- res })
		close(done)
	}()

	select {
	case res := <-results:
		if len(res.Triggered) != 1 {
			t.Fatalf("triggered = %d, want 1", len(res.Triggered))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not run immediately")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

func TestStartTicksAndStops(t *testing.T) {
	fake := newFakeProvider(singleMissingSnapshot())
	rec := New(Config{Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan Result, 64)
	done := make(chan struct{})
	go func() {
		rec.Start(ctx, fake, "test", func(res Result) { results <- res })
		close(done)
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-results:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for run %d", i+1)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
	if got := fake.snapshotCount(); got < 3 {
		t.Fatalf("snapshot calls = %d, want >= 3", got)
	}
}

func TestStartStopsWhenContextAlreadyCanceled(t *testing.T) {
	fake := newFakeProvider(singleMissingSnapshot())
	rec := New(Config{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		rec.Start(ctx, fake, "test", func(Result) {
			t.Error("onResult called for a canceled context")
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return for an already canceled context")
	}
	if got := fake.snapshotCount(); got != 0 {
		t.Fatalf("snapshot calls = %d, want 0", got)
	}
}
