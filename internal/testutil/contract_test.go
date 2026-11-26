package testutil_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
	"github.com/local/image-prepuller/internal/testutil"
)

// TestGoldenSnapshotContract keeps the checked-in fixture honest.
func TestGoldenSnapshotContract(t *testing.T) {
	snap := testutil.GoldenSnapshot(t)
	testutil.RequireContractV2(t, snap)
	if len(snap.Tasks) == 0 {
		t.Fatal("golden snapshot has no tasks")
	}
	if snap.Tasks[0].State != model.TaskRunning {
		t.Errorf("golden task state = %q, want running", snap.Tasks[0].State)
	}
	if snap.Activity[0].Trigger != model.TriggerManual || snap.Activity[0].Actor != "admin" {
		t.Errorf("golden event trigger/actor = %q/%q", snap.Activity[0].Trigger, snap.Activity[0].Actor)
	}
}

// TestRemoteProviderAgainstFakeSpoke is the hub↔spoke wire contract test.
func TestRemoteProviderAgainstFakeSpoke(t *testing.T) {
	ctx := context.Background()
	spoke := testutil.NewFakeSpoke(t, "kz-1")
	remote := spoke.Remote()

	snap := remote.Snapshot(ctx)
	if snap.ClusterName != "kz-1" {
		t.Errorf("snapshot cluster = %q", snap.ClusterName)
	}
	testutil.RequireContractV2(t, snap)
	if snap.SyncStatus == model.SyncUnknown || snap.SyncStatus == model.SyncOffline {
		t.Errorf("snapshot syncStatus = %q, want ok/degraded", snap.SyncStatus)
	}

	health := remote.Health(ctx)
	if !health.Online {
		t.Fatalf("remote health offline: %+v", health)
	}
	if health.Version != model.APIVersion {
		t.Errorf("remote health version = %q, want %q", health.Version, model.APIVersion)
	}

	counters := remote.PullCounters()
	if len(counters) == 0 {
		t.Fatal("remote counters are empty: /internal/v1/counters is not wired")
	}

	req := model.PullRequest{
		Image:   "ghcr.io/acme/api-gateway:1.14.2",
		Node:    "talos-worker-01",
		Actor:   "admin",
		Trigger: model.TriggerManual,
	}
	task, err := remote.TriggerPull(ctx, req)
	if err != nil {
		t.Fatalf("TriggerPull: %v", err)
	}
	if task.ID == "" || task.State != model.TaskRunning {
		t.Fatalf("TriggerPull returned %+v, want running task with id", task)
	}
	if task.Cluster != "kz-1" {
		t.Errorf("task cluster = %q, want kz-1", task.Cluster)
	}

	// Idempotency: the same image+node returns the same task.
	again, err := remote.TriggerPull(ctx, req)
	if err != nil {
		t.Fatalf("second TriggerPull: %v", err)
	}
	if again.ID != task.ID {
		t.Errorf("TriggerPull is not idempotent: %q != %q", again.ID, task.ID)
	}

	if err := remote.CancelPull(ctx, task.ID); err != nil {
		t.Fatalf("CancelPull: %v", err)
	}
	if err := remote.CancelPull(ctx, task.ID); err == nil {
		t.Error("CancelPull of a finished task must fail")
	}
	if err := remote.CancelPull(ctx, "task-unknown"); !errors.Is(err, provider.ErrTaskNotFound) {
		t.Errorf("CancelPull(unknown) = %v, want ErrTaskNotFound", err)
	}
}

// TestHubAPIContractV2 covers the hub endpoints frozen for WS-D/WS-C/WS-H.
func TestHubAPIContractV2(t *testing.T) {
	hub := testutil.NewFakeHub(t, "local-1")
	spoke := testutil.NewFakeSpoke(t, "kz-1")
	hub.RegisterRemote(spoke)

	cookie := login(t, hub.Server, testutil.TestHubUser, testutil.TestHubPass)

	// Unauthenticated access is rejected.
	rec := httptest.NewRecorder()
	hub.Server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pull-tasks?cluster=local-1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("pull-tasks without cookie = %d, want 401", rec.Code)
	}

	// Start a manual pull on the remote cluster and see it in /api/v1/pull-tasks.
	rec = doJSON(t, hub.Server, http.MethodPost, "/api/v1/pull", cookie, map[string]string{
		"cluster": "kz-1",
		"image":   "ghcr.io/acme/api-gateway:1.14.2",
		"node":    "talos-worker-01",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/v1/pull = %d: %s", rec.Code, rec.Body.String())
	}
	var pullResp struct {
		Task model.PullTask `json:"task"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pullResp); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	if pullResp.Task.ID == "" {
		t.Fatalf("pull response has no task: %s", rec.Body.String())
	}

	rec = doJSON(t, hub.Server, http.MethodGet, "/api/v1/pull-tasks?cluster=kz-1", cookie, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), pullResp.Task.ID) {
		t.Fatalf("pull-tasks = %d: %s", rec.Code, rec.Body.String())
	}

	// Cancel through the hub.
	rec = doJSON(t, hub.Server, http.MethodPost, "/api/v1/pulls/"+pullResp.Task.ID+"/cancel?cluster=kz-1", cookie, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}

	// Node images view for the local mock cluster.
	rec = doJSON(t, hub.Server, http.MethodGet, "/api/v1/nodes/talos-worker-01/images?cluster=local-1", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("node images = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"cachedImages", "imageFsUsed", "mock-0.1.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("node images response misses %q: %s", want, body)
		}
	}

	// Provider health stub.
	rec = doJSON(t, hub.Server, http.MethodGet, "/api/v1/health?cluster=kz-1", cookie, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"online":true`) {
		t.Fatalf("health = %d: %s", rec.Code, rec.Body.String())
	}

	// Unknown cluster is a 404, not a panic.
	rec = doJSON(t, hub.Server, http.MethodGet, "/api/v1/pull-tasks?cluster=nope", cookie, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown cluster pull-tasks = %d, want 404", rec.Code)
	}
}

func login(t *testing.T, h http.Handler, user, pass string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not set a cookie")
	}
	return cookies[0]
}

func doJSON(t *testing.T, h http.Handler, method, path string, cookie *http.Cookie, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
