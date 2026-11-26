package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/metrics"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

func TestServerAuthMultiClusterAndMetrics(t *testing.T) {
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
	reg.Register(provider.ClusterEntry{ID: "talos-kz", Name: "talos-kz", Role: "remote", Provider: provider.NewMockProvider("talos-kz")})

	srv, err := NewServer(reg, mgr)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Unauthenticated snapshot must return 401
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// Login
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "pass123"})
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	if loginRec.Code != http.StatusOK {
		t.Fatalf("expected 200 login, got %d", loginRec.Code)
	}
	cookie := loginRec.Result().Cookies()[0]

	// List clusters
	clReq := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	clReq.AddCookie(cookie)
	clRec := httptest.NewRecorder()
	srv.ServeHTTP(clRec, clReq)
	if clRec.Code != http.StatusOK || !strings.Contains(clRec.Body.String(), "talos-kz") {
		t.Fatalf("expected clusters list with talos-kz, got %d: %s", clRec.Code, clRec.Body.String())
	}

	// Authenticated snapshot for remote cluster
	snapReq := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?cluster=talos-kz", nil)
	snapReq.AddCookie(cookie)
	snapRec := httptest.NewRecorder()
	srv.ServeHTTP(snapRec, snapReq)
	if snapRec.Code != http.StatusOK || !strings.Contains(snapRec.Body.String(), "talos-worker-01") {
		t.Fatalf("expected 200 snapshot for talos-kz, got %d", snapRec.Code)
	}

	// Spoke headless API auth check
	spoke := NewSpokeServer(provider.NewMockProvider("talos-kz"), "spoke-secret")
	unauthSpoke := httptest.NewRecorder()
	spoke.ServeHTTP(unauthSpoke, httptest.NewRequest(http.MethodGet, "/internal/v1/snapshot", nil))
	if unauthSpoke.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for spoke without token, got %d", unauthSpoke.Code)
	}
	authSpokeReq := httptest.NewRequest(http.MethodGet, "/internal/v1/snapshot", nil)
	authSpokeReq.Header.Set("Authorization", "Bearer spoke-secret")
	authSpokeRec := httptest.NewRecorder()
	spoke.ServeHTTP(authSpokeRec, authSpokeReq)
	if authSpokeRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for spoke with valid token, got %d", authSpokeRec.Code)
	}

	// Prometheus metrics with cluster label
	mRec := httptest.NewRecorder()
	metrics.Handler(reg).ServeHTTP(mRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if mRec.Code != http.StatusOK {
		t.Fatalf("expected 200 metrics, got %d", mRec.Code)
	}
	mBody := mRec.Body.String()
	for _, want := range []string{
		`prepuller_cluster_up{cluster="talos-hub"`,
		`prepuller_cluster_up{cluster="talos-kz"`,
		`prepuller_image_present{cluster="talos-kz"`,
	} {
		if !strings.Contains(mBody, want) {
			t.Fatalf("expected %q in /metrics output", want)
		}
	}
}
func TestSpokeContractV2Endpoints(t *testing.T) {
	p := provider.NewMockProvider("talos-kz")
	spoke := NewSpokeServer(p, "spoke-secret")

	authed := func(method, path string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer spoke-secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		spoke.ServeHTTP(rec, req)
		return rec
	}

	// Counters endpoint feeds prepuller_pull_total for remote clusters.
	rec := authed(http.MethodGet, "/internal/v1/counters", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "api-gateway") {
		t.Fatalf("counters = %d: %s", rec.Code, rec.Body.String())
	}

	// Pull returns the created task instead of the v1 {"ok":true}.
	rec = authed(http.MethodPost, "/internal/v1/pull", `{"apiVersion":"v2","image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01","actor":"admin","trigger":"manual"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pull = %d: %s", rec.Code, rec.Body.String())
	}
	var task struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.ID == "" || task.State != "running" {
		t.Fatalf("pull returned %+v, want running task with id", task)
	}

	// Cancel the task.
	rec = authed(http.MethodPost, "/internal/v1/pulls/"+task.ID+"/cancel", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}

	// Unknown apiVersion is rejected during the rollout window.
	rec = authed(http.MethodPost, "/internal/v1/pull", `{"apiVersion":"v9","image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported apiVersion = %d, want 400", rec.Code)
	}

	// Previous contract version is still accepted.
	rec = authed(http.MethodPost, "/internal/v1/reconcile", `{"apiVersion":"v1","actor":"admin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("v1 reconcile = %d: %s", rec.Code, rec.Body.String())
	}
}

// errProvider fails TriggerPull with a fixed error so the HTTP layer mapping can
// be exercised without a live Kubernetes cluster. The embedded Provider is nil:
// only TriggerPull is called by these tests.
type errProvider struct {
	provider.Provider
	err error
}

func (p errProvider) TriggerPull(context.Context, model.PullRequest) (model.PullTask, error) {
	return model.PullTask{}, p.err
}

func TestPullErrorStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusBadRequest},
		{fmt.Errorf("pull: %w", provider.ErrTaskNotFound), http.StatusNotFound},
		{fmt.Errorf("pull: %w", provider.ErrCancelNotSupported), http.StatusConflict},
		{fmt.Errorf("pull: %w", provider.ErrAgentDiskPaused), http.StatusConflict},
		{fmt.Errorf("pull: %w", provider.ErrAgentUnauthorized), http.StatusBadGateway},
		{fmt.Errorf("pull: %w", provider.ErrAgentUnavailable), http.StatusBadGateway},
		{provider.ErrNotImplemented, http.StatusNotImplemented},
		{errors.New("image and node are required"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		if got := pullErrorStatus(tc.err); got != tc.want {
			t.Errorf("pullErrorStatus(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

// TestSpokePullErrorStatuses checks the wire statuses the hub/UI see for a node
// whose ImageFS is above the pause threshold (409) and for an unreachable agent
// (502) instead of a blanket 400.
func TestSpokePullErrorStatuses(t *testing.T) {
	body := `{"apiVersion":"v2","image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01"}`
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("agent: %w", provider.ErrAgentDiskPaused), http.StatusConflict},
		{fmt.Errorf("agent: %w", provider.ErrAgentUnavailable), http.StatusBadGateway},
		{fmt.Errorf("agent: %w", provider.ErrAgentUnauthorized), http.StatusBadGateway},
	}
	for _, tc := range cases {
		mux := NewSpokeServer(errProvider{err: tc.err}, "")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/pull", strings.NewReader(body)))
		if rec.Code != tc.want {
			t.Errorf("pull with %v = %d (%s), want %d", tc.err, rec.Code, rec.Body.String(), tc.want)
		}
	}
}
