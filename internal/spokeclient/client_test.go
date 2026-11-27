package spokeclient_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
	"github.com/local/image-prepuller/internal/spokeclient"
	"github.com/local/image-prepuller/internal/web"
)

const (
	e2eCluster = "talos-kz"
	e2eToken   = "cluster-token"
)

// TestOutboundSpokeEndToEnd wires the two halves of the outbound channel: a hub
// HTTP server with the SpokeHub mailbox and a real spoke client pushing the mock
// provider state and executing commands through the spoke's own API handler.
func TestOutboundSpokeEndToEnd(t *testing.T) {
	hub := provider.NewSpokeHub(provider.SpokeHubConfig{
		SyncInterval:   50 * time.Millisecond,
		CommandWait:    100 * time.Millisecond,
		RedeliverAfter: 200 * time.Millisecond,
		CommandTimeout: 3 * time.Second,
	})
	if err := hub.AddCluster(e2eCluster, e2eToken); err != nil {
		t.Fatalf("AddCluster: %v", err)
	}
	hubProvider, err := hub.Provider(e2eCluster)
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}

	mgr, err := auth.NewManager(auth.Config{
		Username:      "admin",
		Password:      "pass123",
		SessionSecret: []byte("unit-test-secret"),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	reg := provider.NewRegistry()
	reg.Register(provider.ClusterEntry{
		ID:       e2eCluster,
		Name:     e2eCluster,
		Role:     "remote",
		Endpoint: "outbound",
		Provider: hubProvider,
	})
	hubServer, err := web.NewServer(reg, mgr, web.WithSpokeHub(hub))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(hubServer)
	defer ts.Close()

	local := provider.NewMockProvider(e2eCluster)
	spokeHandler := web.NewSpokeServer(local, e2eToken)
	client, err := spokeclient.New(spokeclient.Config{
		HubURL:       ts.URL,
		Cluster:      e2eCluster,
		Token:        e2eToken,
		Handler:      spokeHandler,
		Provider:     local,
		SyncInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("spokeclient.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		client.Run(ctx)
	}()

	// The hub only learns about the cluster from the pushed state.
	deadline := time.Now().Add(10 * time.Second)
	var snap model.Snapshot
	for time.Now().Before(deadline) {
		snap = hubProvider.Snapshot(context.Background())
		if snap.SyncStatus != model.SyncOffline && snap.Summary.TotalWorkers > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snap.Summary.TotalWorkers == 0 || snap.SyncStatus == model.SyncOffline {
		t.Fatalf("hub never received the pushed state: %+v", snap)
	}
	if health := hubProvider.Health(context.Background()); !health.Online {
		t.Fatalf("hub health = %+v, want online", health)
	}
	if samples := hubProvider.PullCounters(); samples == nil {
		t.Fatal("hub has no counters after the first state push")
	}
	if len(snap.Nodes) == 0 {
		t.Fatal("pushed snapshot has no nodes")
	}

	// The mock cluster only accepts image/node pairs it knows about, and refuses
	// offline nodes; pick a pair from the pushed snapshot (prefer a failed cell,
	// which is the realistic "warm this up" case).
	ready := make(map[string]bool, len(snap.Nodes))
	for _, n := range snap.Nodes {
		if n.Ready && n.AgentUp {
			ready[n.Name] = true
		}
	}
	var image, node string
	for _, workload := range snap.Workloads {
		for _, img := range workload.Images {
			for _, state := range img.Nodes {
				if !ready[state.Node] {
					continue
				}
				if state.State == model.StateFailed || image == "" {
					image, node = img.Image, state.Node
				}
			}
		}
	}
	if image == "" || node == "" {
		t.Fatalf("pushed snapshot has no missing image to pull: %+v", snap.Summary)
	}

	// A pull is triggered on the hub and executed by the spoke: the round trip
	// goes hub -> command mailbox -> spoke handler -> result -> hub.
	pullCtx, pullCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pullCancel()
	task, err := hubProvider.TriggerPull(pullCtx, model.PullRequest{
		Image:   image,
		Node:    node,
		Actor:   "tester",
		Trigger: model.TriggerManual,
	})
	if err != nil {
		t.Fatalf("TriggerPull through the outbound channel: %v", err)
	}
	if task.ID == "" {
		t.Fatalf("TriggerPull returned an empty task: %+v", task)
	}

	if _, err := hubProvider.TriggerWorkloadPull(pullCtx, model.WorkloadPullRequest{
		Namespace: "demo",
		Kind:      "Deployment",
		Name:      "web",
		Actor:     "tester",
		Trigger:   model.TriggerManual,
	}); err != nil {
		// The mock may not have that workload; only a transport failure is fatal.
		if errors.Is(err, provider.ErrSpokeUnavailable) || errors.Is(err, provider.ErrSpokeUnauthorized) {
			t.Fatalf("TriggerWorkloadPull transport failure: %v", err)
		}
	}

	if err := hubProvider.SyncNow(pullCtx, "tester"); err != nil {
		t.Fatalf("SyncNow through the outbound channel: %v", err)
	}

	// The task must become visible in the pushed snapshot.
	deadline = time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		for _, pushed := range hubProvider.Snapshot(context.Background()).Tasks {
			if pushed.ID == task.ID {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatal("the task triggered through the hub never appeared in the pushed snapshot")
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("spoke client did not stop after context cancellation")
	}
}
