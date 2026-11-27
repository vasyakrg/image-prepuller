package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

const (
	spokeHubTestCluster = "talos-kz"
	spokeHubTestToken   = "cluster-token"
)

func newSpokeHubTestServer(t *testing.T) (*provider.SpokeHub, *provider.PushProvider, http.Handler) {
	t.Helper()
	mgr, err := auth.NewManager(auth.Config{
		Username:      "admin",
		Password:      "pass123",
		SessionSecret: []byte("unit-test-secret"),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	hub := provider.NewSpokeHub(provider.SpokeHubConfig{
		SyncInterval:   50 * time.Millisecond,
		CommandWait:    100 * time.Millisecond,
		RedeliverAfter: time.Second,
	})
	if err := hub.AddCluster(spokeHubTestCluster, spokeHubTestToken); err != nil {
		t.Fatalf("AddCluster: %v", err)
	}
	prov, err := hub.Provider(spokeHubTestCluster)
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	reg := provider.NewRegistry()
	reg.Register(provider.ClusterEntry{
		ID:       spokeHubTestCluster,
		Name:     spokeHubTestCluster,
		Role:     "remote",
		Endpoint: "outbound",
		Provider: prov,
	})
	srv, err := NewServer(reg, mgr, WithSpokeHub(hub))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return hub, prov, srv
}

func spokeHubRequest(method, path, token string, body any) *http.Request {
	var reader *bytes.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func TestSpokeHubRoutesAuthenticateAndAcceptState(t *testing.T) {
	_, prov, srv := newSpokeHubTestServer(t)
	statePath := "/internal/v1/spokes/" + spokeHubTestCluster + "/state"

	state := provider.SpokeState{
		APIVersion: model.APIVersion,
		SentAt:     time.Now(),
		Version:    model.APIVersion,
		Snapshot: model.Snapshot{
			SyncStatus: model.SyncOK,
			LastSyncAt: time.Now(),
			Summary:    model.Summary{TotalWorkers: 2, ReadyWorkers: 2},
		},
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodPost, statePath, "", state))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("state push without a token: got %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodPost, statePath, "wrong-token", state))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("state push with a wrong token: got %d, want 401", rec.Code)
	}

	unknown := "/internal/v1/spokes/talos-eu/state"
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodPost, unknown, spokeHubTestToken, state))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("state push from an unregistered cluster: got %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodPost, statePath, spokeHubTestToken, state))
	if rec.Code != http.StatusOK {
		t.Fatalf("state push: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := prov.Snapshot(context.Background()); got.Summary.TotalWorkers != 2 {
		t.Fatalf("snapshot after state push = %+v", got.Summary)
	}
	if health := prov.Health(context.Background()); !health.Online {
		t.Fatalf("health after state push = %+v, want online", health)
	}

	// An unsupported contract version is rejected, not stored.
	badVersion := state
	badVersion.APIVersion = "v99"
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodPost, statePath, spokeHubTestToken, badVersion))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state push with apiVersion v99: got %d, want 400", rec.Code)
	}
}

func TestSpokeHubCommandsDispatchAndResult(t *testing.T) {
	_, prov, srv := newSpokeHubTestServer(t)
	commandsPath := "/internal/v1/spokes/" + spokeHubTestCluster + "/commands"

	type pullOutcome struct {
		task model.PullTask
		err  error
	}
	outcome := make(chan pullOutcome, 1)
	go func() {
		task, err := prov.TriggerPull(context.Background(), model.PullRequest{Image: "nginx:1.27", Node: "worker-1"})
		outcome <- pullOutcome{task: task, err: err}
	}()

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodGet, commandsPath, spokeHubTestToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("command poll: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var answer provider.SpokeCommands
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode command poll: %v", err)
	}
	if len(answer.Commands) != 1 {
		t.Fatalf("command poll delivered %d commands, want 1", len(answer.Commands))
	}
	cmd := answer.Commands[0]
	if cmd.Method != http.MethodPost || cmd.Path != "/internal/v1/pull" {
		t.Fatalf("command = %+v, want POST /internal/v1/pull", cmd)
	}
	if !strings.Contains(string(cmd.Body), "nginx:1.27") {
		t.Fatalf("command body = %s", cmd.Body)
	}

	result := provider.SpokeCommandResult{
		Status: http.StatusAccepted,
		Body:   json.RawMessage(`{"id":"task-1","image":"nginx:1.27","node":"worker-1","state":"running"}`),
	}
	resultPath := commandsPath + "/" + cmd.ID + "/result"
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodPost, resultPath, spokeHubTestToken, result))
	if rec.Code != http.StatusOK {
		t.Fatalf("result post: got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	select {
	case got := <-outcome:
		if got.err != nil {
			t.Fatalf("TriggerPull: %v", got.err)
		}
		if got.task.ID != "task-1" || got.task.Cluster != spokeHubTestCluster {
			t.Fatalf("TriggerPull task = %+v", got.task)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TriggerPull did not return after the result was posted")
	}

	// A result for a command the hub already gave up on is a no-op.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodPost, commandsPath+"/stale-id/result", spokeHubTestToken, result))
	if rec.Code != http.StatusOK {
		t.Fatalf("spurious result: got %d, want 200", rec.Code)
	}
}

func TestSpokeHubCommandPollWithoutWorkAnswers204(t *testing.T) {
	_, _, srv := newSpokeHubTestServer(t)
	path := "/internal/v1/spokes/" + spokeHubTestCluster + "/commands"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodGet, path, spokeHubTestToken, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("idle command poll: got %d, want 204", rec.Code)
	}
}

func TestHubWithoutSpokeHubHasNoSpokeRoutes(t *testing.T) {
	mgr, err := auth.NewManager(auth.Config{
		Username:      "admin",
		Password:      "pass123",
		SessionSecret: []byte("unit-test-secret"),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	reg := provider.NewRegistry()
	reg.Register(provider.ClusterEntry{ID: "talos-hub", Name: "talos-hub", Role: "local", Provider: provider.NewMockProvider("talos-hub")})
	srv, err := NewServer(reg, mgr)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, spokeHubRequest(http.MethodGet, "/internal/v1/spokes/talos-kz/commands", spokeHubTestToken, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("spoke route on a hub without the outbound channel: got %d, want 404", rec.Code)
	}
}
