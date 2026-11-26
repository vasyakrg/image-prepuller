package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// fastConfig keeps retry tests fast while still exercising the real backoff.
func fastConfig() RemoteConfig {
	return RemoteConfig{
		Timeout:     200 * time.Millisecond,
		MaxAttempts: 3,
		Backoff:     5 * time.Millisecond,
	}
}

func newTestRemote(url string, cfg RemoteConfig) *RemoteProvider {
	return NewRemoteProviderWithConfig("kz-1", url, "tok", cfg)
}

// TestRemoteSnapshotTimeoutIsOffline checks the per-request timeout and the
// offline snapshot fallback: a hanging spoke must not stall a metrics scrape.
func TestRemoteSnapshotTimeoutIsOffline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	cfg := fastConfig()
	cfg.Timeout = 40 * time.Millisecond
	remote := newTestRemote(srv.URL, cfg)

	started := time.Now()
	snap := remote.Snapshot(context.Background())
	elapsed := time.Since(started)

	if elapsed > time.Second {
		t.Errorf("Snapshot took %v, want it bounded by the request timeout", elapsed)
	}
	if snap.SyncStatus != model.SyncOffline || !snap.Partial {
		t.Errorf("offline snapshot = %+v, want syncStatus offline and partial", snap)
	}
	if snap.Summary.TotalWorkers != 0 {
		t.Errorf("offline snapshot totalWorkers = %d, want 0", snap.Summary.TotalWorkers)
	}
	if remote.LastError() == "" {
		t.Error("LastError is empty after a timed out snapshot")
	}

	health := remote.Health(context.Background())
	if health.Online {
		t.Errorf("Health online on a dead spoke: %+v", health)
	}
	if health.Error == "" {
		t.Error("Health error text is empty on a dead spoke")
	}
	if remote.Timeout() != cfg.Timeout {
		t.Errorf("Timeout() = %v, want %v", remote.Timeout(), cfg.Timeout)
	}
}

// TestRemoteRetriesTransientErrors covers the retry policy for idempotent calls.
func TestRemoteRetriesTransientErrors(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	if err := newTestRemote(srv.URL, fastConfig()).SyncNow(context.Background(), "admin"); err != nil {
		t.Fatalf("SyncNow after two 503s = %v, want success", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// TestRemoteRetryExhaustionIsUnavailable checks the typed ErrSpokeUnavailable.
func TestRemoteRetryExhaustionIsUnavailable(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	err := newTestRemote(srv.URL, fastConfig()).SyncNow(context.Background(), "admin")
	if !errors.Is(err, ErrSpokeUnavailable) {
		t.Fatalf("SyncNow error = %v, want ErrSpokeUnavailable", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// TestRemoteUnauthorizedIsNotRetried keeps 401 a single, typed decision.
func TestRemoteUnauthorizedIsNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := newTestRemote(srv.URL, fastConfig()).SyncNow(context.Background(), "admin")
	if !errors.Is(err, ErrSpokeUnauthorized) {
		t.Fatalf("SyncNow error = %v, want ErrSpokeUnauthorized", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (401 is not retried)", got)
	}
}

// TestRemotePullRetryKeepsSingleTask proves that retrying a POST pull cannot
// multiply pull tasks: the spoke answers with the idempotent task.
func TestRemotePullRetryKeepsSingleTask(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"task-1","image":"nginx:latest","node":"n1","state":"running"}`))
	}))
	defer srv.Close()

	task, err := newTestRemote(srv.URL, fastConfig()).TriggerPull(context.Background(), model.PullRequest{
		Image: "nginx:latest",
		Node:  "n1",
	})
	if err != nil {
		t.Fatalf("TriggerPull: %v", err)
	}
	if task.ID != "task-1" || task.Cluster != "kz-1" || task.State != model.TaskRunning {
		t.Errorf("task = %+v, want the idempotent spoke task-1", task)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2", got)
	}
}

// TestRemoteCancelStatusMapping maps spoke 404/409 onto provider sentinels.
func TestRemoteCancelStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, ErrTaskNotFound},
		{http.StatusConflict, ErrTaskNotActive},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		err := newTestRemote(srv.URL, fastConfig()).CancelPull(context.Background(), "task-9")
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("CancelPull with HTTP %d = %v, want %v", tc.status, err, tc.want)
		}
	}

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := newTestRemote(srv.URL, fastConfig()).CancelPull(context.Background(), "task-9")
	if err == nil || errors.Is(err, ErrTaskNotFound) {
		t.Errorf("CancelPull with HTTP 500 = %v, want a generic error", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (500 is not retried)", got)
	}
}

// TestRemoteContextNotSwallowed verifies that cancellation is reported as such
// and never turned into retries or a timeout wrapper.
func TestRemoteContextNotSwallowed(t *testing.T) {
	// The handler sleeps instead of watching r.Context(): an HTTP/1.1 server is
	// not notified when the client cancels, and httptest.Server.Close would then
	// block on the handler forever.
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		time.Sleep(150 * time.Millisecond)
	}))
	defer srv.Close()

	remote := newTestRemote(srv.URL, fastConfig())

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := remote.TriggerPull(canceled, model.PullRequest{Image: "nginx:latest", Node: "n1"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("TriggerPull with a canceled context = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 0 {
		t.Errorf("attempts = %d, want 0 for a pre-canceled context", got)
	}

	midFlight, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	_, err = remote.TriggerPull(midFlight, model.PullRequest{Image: "nginx:latest", Node: "n1"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("TriggerPull with an expiring context = %v, want context.DeadlineExceeded", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (a canceled call is not retried)", got)
	}
}

// TestRemoteCountersEnvelopeAndLegacy accepts both the v2 envelope and the bare
// sample array of an older spoke.
func TestRemoteCountersEnvelopeAndLegacy(t *testing.T) {
	envelope := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pullTotal":7,"pullSucceeded":5,"pullFailed":1,"pullCanceled":1,
			"pullInFlight":2,"imagesCached":9,"lastSyncAt":"2026-11-26T12:00:00Z",
			"samples":[{"image":"nginx:latest","node":"n1","result":"success","count":5}]}`))
	}))
	defer envelope.Close()

	counters, err := newTestRemote(envelope.URL, fastConfig()).Counters(context.Background())
	if err != nil {
		t.Fatalf("Counters: %v", err)
	}
	if counters.PullTotal != 7 || counters.PullSucceeded != 5 || counters.PullInFlight != 2 || counters.ImagesCached != 9 {
		t.Errorf("counters = %+v, want the aggregate fields", counters)
	}
	if len(counters.Samples) != 1 || counters.Samples[0].Count != 5 {
		t.Errorf("samples = %+v, want one sample", counters.Samples)
	}

	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"image":"nginx:latest","node":"n1","result":"success","count":3}]`))
	}))
	defer legacy.Close()
	legacyRemote := newTestRemote(legacy.URL, fastConfig())
	if got := legacyRemote.PullCounters(); len(got) != 1 || got[0].Count != 3 {
		t.Errorf("legacy PullCounters = %+v, want one sample with count 3", got)
	}
}

// TestRemoteCountersOfflineMeansNoMetrics keeps "no counters" distinct from
// "zero counters".
func TestRemoteCountersOfflineMeansNoMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	url := srv.URL
	srv.Close()

	remote := newTestRemote(url, fastConfig())
	if got := remote.PullCounters(); got != nil {
		t.Errorf("PullCounters on a dead spoke = %+v, want nil", got)
	}
	if _, err := remote.Counters(context.Background()); !errors.Is(err, ErrSpokeUnavailable) {
		t.Errorf("Counters on a dead spoke = %v, want ErrSpokeUnavailable", err)
	}
}

// TestRemoteHealthUpAndFallback covers the v2 health endpoint and the /healthz
// fallback for an older spoke.
func TestRemoteHealthUpAndFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/health":
			_, _ = w.Write([]byte(`{"online":true,"version":"v2","checkedAt":"2026-11-26T12:00:00Z",
				"apiVersion":"v2","uptimeSeconds":42}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	report := newTestRemote(srv.URL, fastConfig()).HealthReport(context.Background())
	if !report.Online || report.Version != model.APIVersion {
		t.Errorf("health report = %+v, want online with version %q", report, model.APIVersion)
	}
	if report.APIVersion != model.APIVersion || report.UptimeSeconds != 42 {
		t.Errorf("health report contract fields = %+v", report)
	}
	if report.TimeoutMs <= 0 || report.LatencyMs < 0 || report.CheckedAt.IsZero() {
		t.Errorf("health report timings = %+v", report)
	}

	oldSpoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("X-Prepuller-Version", model.APIVersion)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer oldSpoke.Close()

	fallback := newTestRemote(oldSpoke.URL, fastConfig()).HealthReport(context.Background())
	if !fallback.Online || fallback.Version != model.APIVersion {
		t.Errorf("healthz fallback = %+v, want online with version %q", fallback, model.APIVersion)
	}
}

// TestRemoteLastErrorLifecycle keeps the last transport error visible and
// clears it after a successful call.
func TestRemoteLastErrorLifecycle(t *testing.T) {
	var healthy int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/snapshot" && atomic.LoadInt32(&healthy) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	remote := newTestRemote(srv.URL, fastConfig())
	if snap := remote.Snapshot(context.Background()); snap.SyncStatus != model.SyncOffline {
		t.Fatalf("snapshot syncStatus = %q, want offline", snap.SyncStatus)
	}
	if !strings.Contains(remote.LastError(), "500") {
		t.Errorf("LastError = %q, want the HTTP 500 detail", remote.LastError())
	}

	atomic.StoreInt32(&healthy, 1)
	if err := remote.SyncNow(context.Background(), "admin"); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if remote.LastError() != "" {
		t.Errorf("LastError = %q, want empty after a successful call", remote.LastError())
	}
}
