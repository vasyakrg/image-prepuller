package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

func TestBackoffDelay(t *testing.T) {
	cfg := Config{
		BackoffBase:   10 * time.Second,
		BackoffMax:    45 * time.Second,
		BackoffFactor: 2,
	}
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 0, want: 10 * time.Second},
		{failures: 1, want: 10 * time.Second},
		{failures: 2, want: 20 * time.Second},
		{failures: 3, want: 40 * time.Second},
		{failures: 4, want: 45 * time.Second},
		{failures: 10, want: 45 * time.Second},
	}
	for _, tc := range cases {
		if got := cfg.backoffDelay(tc.failures); got != tc.want {
			t.Errorf("backoffDelay(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}

	constant := Config{BackoffBase: 5 * time.Second, BackoffMax: time.Minute, BackoffFactor: 1}
	if got := constant.backoffDelay(5); got != 5*time.Second {
		t.Errorf("constant factor delay = %v, want 5s", got)
	}
}

func TestBackoffOnTriggerErrors(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	cfg := Config{
		Now:           clock.Now,
		BackoffBase:   time.Minute,
		BackoffMax:    10 * time.Minute,
		BackoffFactor: 2,
	}
	snap := testSnapshot(
		[]model.NodeInfo{readyNode("n1")},
		[]model.Workload{testWorkload("prod", "api",
			testImage(testImageA, model.PullPolicyIfNotPresent, testSlot("n1", model.StateMissing)))},
	)
	fake := newFakeProvider(snap)
	fake.triggerErr[key(testImageA, "n1")] = errors.New("registry 401 unauthorized")

	rec := New(cfg)
	run := func() Result {
		t.Helper()
		res, err := rec.RunOnce(context.Background(), fake, "test")
		if err != nil {
			t.Fatalf("RunOnce() error = %v", err)
		}
		return res
	}

	res := run()
	if len(res.Planned) != 1 || len(res.Triggered) != 0 || len(res.Failures) != 1 {
		t.Fatalf("first run: planned=%d triggered=%d failures=%d, want 1/0/1",
			len(res.Planned), len(res.Triggered), len(res.Failures))
	}
	if got := rec.backoff.failures(key(testImageA, "n1")); got != 1 {
		t.Fatalf("failure count = %d, want 1", got)
	}

	// Inside the first backoff window the pair must not be retried.
	clock.Advance(30 * time.Second)
	res = run()
	if len(res.Planned) != 0 {
		t.Fatalf("planned during backoff = %d, want 0", len(res.Planned))
	}
	if res.Skipped[SkipBackoff] != 1 {
		t.Fatalf("backoff skips = %d, want 1 (all: %v)", res.Skipped[SkipBackoff], res.Skipped)
	}
	if got := len(fake.triggers()); got != 1 {
		t.Fatalf("trigger calls during backoff = %d, want 1", got)
	}

	// The window elapses: one retry, and the backoff doubles.
	clock.Advance(30 * time.Second) // t0 + 60s
	res = run()
	if len(res.Planned) != 1 || len(res.Failures) != 1 {
		t.Fatalf("retry run: planned=%d failures=%d, want 1/1", len(res.Planned), len(res.Failures))
	}
	if got := rec.backoff.failures(key(testImageA, "n1")); got != 2 {
		t.Fatalf("failure count after retry = %d, want 2", got)
	}
	if got := len(fake.triggers()); got != 2 {
		t.Fatalf("trigger calls after retry = %d, want 2", got)
	}

	// Doubled window: next attempt is at t0+60s+2m = t0+180s.
	clock.Advance(time.Minute) // t0 + 120s, still blocked
	if res = run(); len(res.Planned) != 0 {
		t.Fatalf("planned inside doubled window = %d, want 0", len(res.Planned))
	}
	clock.Advance(time.Minute) // t0 + 180s, window over
	if res = run(); len(res.Planned) != 1 {
		t.Fatalf("planned after doubled window = %d, want 1", len(res.Planned))
	}
	if got := len(fake.triggers()); got != 3 {
		t.Fatalf("trigger calls = %d, want 3", got)
	}
}

func TestBackoffFromObservedFailure(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	cfg := Config{Now: clock.Now, BackoffBase: time.Minute, BackoffMax: 10 * time.Minute}
	failedSlot := testSlot("n1", model.StateFailed)
	snap := testSnapshot(
		[]model.NodeInfo{readyNode("n1")},
		[]model.Workload{testWorkload("prod", "api",
			testImage(testImageA, model.PullPolicyIfNotPresent, failedSlot))},
	)
	fake := newFakeProvider(snap)
	rec := New(cfg)
	run := func() Result {
		t.Helper()
		res, err := rec.RunOnce(context.Background(), fake, "test")
		if err != nil {
			t.Fatalf("RunOnce() error = %v", err)
		}
		return res
	}

	// The pull is accepted; the failure happens asynchronously later.
	if res := run(); len(res.Triggered) != 1 || len(res.Failures) != 0 {
		t.Fatalf("first run triggered=%d failures=%d, want 1/0", len(res.Triggered), len(res.Failures))
	}

	// The provider now reports the pair as failed, after our attempt.
	failedSlot.LastChecked = clock.Now().Add(time.Second)
	snap.Workloads[0].Images[0].Nodes[0] = failedSlot
	fake.setSnapshot(snap)

	clock.Advance(time.Second)
	res := run()
	if len(res.Planned) != 0 {
		t.Fatalf("planned while observed failure backs off = %d, want 0", len(res.Planned))
	}
	if res.Skipped[SkipBackoff] != 1 {
		t.Fatalf("backoff skips = %d, want 1 (all: %v)", res.Skipped[SkipBackoff], res.Skipped)
	}
	if got := rec.backoff.failures(key(testImageA, "n1")); got != 1 {
		t.Fatalf("failure count from observed failure = %d, want 1", got)
	}
	if got := len(fake.triggers()); got != 1 {
		t.Fatalf("trigger calls = %d, want 1", got)
	}

	// After the window (t0+1s + 1m) the pair is attempted again.
	clock.Advance(time.Minute)
	if res = run(); len(res.Planned) != 1 {
		t.Fatalf("planned after window = %d, want 1", len(res.Planned))
	}
	if got := len(fake.triggers()); got != 2 {
		t.Fatalf("trigger calls after window = %d, want 2", got)
	}
}

func TestBackoffIgnoresStaleFailedObservation(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	cfg := Config{Now: clock.Now, BackoffBase: time.Minute, BackoffMax: 10 * time.Minute}
	failedSlot := testSlot("n1", model.StateFailed)
	snap := testSnapshot(
		[]model.NodeInfo{readyNode("n1")},
		[]model.Workload{testWorkload("prod", "api",
			testImage(testImageA, model.PullPolicyIfNotPresent, failedSlot))},
	)
	fake := newFakeProvider(snap)
	rec := New(cfg)
	if res, err := rec.RunOnce(context.Background(), fake, "test"); err != nil || len(res.Triggered) != 1 {
		t.Fatalf("first run: triggered=%d err=%v", len(res.Triggered), err)
	}

	// The failed state predates our attempt: it must not consume the backoff
	// window, otherwise a stale snapshot could block a healthy node for long.
	failedSlot.LastChecked = clock.Now().Add(-time.Minute)
	snap.Workloads[0].Images[0].Nodes[0] = failedSlot
	fake.setSnapshot(snap)

	clock.Advance(time.Second)
	res, err := rec.RunOnce(context.Background(), fake, "test")
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(res.Planned) != 1 {
		t.Fatalf("planned with stale failure = %d, want 1", len(res.Planned))
	}
	if got := rec.backoff.failures(key(testImageA, "n1")); got != 0 {
		t.Fatalf("failure count = %d, want 0", got)
	}
	if got := len(fake.triggers()); got != 2 {
		t.Fatalf("trigger calls = %d, want 2", got)
	}
}

func TestBackoffResetOnPresent(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	cfg := Config{Now: clock.Now, BackoffBase: time.Minute, BackoffMax: 10 * time.Minute}
	snap := testSnapshot(
		[]model.NodeInfo{readyNode("n1")},
		[]model.Workload{testWorkload("prod", "api",
			testImage(testImageA, model.PullPolicyIfNotPresent, testSlot("n1", model.StateMissing)))},
	)
	fake := newFakeProvider(snap)
	fake.triggerErr[key(testImageA, "n1")] = errors.New("registry timeout")
	rec := New(cfg)
	if res, err := rec.RunOnce(context.Background(), fake, "test"); err != nil || len(res.Failures) != 1 {
		t.Fatalf("first run: failures=%d err=%v", len(res.Failures), err)
	}
	if len(rec.backoff.entries) != 1 {
		t.Fatalf("backoff entries = %d, want 1", len(rec.backoff.entries))
	}

	// The image shows up warm: entry is reset and the pair is simply "present".
	snap.Workloads[0].Images[0].Nodes[0] = testSlot("n1", model.StatePresent)
	fake.setSnapshot(snap)
	clock.Advance(time.Second)
	res, err := rec.RunOnce(context.Background(), fake, "test")
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if len(rec.backoff.entries) != 0 {
		t.Fatalf("backoff entries after present = %d, want 0", len(rec.backoff.entries))
	}
	if res.Skipped[SkipPresent] != 1 {
		t.Fatalf("present skips = %d, want 1 (all: %v)", res.Skipped[SkipPresent], res.Skipped)
	}
}

func TestBackoffResetOnSucceededTask(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	cfg := Config{Now: clock.Now, BackoffBase: time.Minute, BackoffMax: 10 * time.Minute}
	snap := testSnapshot(
		[]model.NodeInfo{readyNode("n1")},
		[]model.Workload{testWorkload("prod", "api",
			testImage(testImageA, model.PullPolicyIfNotPresent, testSlot("n1", model.StateMissing)))},
	)
	fake := newFakeProvider(snap)
	fake.triggerErr[key(testImageA, "n1")] = errors.New("registry timeout")
	rec := New(cfg)
	if _, err := rec.RunOnce(context.Background(), fake, "test"); err != nil {
		t.Fatalf("first run error = %v", err)
	}

	snap.Tasks = []model.PullTask{{
		Image: testImageA, Node: "n1", State: model.TaskSucceeded,
	}}
	fake.setSnapshot(snap)
	delete(fake.triggerErr, key(testImageA, "n1"))
	clock.Advance(time.Second)
	res, err := rec.RunOnce(context.Background(), fake, "test")
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if got := rec.backoff.failures(key(testImageA, "n1")); got != 0 {
		t.Fatalf("failure count after succeeded task = %d, want 0", got)
	}
	// The failed trigger is forgotten, so the slot is planned again.
	if len(res.Planned) != 1 {
		t.Fatalf("planned after succeeded task = %d, want 1", len(res.Planned))
	}
}

func TestBackoffPrune(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	cfg := Config{BackoffBase: time.Minute, BackoffMax: 10 * time.Minute}
	b := newBackoffState()
	b.recordFailure("expired", now.Add(-time.Hour), cfg, "old")
	b.recordFailure("active", now, cfg, "new")
	b.recordAttempt("warm", now.Add(-time.Hour))

	b.prune(now, cfg.BackoffMax)
	if _, ok := b.entries["expired"]; ok {
		t.Error("expired backoff entry was not pruned")
	}
	if _, ok := b.entries["active"]; !ok {
		t.Error("active backoff entry was pruned")
	}
	if _, ok := b.entries["warm"]; ok {
		t.Error("idle attempt entry was not pruned")
	}
}
