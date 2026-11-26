package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/metrics"
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
