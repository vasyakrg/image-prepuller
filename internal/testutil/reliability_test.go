package testutil_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/metrics"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
	"github.com/local/image-prepuller/internal/testutil"
)

// testRemoteConfig keeps spoke↔hub reliability tests fast.
func testRemoteConfig() provider.RemoteConfig {
	return provider.RemoteConfig{
		Timeout:     500 * time.Millisecond,
		MaxAttempts: 2,
		Backoff:     time.Millisecond,
	}
}

// authedSpoke performs one Bearer-authenticated request against the fake spoke.
func authedSpoke(t *testing.T, spoke *testutil.FakeSpoke, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+spoke.Token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	spoke.Server.Config.Handler.ServeHTTP(rec, req)
	return rec
}

// TestSpokeAPIVersionValidation covers the rollout window: v1/v2/empty are
// accepted, anything else is a 400 with a JSON error.
func TestSpokeAPIVersionValidation(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-1")

	for _, apiVersion := range []string{"", model.PreviousAPIVersion, model.APIVersion} {
		body := `{"apiVersion":"` + apiVersion + `","image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01"}`
		if apiVersion == "" {
			body = `{"image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01"}`
		}
		rec := authedSpoke(t, spoke, http.MethodPost, "/internal/v1/pull", body)
		if rec.Code != http.StatusAccepted {
			t.Errorf("apiVersion %q pull = %d: %s", apiVersion, rec.Code, rec.Body.String())
		}
	}

	rec := authedSpoke(t, spoke, http.MethodPost, "/internal/v1/pull", `{"apiVersion":"v9","image":"x","node":"y"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported apiVersion") {
		t.Errorf("unsupported apiVersion = %d: %s, want 400 with an error", rec.Code, rec.Body.String())
	}

	rec = authedSpoke(t, spoke, http.MethodPost, "/internal/v1/reconcile", `{"apiVersion":"v9"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("reconcile with apiVersion v9 = %d, want 400", rec.Code)
	}

	rec = authedSpoke(t, spoke, http.MethodPost, "/internal/v1/pull", "{")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("invalid JSON = %d: %s, want 400 with an error", rec.Code, rec.Body.String())
	}
}

// TestSpokeIgnoresUnknownFields keeps the hub-side additive evolution possible.
func TestSpokeIgnoresUnknownFields(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-1")
	rec := authedSpoke(t, spoke, http.MethodPost, "/internal/v1/pull",
		`{"apiVersion":"v2","image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01","future":{"a":1},"extra":"ignored"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pull with unknown fields = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSpokeMethodNotAllowed relies on the Go 1.22 mux: a wrong method on a known
// path is a 405, not a 404.
func TestSpokeMethodNotAllowed(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-1")
	cases := []struct{ method, path string }{
		{http.MethodGet, "/internal/v1/pulls/task-1/cancel"},
		{http.MethodPut, "/internal/v1/snapshot"},
		{http.MethodDelete, "/internal/v1/counters"},
		{http.MethodPost, "/internal/v1/health"},
	}
	for _, tc := range cases {
		rec := authedSpoke(t, spoke, tc.method, tc.path, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
		}
	}
}

// TestSpokeCancelStatuses covers 202/404/409 and the idempotent repeat cancel.
func TestSpokeCancelStatuses(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-1")

	rec := authedSpoke(t, spoke, http.MethodPost, "/internal/v1/pulls/task-unknown/cancel", `{"apiVersion":"v2"}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("cancel unknown task = %d: %s, want 404 with an error", rec.Code, rec.Body.String())
	}

	rec = authedSpoke(t, spoke, http.MethodPost, "/internal/v1/pull",
		`{"apiVersion":"v2","image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01","actor":"admin"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pull = %d: %s", rec.Code, rec.Body.String())
	}
	var task model.PullTask
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}

	rec = authedSpoke(t, spoke, http.MethodPost, "/internal/v1/pulls/"+task.ID+"/cancel", `{"apiVersion":"v2"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first cancel = %d: %s, want 202", rec.Code, rec.Body.String())
	}

	rec = authedSpoke(t, spoke, http.MethodPost, "/internal/v1/pulls/"+task.ID+"/cancel", `{"apiVersion":"v2"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("repeated cancel = %d: %s, want 409 with an error", rec.Code, rec.Body.String())
	}
}

// TestSpokeCountersAlwaysFull checks the aggregate counters answer: 200 with
// every field present even for an idle cluster.
func TestSpokeCountersAlwaysFull(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-1")
	rec := authedSpoke(t, spoke, http.MethodGet, "/internal/v1/counters", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("counters = %d: %s", rec.Code, rec.Body.String())
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("counters body is not an object: %v (%s)", err, rec.Body.String())
	}
	for _, key := range []string{
		"pullTotal", "pullSucceeded", "pullFailed", "pullCanceled",
		"pullInFlight", "imagesCached", "lastSyncAt", "samples",
	} {
		if _, ok := fields[key]; !ok {
			t.Errorf("counters response misses %q: %s", key, rec.Body.String())
		}
	}

	var counters provider.SpokeCounters
	if err := json.Unmarshal(rec.Body.Bytes(), &counters); err != nil {
		t.Fatalf("decode counters: %v", err)
	}
	if counters.Samples == nil || len(counters.Samples) == 0 {
		t.Errorf("counters samples = %+v, want the per-series data for prepuller_pull_total", counters.Samples)
	}
	if counters.PullTotal == 0 {
		t.Errorf("counters pullTotal = 0, want the sum of the mock counters")
	}
	if counters.LastSyncAt.IsZero() {
		t.Error("counters lastSyncAt is zero")
	}
}

// TestSpokeCountersRequireToken keeps the counters endpoint behind Bearer auth.
func TestSpokeCountersRequireToken(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-1")
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/counters", nil)
	rec := httptest.NewRecorder()
	spoke.Server.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("counters without token = %d, want 401", rec.Code)
	}
}

// TestSpokeHealthContract checks /internal/v1/health fields and the
// RemoteProvider mapping (online/latency/version/timeout).
func TestSpokeHealthContract(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-1")

	rec := authedSpoke(t, spoke, http.MethodGet, "/internal/v1/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Online        bool      `json:"online"`
		Version       string    `json:"version"`
		APIVersion    string    `json:"apiVersion"`
		UptimeSeconds int64     `json:"uptimeSeconds"`
		CheckedAt     time.Time `json:"checkedAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if !body.Online || body.Version != model.APIVersion || body.APIVersion != model.APIVersion {
		t.Errorf("health = %+v, want online with version/apiVersion %q", body, model.APIVersion)
	}
	if body.UptimeSeconds < 0 || body.CheckedAt.IsZero() {
		t.Errorf("health uptime/checkedAt = %+v", body)
	}

	remote := provider.NewRemoteProviderWithConfig(spoke.Cluster, spoke.URL(), spoke.Token, testRemoteConfig())
	report := remote.HealthReport(context.Background())
	if !report.Online || report.Error != "" {
		t.Errorf("remote health = %+v, want online without an error", report)
	}
	if report.Version != model.APIVersion || report.APIVersion != model.APIVersion {
		t.Errorf("remote health versions = %q/%q", report.Version, report.APIVersion)
	}
	if report.TimeoutMs <= 0 || report.LatencyMs < 0 || report.CheckedAt.IsZero() {
		t.Errorf("remote health timings = %+v", report)
	}
}

// TestRemoteProviderDeadSpoke covers the offline behaviour: no panics, typed
// errors, offline snapshot and nil counters.
func TestRemoteProviderDeadSpoke(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-dead")
	url := spoke.URL()
	spoke.Server.Close()

	remote := provider.NewRemoteProviderWithConfig("kz-dead", url, spoke.Token, testRemoteConfig())
	ctx := context.Background()

	snap := remote.Snapshot(ctx)
	if snap.SyncStatus != model.SyncOffline || !snap.Partial {
		t.Errorf("dead spoke snapshot = %+v, want offline and partial", snap)
	}
	if snap.Summary.TotalWorkers != 0 {
		t.Errorf("dead spoke snapshot totalWorkers = %d, want 0", snap.Summary.TotalWorkers)
	}

	health := remote.Health(ctx)
	if health.Online || health.Error == "" {
		t.Errorf("dead spoke health = %+v, want offline with an error", health)
	}

	if got := remote.PullCounters(); got != nil {
		t.Errorf("dead spoke PullCounters = %+v, want nil", got)
	}
	if _, err := remote.Counters(ctx); !errors.Is(err, provider.ErrSpokeUnavailable) {
		t.Errorf("dead spoke Counters = %v, want ErrSpokeUnavailable", err)
	}
	if remote.LastError() == "" {
		t.Error("LastError is empty after a dead spoke")
	}
	if _, err := remote.TriggerPull(ctx, model.PullRequest{Image: "nginx:latest", Node: "n1"}); !errors.Is(err, provider.ErrSpokeUnavailable) {
		t.Errorf("dead spoke TriggerPull = %v, want ErrSpokeUnavailable", err)
	}
	if err := remote.CancelPull(ctx, "task-1"); !errors.Is(err, provider.ErrSpokeUnavailable) {
		t.Errorf("dead spoke CancelPull = %v, want ErrSpokeUnavailable", err)
	}
}

// TestRemoteProviderRejectedToken keeps 401 distinct from 502.
func TestRemoteProviderRejectedToken(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-1")
	remote := provider.NewRemoteProviderWithConfig(spoke.Cluster, spoke.URL(), "wrong-token", testRemoteConfig())

	if _, err := remote.Counters(context.Background()); !errors.Is(err, provider.ErrSpokeUnauthorized) {
		t.Errorf("Counters with a wrong token = %v, want ErrSpokeUnauthorized", err)
	}
	if err := remote.SyncNow(context.Background(), "admin"); !errors.Is(err, provider.ErrSpokeUnauthorized) {
		t.Errorf("SyncNow with a wrong token = %v, want ErrSpokeUnauthorized", err)
	}
}

// TestHubStaysUpWithDeadSpoke is the acceptance check: a dead spoke shows up as
// offline in the UI/API, the hub keeps answering and /metrics still renders.
func TestHubStaysUpWithDeadSpoke(t *testing.T) {
	hub := testutil.NewFakeHub(t, "local-1")
	spoke := testutil.NewFakeSpoke(t, "kz-dead")
	hub.RegisterRemote(spoke)
	spoke.Server.Close()

	cookie := login(t, hub.Server, testutil.TestHubUser, testutil.TestHubPass)

	rec := doJSON(t, hub.Server, http.MethodGet, "/api/v1/snapshot?cluster=kz-dead", cookie, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"syncStatus":"offline"`) {
		t.Fatalf("snapshot of a dead spoke = %d: %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, hub.Server, http.MethodGet, "/api/v1/health?cluster=kz-dead", cookie, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"online":false`) {
		t.Fatalf("health of a dead spoke = %d: %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, hub.Server, http.MethodGet, "/api/v1/clusters", cookie, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "kz-dead") {
		t.Fatalf("clusters with a dead spoke = %d: %s", rec.Code, rec.Body.String())
	}

	mRec := httptest.NewRecorder()
	metrics.Handler(hub.Registry).ServeHTTP(mRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if mRec.Code != http.StatusOK {
		t.Fatalf("metrics with a dead spoke = %d", mRec.Code)
	}
	if !strings.Contains(mRec.Body.String(), `prepuller_image_present{cluster="local-1"`) {
		t.Error("metrics output lost the healthy local cluster")
	}
}

// TestSpokeBodyLimit bounds request bodies; a huge body is a 413.
func TestSpokeBodyLimit(t *testing.T) {
	spoke := testutil.NewFakeSpoke(t, "kz-1")
	huge := `{"apiVersion":"v2","image":"nginx:latest","node":"n1","pad":"` +
		strings.Repeat("a", 2<<20) + `"}`
	rec := authedSpoke(t, spoke, http.MethodPost, "/internal/v1/pull", huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge body = %d: %s, want 413", rec.Code, rec.Body.String())
	}
}
