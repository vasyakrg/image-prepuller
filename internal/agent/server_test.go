package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestServer(t *testing.T, token string) (*Server, *fakeCRI) {
	t.Helper()
	f := newFakeCRI()
	cfg := testConfig()
	cfg.Token = token
	srv, err := NewServer(cfg, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, f
}

func doRequest(t *testing.T, h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthNeedsNoAuth(t *testing.T) {
	srv, _ := newTestServer(t, "secret")
	rec := doRequest(t, srv.Handler(), http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var health Health
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.Status != "ok" || health.APIVersion != WireAPIVersion || health.AgentVersion != Version {
		t.Fatalf("unexpected health payload: %+v", health)
	}
	if health.NodeName != "talos-worker-01" {
		t.Fatalf("health node = %q", health.NodeName)
	}
}

func TestSnapshotRequiresBearerToken(t *testing.T) {
	srv, _ := newTestServer(t, "secret")
	for _, tc := range []struct {
		name  string
		token string
	}{{name: "missing"}, {name: "wrong", token: "nope"}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv.Handler(), http.MethodGet, snapshotPath, "", tc.token)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
	rec := doRequest(t, srv.Handler(), http.MethodGet, snapshotPath, "", "secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotPayload(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := doRequest(t, srv.Handler(), http.MethodGet, snapshotPath, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type = %q", ct)
	}
	var snap Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.APIVersion != WireAPIVersion || snap.NodeName != "talos-worker-01" {
		t.Fatalf("unexpected snapshot identity: %+v", snap)
	}
	if snap.CachedImageCount != 4 || snap.ImageFS.CapacityBytes == 0 {
		t.Fatalf("unexpected snapshot inventory: count=%d fs=%+v", snap.CachedImageCount, snap.ImageFS)
	}
}

func TestSnapshotCRIError(t *testing.T) {
	srv, f := newTestServer(t, "")
	f.listErr = errors.New("containerd is down")
	rec := doRequest(t, srv.Handler(), http.MethodGet, snapshotPath, "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "containerd is down") {
		t.Fatalf("error body = %s", rec.Body.String())
	}
}

func TestImageStatus(t *testing.T) {
	srv, _ := newTestServer(t, "")
	rec := doRequest(t, srv.Handler(), http.MethodGet, imagesPath+"ghcr.io/acme/api-gateway:1.14.2", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res ImageResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode image: %v", err)
	}
	if res.APIVersion != WireAPIVersion || res.Image.Name != "ghcr.io/acme/api-gateway:1.14.2" || res.Image.SizeBytes == 0 {
		t.Fatalf("unexpected image payload: %+v", res)
	}

	rec = doRequest(t, srv.Handler(), http.MethodGet, imagesPath+"ghcr.io/acme/missing:9.9.9", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPullImage(t *testing.T) {
	srv, f := newTestServer(t, "")
	rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"registry.internal/new:1.0.0"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var res PullResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode pull result: %v", err)
	}
	if res.APIVersion != WireAPIVersion || res.Image != "registry.internal/new:1.0.0" || res.ImageRef == "" || res.PulledAt.IsZero() {
		t.Fatalf("unexpected pull result: %+v", res)
	}
	if calls := f.pullCalls(); len(calls) != 1 || calls[0] != "registry.internal/new:1.0.0" {
		t.Fatalf("CRI pull calls = %v", calls)
	}
	if _, ok, err := f.ImageStatus(context.Background(), "registry.internal/new:1.0.0"); err != nil || !ok {
		t.Fatalf("pulled image not in CRI cache (ok=%v, err=%v)", ok, err)
	}
}

func TestPullPausedWhenImageFSFull(t *testing.T) {
	srv, f := newTestServer(t, "")
	f.SetImageFS(ImageFSStats{UsedBytes: 80 * giB, CapacityBytes: 100 * giB})
	rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"nginx:latest"}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "paused") {
		t.Fatalf("error body = %s", rec.Body.String())
	}
	if calls := f.pullCalls(); len(calls) != 0 {
		t.Fatalf("CRI was asked to pull while paused: %v", calls)
	}
}

func TestPullValidationAndFailures(t *testing.T) {
	srv, f := newTestServer(t, "")

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "invalid json", body: "{", want: http.StatusBadRequest},
		{name: "empty image", body: `{"image":"  "}`, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, tc.body, "")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	f.pullErr = errors.New("401 Unauthorized")
	rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"registry.internal/private:1"}`, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}

	// Wrong method on the pulls route is rejected by the mux.
	rec = doRequest(t, srv.Handler(), http.MethodGet, pullsPath, "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestServeListenError(t *testing.T) {
	f := newFakeCRI()
	cfg := testConfig()
	cfg.ListenAddr = "127.0.0.1:99999"
	srv, err := NewServer(cfg, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("Serve with an invalid port returned nil error")
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	f := newFakeCRI()
	cfg := testConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	srv, err := NewServer(cfg, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel = %v, want nil", err)
		}
	case <-timeAfter(t, 5):
		t.Fatal("Serve did not shut down within 5s")
	}
}

func TestNewServerRejectsNilCRI(t *testing.T) {
	if _, err := NewServer(testConfig(), nil); err == nil {
		t.Fatal("NewServer(nil CRI) = nil error")
	}
}

func TestPullFailsFastOnDeadRegistry(t *testing.T) {
	stubRegistryDial(t, func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	srv, f := newTestServer(t, "")
	srv.cfg.DisableRegistryProbe = false
	srv.cfg.RegistryProbeTimeout = 50 * time.Millisecond

	started := time.Now()
	rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"192.0.2.1:5000/dead:1"}`, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "registry unreachable") {
		t.Fatalf("error body = %s", rec.Body.String())
	}
	if calls := f.pullCalls(); len(calls) != 0 {
		t.Fatalf("CRI was asked to pull into a dead registry: %v", calls)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("dead registry pull took %s, want it bounded by the probe timeout", elapsed)
	}
}

func TestPullRegistryProbeCanBeDisabled(t *testing.T) {
	stubRegistryDial(t, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("probe must not run")
	})
	srv, f := newTestServer(t, "")
	srv.cfg.DisableRegistryProbe = true

	rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"registry.internal/new:1.0.0"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if calls := f.pullCalls(); len(calls) != 1 {
		t.Fatalf("CRI pull calls = %v", calls)
	}
}

func TestPullStallWatchdogCancelsStalledPull(t *testing.T) {
	srv, f := newTestServer(t, "")
	srv.cfg.PullStallTimeout = 100 * time.Millisecond
	srv.cfg.PullTimeout = 10 * time.Second
	// The pull never finishes on its own and the image filesystem never grows:
	// a registry that accepts connections and then stalls.
	f.pullHook = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	started := time.Now()
	rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"registry.internal/stuck:1.0.0"}`, "")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stalled") {
		t.Fatalf("error body = %s", rec.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("stalled pull was cancelled after %s, want it near the stall window", elapsed)
	}
}

func TestPullStallWatchdogKeepsProgressingPull(t *testing.T) {
	srv, f := newTestServer(t, "")
	srv.cfg.PullStallTimeout = 100 * time.Millisecond
	srv.cfg.PullTimeout = 10 * time.Second
	// The pull takes longer than the stall window but keeps growing the image
	// filesystem, so the watchdog must leave it alone.
	var mu sync.Mutex
	used := int64(1)
	f.fsHook = func(context.Context) (ImageFSStats, error) {
		mu.Lock()
		defer mu.Unlock()
		used += 1 << 20
		return ImageFSStats{UsedBytes: used}, nil
	}
	f.pullHook = func(ctx context.Context, image string) (string, error) {
		select {
		case <-time.After(300 * time.Millisecond):
			return "sha256:" + image, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"registry.internal/slow:1.0.0"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestPullTimeoutWithoutStallWatch(t *testing.T) {
	srv, f := newTestServer(t, "")
	srv.cfg.DisablePullStallWatch = true
	srv.cfg.PullTimeout = 100 * time.Millisecond
	f.pullHook = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"registry.internal/hang:1.0.0"}`, "")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", rec.Code, rec.Body.String())
	}
}
