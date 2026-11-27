package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

func testSpokeHubConfig() SpokeHubConfig {
	return SpokeHubConfig{
		SyncInterval:   20 * time.Millisecond,
		CommandWait:    150 * time.Millisecond,
		RedeliverAfter: 50 * time.Millisecond,
		CommandTimeout: 2 * time.Second,
	}
}

func newTestSpokeHub(t *testing.T) *SpokeHub {
	t.Helper()
	hub := NewSpokeHub(testSpokeHubConfig())
	if err := hub.AddCluster("talos-kz", "cluster-token"); err != nil {
		t.Fatalf("AddCluster: %v", err)
	}
	return hub
}

// spawnFakeSpoke polls the mailbox like a real spoke and answers with handle.
func spawnFakeSpoke(ctx context.Context, hub *SpokeHub, cluster string, handle func(SpokeCommand) SpokeCommandResult) *atomic.Int32 {
	var handled atomic.Int32
	go func() {
		for ctx.Err() == nil {
			commands, err := hub.PollCommands(ctx, cluster)
			if err != nil {
				return
			}
			for _, cmd := range commands {
				handled.Add(1)
				_ = hub.AcceptResult(cluster, cmd.ID, handle(cmd))
			}
		}
	}()
	return &handled
}

func TestSpokeHubAddClusterAndAuthorize(t *testing.T) {
	hub := NewSpokeHub(SpokeHubConfig{})
	if err := hub.AddCluster("talos-kz", "cluster-token"); err != nil {
		t.Fatalf("AddCluster: %v", err)
	}
	if err := hub.AddCluster("talos-kz", "other"); err == nil {
		t.Fatal("duplicate cluster accepted")
	}
	if err := hub.AddCluster("  ", "token"); err == nil {
		t.Fatal("empty cluster name accepted")
	}
	if err := hub.AddCluster("talos-eu", ""); err == nil {
		t.Fatal("empty token accepted: the hub must never authenticate an unregistered token")
	}
	if !hub.Authorize("talos-kz", "cluster-token") {
		t.Error("valid token rejected")
	}
	for _, tc := range []struct{ cluster, token string }{
		{"talos-kz", "wrong"},
		{"talos-kz", ""},
		{"talos-eu", "cluster-token"},
	} {
		if hub.Authorize(tc.cluster, tc.token) {
			t.Errorf("Authorize(%q, %q) = true, want false", tc.cluster, tc.token)
		}
	}
	if _, ok := hub.Token("talos-eu"); ok {
		t.Error("Token reported an unregistered cluster")
	}
	names := hub.Clusters()
	if len(names) != 1 || names[0] != "talos-kz" {
		t.Fatalf("Clusters() = %v", names)
	}
	if _, err := hub.Provider("talos-eu"); !errors.Is(err, ErrUnknownSpokeCluster) {
		t.Fatalf("Provider(unknown) error = %v, want ErrUnknownSpokeCluster", err)
	}
	if err := hub.AcceptState("talos-eu", SpokeState{}); !errors.Is(err, ErrUnknownSpokeCluster) {
		t.Fatalf("AcceptState(unknown) error = %v", err)
	}
	if err := hub.AcceptResult("talos-eu", "id", SpokeCommandResult{}); !errors.Is(err, ErrUnknownSpokeCluster) {
		t.Fatalf("AcceptResult(unknown) error = %v", err)
	}
	if _, err := hub.PollCommands(context.Background(), "talos-eu"); !errors.Is(err, ErrUnknownSpokeCluster) {
		t.Fatalf("PollCommands(unknown) error = %v", err)
	}
}

func TestSpokeHubProviderCarriesRemoteCallsOverTheMailbox(t *testing.T) {
	hub := newTestSpokeHub(t)
	prov, err := hub.Provider("talos-kz")
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handled := spawnFakeSpoke(ctx, hub, "talos-kz", func(cmd SpokeCommand) SpokeCommandResult {
		switch cmd.Path {
		case "/internal/v1/pull":
			return SpokeCommandResult{
				Status: http.StatusAccepted,
				Body:   json.RawMessage(`{"id":"task-1","image":"nginx:1.27","node":"worker-1","state":"running"}`),
			}
		case "/internal/v1/pulls/finished/cancel":
			return SpokeCommandResult{Status: http.StatusConflict, Body: json.RawMessage(`{"error":"already finished"}`)}
		case "/internal/v1/pulls/missing/cancel":
			return SpokeCommandResult{Status: http.StatusNotFound, Body: json.RawMessage(`{"error":"task not found"}`)}
		}
		return SpokeCommandResult{Status: http.StatusOK, Body: json.RawMessage(`{"ok":true}`)}
	})

	task, err := prov.TriggerPull(ctx, model.PullRequest{Image: "nginx:1.27", Node: "worker-1"})
	if err != nil {
		t.Fatalf("TriggerPull: %v", err)
	}
	if task.ID != "task-1" || task.Cluster != "talos-kz" {
		t.Fatalf("TriggerPull task = %+v", task)
	}
	if err := prov.CancelPull(ctx, "finished"); !errors.Is(err, ErrTaskNotActive) {
		t.Fatalf("CancelPull(finished) error = %v, want ErrTaskNotActive", err)
	}
	if err := prov.CancelPull(ctx, "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("CancelPull(missing) error = %v, want ErrTaskNotFound", err)
	}
	if err := prov.CancelPull(ctx, "running"); err != nil {
		t.Fatalf("CancelPull(running): %v", err)
	}
	if _, err := prov.TriggerWorkloadPull(ctx, model.WorkloadPullRequest{Namespace: "demo", Kind: "Deployment", Name: "web"}); err != nil {
		t.Fatalf("TriggerWorkloadPull: %v", err)
	}
	if err := prov.SyncNow(ctx, "alice"); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if handled.Load() < 6 {
		t.Fatalf("fake spoke handled %d commands, want at least 6", handled.Load())
	}

	// Without a pushed state every read is the offline answer.
	snap := prov.Snapshot(ctx)
	if snap.SyncStatus != model.SyncOffline || !snap.Partial {
		t.Fatalf("Snapshot without state = %+v, want offline/partial", snap)
	}
	if health := prov.Health(ctx); health.Online {
		t.Fatalf("Health without state = %+v, want offline", health)
	}
	if samples := prov.PullCounters(); samples != nil {
		t.Fatalf("PullCounters without state = %v, want nil", samples)
	}
}

func TestSpokeHubServesPushedStateAndExpiresIt(t *testing.T) {
	now := time.Now()
	hub := NewSpokeHub(SpokeHubConfig{
		SyncInterval: 20 * time.Millisecond,
		CommandWait:  time.Second,
		Now:          func() time.Time { return now },
	})
	if err := hub.AddCluster("talos-kz", "cluster-token"); err != nil {
		t.Fatalf("AddCluster: %v", err)
	}
	prov, err := hub.Provider("talos-kz")
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}

	snap := model.Snapshot{
		ClusterName: "talos-kz",
		APIVersion:  model.APIVersion,
		SyncStatus:  model.SyncOK,
		UpdatedAt:   now,
		LastSyncAt:  now,
		Summary:     model.Summary{TotalWorkers: 3, ReadyWorkers: 3, TotalWorkloads: 4},
	}
	state := SpokeState{
		APIVersion: model.APIVersion,
		SentAt:     now,
		Version:    model.APIVersion,
		Snapshot:   snap,
		Counters: SpokeCounters{
			PullTotal: 2,
			Samples:   []model.PullMetricSample{{Image: "a:1", Node: "worker-1", Result: "success", Count: 2}},
		},
	}
	if err := hub.AcceptState("talos-kz", state); err != nil {
		t.Fatalf("AcceptState: %v", err)
	}
	if got := prov.Snapshot(context.Background()); got.Summary.TotalWorkers != 3 {
		t.Fatalf("Snapshot after push = %+v", got.Summary)
	}
	if health := prov.Health(context.Background()); !health.Online || health.Version != model.APIVersion {
		t.Fatalf("Health after push = %+v", health)
	}
	if samples := prov.PullCounters(); len(samples) != 1 || samples[0].Count != 2 {
		t.Fatalf("PullCounters after push = %v", samples)
	}
	if age, ok := prov.SyncAge(); !ok || age != 0 {
		t.Fatalf("SyncAge = %v, %v", age, ok)
	}

	// One minute of silence: the hub must stop serving the stale snapshot.
	now = now.Add(time.Minute)
	if got := prov.Snapshot(context.Background()); got.SyncStatus != model.SyncOffline || !got.Partial {
		t.Fatalf("stale Snapshot = %+v, want offline/partial", got)
	}
	if health := prov.Health(context.Background()); health.Online {
		t.Fatalf("stale Health = %+v, want offline", health)
	}
	if samples := prov.PullCounters(); samples != nil {
		t.Fatalf("stale PullCounters = %v, want nil", samples)
	}

	// A spoke that identifies itself under another name is a misconfiguration.
	bad := state
	bad.ClusterName = "talos-eu"
	if err := hub.AcceptState("talos-kz", bad); err == nil {
		t.Fatal("state from a cluster with a different name accepted")
	}
}

func TestSpokeHubRedeliversUnacknowledgedCommands(t *testing.T) {
	hub := newTestSpokeHub(t)
	prov, err := hub.Provider("talos-kz")
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var deliveries atomic.Int32
	go func() {
		for ctx.Err() == nil {
			commands, err := hub.PollCommands(ctx, "talos-kz")
			if err != nil {
				return
			}
			for _, cmd := range commands {
				if deliveries.Add(1) < 2 {
					// Drop the first delivery: the hub must offer it again.
					continue
				}
				_ = hub.AcceptResult("talos-kz", cmd.ID, SpokeCommandResult{
					Status: http.StatusAccepted,
					Body:   json.RawMessage(`{"id":"task-1","state":"running"}`),
				})
			}
		}
	}()

	task, err := prov.TriggerPull(ctx, model.PullRequest{Image: "a:1", Node: "worker-1"})
	if err != nil {
		t.Fatalf("TriggerPull: %v", err)
	}
	if task.ID != "task-1" {
		t.Fatalf("task = %+v", task)
	}
	if got := deliveries.Load(); got < 2 {
		t.Fatalf("command delivered %d times, want redelivery", got)
	}
}

func TestSpokeHubDropsCancelledWaiter(t *testing.T) {
	hub := newTestSpokeHub(t)
	prov, err := hub.Provider("talos-kz")
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := prov.TriggerPull(ctx, model.PullRequest{Image: "a:1", Node: "worker-1"}); err == nil {
		t.Fatal("TriggerPull without a spoke succeeded")
	}

	// The abandoned command must not stay queued for the next poll.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer waitCancel()
	commands, _ := hub.PollCommands(waitCtx, "talos-kz")
	if len(commands) != 0 {
		t.Fatalf("abandoned command was left queued: %+v", commands)
	}
}

func TestSpokeHubAcceptsSpuriousResult(t *testing.T) {
	hub := newTestSpokeHub(t)
	if err := hub.AcceptResult("talos-kz", "never-issued", SpokeCommandResult{Status: http.StatusOK}); err != nil {
		t.Fatalf("AcceptResult(spurious) = %v, want no error", err)
	}
}
