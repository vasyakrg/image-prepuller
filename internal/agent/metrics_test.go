package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// metricsBody renders the agent metrics handler and returns the exposition.
func metricsBody(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// sampleValue returns the value line of one series, failing when it is absent.
func sampleValue(t *testing.T, body, series string) string {
	t.Helper()
	prefix := series + " "
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("sample %q not found in:\n%s", series, body)
	return ""
}

// wantSample asserts one series has exactly the given value.
func wantSample(t *testing.T, body, series, want string) {
	t.Helper()
	if got := sampleValue(t, body, series); got != want {
		t.Fatalf("%s = %q, want %q\n%s", series, got, want, body)
	}
}

// wantNoSample asserts a series is absent.
func wantNoSample(t *testing.T, body, series string) {
	t.Helper()
	prefix := series + " "
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			t.Fatalf("unexpected sample %q", line)
		}
	}
}

// wantFamily asserts a family carries both HELP and TYPE lines.
func wantFamily(t *testing.T, body, name, typ string) {
	t.Helper()
	if !strings.Contains(body, "# HELP "+name+" ") {
		t.Fatalf("missing HELP for %s", name)
	}
	if !strings.Contains(body, "# TYPE "+name+" "+typ+"\n") {
		t.Fatalf("missing TYPE %s %s", name, typ)
	}
}

func TestMetricsExpositionFormat(t *testing.T) {
	m := NewMetrics("node-a", "1.2.3")
	body := metricsBody(t, m.Handler())

	families := []struct {
		name string
		typ  string
	}{
		{"prepuller_agent_up", "gauge"},
		{"prepuller_agent_build_info", "gauge"},
		{"prepuller_agent_start_time_seconds", "gauge"},
		{"prepuller_agent_last_contact_timestamp_seconds", "gauge"},
		{"prepuller_agent_requests_total", "counter"},
		{"prepuller_agent_cri_up", "gauge"},
		{"prepuller_agent_pull_in_flight", "gauge"},
		{"prepuller_agent_pull_total", "counter"},
		{"prepuller_agent_imagefs_used_bytes", "gauge"},
		{"prepuller_agent_imagefs_capacity_bytes", "gauge"},
		{"prepuller_agent_images_cached_total", "gauge"},
	}
	last := -1
	for _, family := range families {
		wantFamily(t, body, family.name, family.typ)
		idx := strings.Index(body, "# HELP "+family.name+" ")
		if idx <= last {
			t.Fatalf("family %s is out of order", family.name)
		}
		last = idx
	}

	if !strings.HasSuffix(body, "\n") {
		t.Fatal("exposition does not end with a newline")
	}
	if strings.HasSuffix(body, "\n\n") {
		t.Fatal("exposition ends with a blank line")
	}
	if strings.Contains(body, "\n\n") {
		t.Fatalf("exposition contains blank lines:\n%s", body)
	}

	// Dead-man gauge and build info.
	wantSample(t, body, `prepuller_agent_up{node="node-a"}`, "1")
	buildSeries := fmt.Sprintf(`prepuller_agent_build_info{node="node-a",version="1.2.3",go_version="%s"}`, runtime.Version())
	wantSample(t, body, buildSeries, "1")

	// Families without samples still carry HELP/TYPE: no requests yet.
	wantNoSample(t, body, `prepuller_agent_requests_total{node="node-a",endpoint="healthz",code="200"}`)

	start := m.startTime.Unix()
	for _, endpoint := range lastContactEndpoints {
		series := fmt.Sprintf(`prepuller_agent_last_contact_timestamp_seconds{node="node-a",endpoint="%s"}`, endpoint)
		wantSample(t, body, series, fmt.Sprint(start))
	}

	// All known pull results are emitted from boot with zero.
	for _, result := range pullResults {
		series := fmt.Sprintf(`prepuller_agent_pull_total{node="node-a",result="%s"}`, result)
		wantSample(t, body, series, "0")
	}

	// Same state renders identically.
	if again := metricsBody(t, m.Handler()); again != body {
		t.Fatalf("metrics output is not stable:\n--- first\n%s\n--- second\n%s", body, again)
	}
}

func TestMetricsObserveRequest(t *testing.T) {
	m := NewMetrics("node-b", "2.0.0")
	m.ObserveRequest(endpointHealthz, http.StatusOK)
	m.ObserveRequest(endpointHealthz, http.StatusOK)
	m.ObserveRequest(endpointSnapshot, http.StatusUnauthorized)
	m.ObserveRequest(endpointImages, http.StatusNotFound)
	m.ObserveRequest(endpointPulls, http.StatusInternalServerError)
	m.ObserveRequest(endpointOther, http.StatusMethodNotAllowed)
	m.ObserveRequest("unexpected", http.StatusTeapot)

	body := metricsBody(t, m.Handler())
	for _, tc := range []struct {
		endpoint string
		code     int
		count    string
	}{
		{endpointHealthz, http.StatusOK, "2"},
		{endpointSnapshot, http.StatusUnauthorized, "1"},
		{endpointImages, http.StatusNotFound, "1"},
		{endpointPulls, http.StatusInternalServerError, "1"},
		{endpointOther, http.StatusMethodNotAllowed, "1"},
		{"unexpected", http.StatusTeapot, "1"},
	} {
		series := fmt.Sprintf(`prepuller_agent_requests_total{node="node-b",endpoint="%s",code="%d"}`, tc.endpoint, tc.code)
		wantSample(t, body, series, tc.count)
	}

	// Fixed endpoints keep a stable order; unexpected ones sort after them.
	prev := -1
	for _, endpoint := range append(append([]string(nil), requestEndpoints...), "unexpected") {
		idx := strings.Index(body, fmt.Sprintf(`prepuller_agent_requests_total{node="node-b",endpoint="%s",`, endpoint))
		if idx <= prev {
			t.Fatalf("endpoint %s is out of order", endpoint)
		}
		prev = idx
	}

	// Other endpoints get counters but no last-contact timestamp.
	wantNoSample(t, body, `prepuller_agent_last_contact_timestamp_seconds{node="node-b",endpoint="other"}`)
	wantNoSample(t, body, `prepuller_agent_last_contact_timestamp_seconds{node="node-b",endpoint="unexpected"}`)
}

func TestMetricsLastContactInitializedAndAdvances(t *testing.T) {
	m := NewMetrics("node-c", "3.0.0")
	start := m.startTime

	body := metricsBody(t, m.Handler())
	for _, endpoint := range lastContactEndpoints {
		series := fmt.Sprintf(`prepuller_agent_last_contact_timestamp_seconds{node="node-c",endpoint="%s"}`, endpoint)
		wantSample(t, body, series, fmt.Sprint(start.Unix()))
	}

	// Move the clock past the start so the advance is unambiguous.
	m.now = func() time.Time { return start.Add(42 * time.Second) }
	m.ObserveRequest(endpointSnapshot, http.StatusOK)
	m.ObserveRequest(endpointOther, http.StatusNotFound)

	body = metricsBody(t, m.Handler())
	wantSample(t, body,
		`prepuller_agent_last_contact_timestamp_seconds{node="node-c",endpoint="snapshot"}`,
		fmt.Sprint(start.Add(42*time.Second).Unix()))
	wantSample(t, body,
		`prepuller_agent_last_contact_timestamp_seconds{node="node-c",endpoint="healthz"}`,
		fmt.Sprint(start.Unix()))
	wantNoSample(t, body, `prepuller_agent_last_contact_timestamp_seconds{node="node-c",endpoint="other"}`)
}

func TestMetricsSnapshotGauges(t *testing.T) {
	m := NewMetrics("node-d", "4.0.0")
	body := metricsBody(t, m.Handler())
	wantSample(t, body, `prepuller_agent_cri_up{node="node-d"}`, "0")

	m.ObserveSnapshot(true, 7, 1000, 2000)
	body = metricsBody(t, m.Handler())
	wantSample(t, body, `prepuller_agent_cri_up{node="node-d"}`, "1")
	wantSample(t, body, `prepuller_agent_images_cached_total{node="node-d"}`, "7")
	wantSample(t, body, `prepuller_agent_imagefs_used_bytes{node="node-d"}`, "1000")
	wantSample(t, body, `prepuller_agent_imagefs_capacity_bytes{node="node-d"}`, "2000")

	// A failed snapshot flips cri_up but keeps the last known filesystem and
	// cache gauges instead of pretending the node is empty.
	m.ObserveSnapshot(false, 0, 0, 0)
	body = metricsBody(t, m.Handler())
	wantSample(t, body, `prepuller_agent_cri_up{node="node-d"}`, "0")
	wantSample(t, body, `prepuller_agent_images_cached_total{node="node-d"}`, "7")
	wantSample(t, body, `prepuller_agent_imagefs_used_bytes{node="node-d"}`, "1000")

	m.ObserveSnapshot(true, 9, 3000, 4000)
	body = metricsBody(t, m.Handler())
	wantSample(t, body, `prepuller_agent_cri_up{node="node-d"}`, "1")
	wantSample(t, body, `prepuller_agent_images_cached_total{node="node-d"}`, "9")
	wantSample(t, body, `prepuller_agent_imagefs_used_bytes{node="node-d"}`, "3000")
	wantSample(t, body, `prepuller_agent_imagefs_capacity_bytes{node="node-d"}`, "4000")
}

func TestMetricsPullCounters(t *testing.T) {
	m := NewMetrics("node-e", "5.0.0")
	m.PullStarted()
	m.PullStarted()
	body := metricsBody(t, m.Handler())
	wantSample(t, body, `prepuller_agent_pull_in_flight{node="node-e"}`, "2")

	m.PullFinished("success")
	body = metricsBody(t, m.Handler())
	wantSample(t, body, `prepuller_agent_pull_in_flight{node="node-e"}`, "1")
	wantSample(t, body, `prepuller_agent_pull_total{node="node-e",result="success"}`, "1")

	m.PullFinished("cri_error")
	m.PullFinished("stalled")
	m.PullFinished("registry_unreachable")
	m.PullFinished("registry_auth")
	m.PullFinished("disk_paused")
	body = metricsBody(t, m.Handler())
	wantSample(t, body, `prepuller_agent_pull_in_flight{node="node-e"}`, "0")
	for _, result := range pullResults {
		wantSample(t, body, fmt.Sprintf(`prepuller_agent_pull_total{node="node-e",result="%s"}`, result), "1")
	}

	// An unmatched finish must never drive the in-flight gauge negative.
	m.PullFinished("cri_error")
	body = metricsBody(t, m.Handler())
	wantSample(t, body, `prepuller_agent_pull_in_flight{node="node-e"}`, "0")
	wantSample(t, body, `prepuller_agent_pull_total{node="node-e",result="cri_error"}`, "2")
}

func TestMetricsHandlerMethods(t *testing.T) {
	m := NewMetrics("node-f", "6.0.0")
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		req := httptest.NewRequest(method, "/metrics", nil)
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /metrics = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("Allow = %q, want GET", allow)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestEndpointForPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{healthzPath, endpointHealthz},
		{healthzPath + "/", endpointOther},
		{snapshotPath, endpointSnapshot},
		{imagesPath + "ghcr.io/acme/api-gateway:1.14.2", endpointImages},
		{imagesPath, endpointImages},
		{pullsPath, endpointPulls},
		{pullsPath + "/", endpointOther},
		{"/nope", endpointOther},
	} {
		if got := endpointForPath(tc.path); got != tc.want {
			t.Fatalf("endpointForPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestServerCountsRequestsThroughAuth(t *testing.T) {
	srv, _ := newTestServer(t, "secret")

	doRequest(t, srv.Handler(), http.MethodGet, healthzPath, "", "")
	doRequest(t, srv.Handler(), http.MethodGet, snapshotPath, "", "")
	doRequest(t, srv.Handler(), http.MethodGet, snapshotPath, "", "secret")
	doRequest(t, srv.Handler(), http.MethodGet, imagesPath+"missing:1", "", "secret")
	doRequest(t, srv.Handler(), http.MethodGet, pullsPath, "", "")
	doRequest(t, srv.Handler(), http.MethodGet, "/nope", "", "")

	body := metricsBody(t, srv.MetricsHandler())
	node := `node="talos-worker-01"`
	for _, tc := range []struct {
		endpoint string
		code     int
	}{
		{endpointHealthz, http.StatusOK},
		{endpointSnapshot, http.StatusUnauthorized},
		{endpointSnapshot, http.StatusOK},
		{endpointImages, http.StatusNotFound},
		{endpointPulls, http.StatusMethodNotAllowed},
		{endpointOther, http.StatusNotFound},
	} {
		series := fmt.Sprintf("prepuller_agent_requests_total{%s,endpoint=%q,code=%q}", node, tc.endpoint, fmt.Sprint(tc.code))
		wantSample(t, body, series, "1")
	}

	// Every endpoint has a last-contact series, including the never-called one.
	for _, endpoint := range lastContactEndpoints {
		series := fmt.Sprintf("prepuller_agent_last_contact_timestamp_seconds{%s,endpoint=%q}", node, endpoint)
		if got := sampleValue(t, body, series); got == "" {
			t.Fatalf("empty last contact for %s", endpoint)
		}
	}
}

func TestServerSnapshotMetrics(t *testing.T) {
	srv, f := newTestServer(t, "")
	body := metricsBody(t, srv.MetricsHandler())
	wantSample(t, body, `prepuller_agent_cri_up{node="talos-worker-01"}`, "0")

	rec := doRequest(t, srv.Handler(), http.MethodGet, snapshotPath, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d: %s", rec.Code, rec.Body.String())
	}
	body = metricsBody(t, srv.MetricsHandler())
	wantSample(t, body, `prepuller_agent_cri_up{node="talos-worker-01"}`, "1")
	wantSample(t, body, `prepuller_agent_images_cached_total{node="talos-worker-01"}`, "4")
	wantSample(t, body, `prepuller_agent_imagefs_used_bytes{node="talos-worker-01"}`, fmt.Sprint(58*giB))
	wantSample(t, body, `prepuller_agent_imagefs_capacity_bytes{node="talos-worker-01"}`, fmt.Sprint(100*giB))

	f.listErr = errors.New("containerd is down")
	rec = doRequest(t, srv.Handler(), http.MethodGet, snapshotPath, "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed snapshot status = %d", rec.Code)
	}
	body = metricsBody(t, srv.MetricsHandler())
	wantSample(t, body, `prepuller_agent_cri_up{node="talos-worker-01"}`, "0")
	wantSample(t, body, `prepuller_agent_images_cached_total{node="talos-worker-01"}`, "4")
}

func TestServerPullMetrics(t *testing.T) {
	node := `node="talos-worker-01"`
	pullSeries := func(result string) string {
		return fmt.Sprintf("prepuller_agent_pull_total{%s,result=%q}", node, result)
	}

	t.Run("success", func(t *testing.T) {
		srv, _ := newTestServer(t, "")
		rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"registry.internal/new:1.0.0"}`, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		body := metricsBody(t, srv.MetricsHandler())
		wantSample(t, body, pullSeries("success"), "1")
		wantSample(t, body, fmt.Sprintf("prepuller_agent_pull_in_flight{%s}", node), "0")
	})

	t.Run("disk paused", func(t *testing.T) {
		srv, f := newTestServer(t, "")
		f.SetImageFS(ImageFSStats{UsedBytes: 80 * giB, CapacityBytes: 100 * giB})
		rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"nginx:latest"}`, "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		body := metricsBody(t, srv.MetricsHandler())
		wantSample(t, body, pullSeries("disk_paused"), "1")
		wantSample(t, body, fmt.Sprintf("prepuller_agent_pull_in_flight{%s}", node), "0")
	})

	t.Run("registry unreachable", func(t *testing.T) {
		stubRegistryDial(t, func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		srv, _ := newTestServer(t, "")
		srv.cfg.DisableRegistryProbe = false
		srv.cfg.RegistryProbeTimeout = 50 * time.Millisecond
		rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"192.0.2.1:5000/dead:1"}`, "")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		body := metricsBody(t, srv.MetricsHandler())
		wantSample(t, body, pullSeries("registry_unreachable"), "1")
		wantSample(t, body, fmt.Sprintf("prepuller_agent_pull_in_flight{%s}", node), "0")
	})

	t.Run("cri error", func(t *testing.T) {
		srv, f := newTestServer(t, "")
		f.pullErr = errors.New("invalid image reference")
		rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"registry.internal/private:1"}`, "")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		body := metricsBody(t, srv.MetricsHandler())
		wantSample(t, body, pullSeries("cri_error"), "1")
		wantSample(t, body, fmt.Sprintf("prepuller_agent_pull_in_flight{%s}", node), "0")
	})

	t.Run("registry auth", func(t *testing.T) {
		srv, f := newTestServer(t, "")
		f.pullErr = errors.New("failed to fetch anonymous token: 403 Forbidden")
		rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"hub.example/tools/app:1"}`, "")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		body := metricsBody(t, srv.MetricsHandler())
		wantSample(t, body, pullSeries("registry_auth"), "1")
		wantSample(t, body, fmt.Sprintf("prepuller_agent_pull_in_flight{%s}", node), "0")
	})

	t.Run("stalled", func(t *testing.T) {
		srv, f := newTestServer(t, "")
		srv.cfg.PullStallTimeout = 100 * time.Millisecond
		srv.cfg.PullTimeout = 10 * time.Second
		f.pullHook = func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}
		rec := doRequest(t, srv.Handler(), http.MethodPost, pullsPath, `{"image":"registry.internal/stuck:1.0.0"}`, "")
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		body := metricsBody(t, srv.MetricsHandler())
		wantSample(t, body, pullSeries("stalled"), "1")
		wantSample(t, body, fmt.Sprintf("prepuller_agent_pull_in_flight{%s}", node), "0")
	})
}

func TestServerPullInFlightGauge(t *testing.T) {
	srv, f := newTestServer(t, "")
	release := make(chan struct{})
	f.pullHook = func(context.Context, string) (string, error) {
		<-release
		return "sha256:in-flight", nil
	}

	recCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, pullsPath, strings.NewReader(`{"image":"registry.internal/slow:1.0.0"}`))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		recCh <- rec
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		body := metricsBody(t, srv.MetricsHandler())
		if sampleValue(t, body, `prepuller_agent_pull_in_flight{node="talos-worker-01"}`) == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pull_in_flight never reached 1:\n%s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(release)
	rec := <-recCh
	if rec.Code != http.StatusOK {
		t.Fatalf("pull status = %d: %s", rec.Code, rec.Body.String())
	}
	body := metricsBody(t, srv.MetricsHandler())
	wantSample(t, body, `prepuller_agent_pull_in_flight{node="talos-worker-01"}`, "0")
	wantSample(t, body, `prepuller_agent_pull_total{node="talos-worker-01",result="success"}`, "1")
}

// freeTCPAddr reserves and releases one loopback port. The short window is the
// usual price of testing a real listener; the test only needs the port to be
// free for this process.
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

func TestServeMetricsEndpoint(t *testing.T) {
	f := newFakeCRI()
	cfg := testConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.MetricsAddr = freeTCPAddr(t)
	srv, err := NewServer(cfg, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	url := "http://" + cfg.MetricsAddr + "/metrics"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	var body string
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			bodyBytes := make([]byte, 4096)
			n, _ := resp.Body.Read(bodyBytes)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d", url, resp.StatusCode)
			}
			body = string(bodyBytes[:n])
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics listener on %s did not answer: %v", cfg.MetricsAddr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, `prepuller_agent_up{node="talos-worker-01"} 1`) {
		t.Fatalf("metrics endpoint body does not carry prepuller_agent_up:\n%s", body)
	}
	if !strings.Contains(body, "# TYPE prepuller_agent_pull_total counter") {
		t.Fatalf("metrics endpoint body lacks family headers:\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not shut down within 5s")
	}
}

func TestServeMetricsListenError(t *testing.T) {
	f := newFakeCRI()
	cfg := testConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.MetricsAddr = "127.0.0.1:99999"
	srv, err := NewServer(cfg, f)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("Serve with an invalid metrics port returned nil error")
	}
}

func TestServeWithoutMetricsAddr(t *testing.T) {
	f := newFakeCRI()
	cfg := testConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.MetricsAddr = ""
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
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not shut down within 5s")
	}
}
