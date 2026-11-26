package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/provider"
)

// newRBACServer builds a hub with one operator and one viewer so the RBAC
// wiring in server.go can be exercised end to end.
func newRBACServer(t *testing.T, opts ...auth.Option) (*Server, *auth.Manager) {
	t.Helper()
	users := []auth.User{
		{Name: "admin", Password: "admin-pass", Role: auth.RoleOperator},
		{Name: "viewer", Password: "viewer-pass", Role: auth.RoleViewer},
	}
	opts = append([]auth.Option{auth.WithAuditor(auth.NewMemoryAuditor(500))}, opts...)
	mgr, err := auth.NewManagerFromConfig("rbac-test-secret", users, opts...)
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	reg := provider.NewRegistry()
	reg.Register(provider.ClusterEntry{ID: "talos-hub", Name: "talos-hub", Role: "local", Provider: provider.NewMockProvider("talos-hub")})
	srv, err := NewServer(reg, mgr)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, mgr
}

// login performs POST /api/v1/auth/login and returns the cookie and decoded body.
func login(t *testing.T, srv *Server, username, password string) (*http.Cookie, map[string]any, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	var cookie *http.Cookie
	if cookies := rec.Result().Cookies(); len(cookies) > 0 {
		cookie = cookies[0]
	}
	return cookie, decoded, rec.Code
}

func doAuthed(srv *Server, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestHubRBACWiring(t *testing.T) {
	srv, _ := newRBACServer(t)

	// Login returns the role so the UI can hide operator controls.
	opCookie, opBody, code := login(t, srv, "admin", "admin-pass")
	if code != http.StatusOK {
		t.Fatalf("operator login = %d: %v", code, opBody)
	}
	if got, _ := opBody["role"].(string); got != string(auth.RoleOperator) {
		t.Fatalf("operator login role = %v, want %q", opBody["role"], auth.RoleOperator)
	}
	viewCookie, viewBody, code := login(t, srv, "viewer", "viewer-pass")
	if code != http.StatusOK {
		t.Fatalf("viewer login = %d: %v", code, viewBody)
	}
	if got, _ := viewBody["role"].(string); got != string(auth.RoleViewer) {
		t.Fatalf("viewer login role = %v, want %q", viewBody["role"], auth.RoleViewer)
	}

	// /auth/me reports the role as well.
	rec := doAuthed(srv, http.MethodGet, "/api/v1/auth/me", "", viewCookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"role":"viewer"`) {
		t.Fatalf("viewer /auth/me = %d: %s", rec.Code, rec.Body.String())
	}

	pullBody := "{\"cluster\":\"talos-hub\",\"image\":\"ghcr.io/acme/api-gateway:1.14.2\",\"node\":\"talos-worker-01\"}"

	// Anonymous is rejected before any role check.
	if rec := doAuthed(srv, http.MethodPost, "/api/v1/pull", pullBody, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous pull = %d, want 401", rec.Code)
	}

	// Viewer reads are allowed, state changes are not.
	if rec := doAuthed(srv, http.MethodGet, "/api/v1/clusters", "", viewCookie); rec.Code != http.StatusOK {
		t.Fatalf("viewer clusters = %d, want 200", rec.Code)
	}
	for _, path := range []string{"/api/v1/pull", "/api/v1/workloads/pull", "/api/v1/reconcile", "/api/v1/pulls/task-1/cancel"} {
		rec := doAuthed(srv, http.MethodPost, path, pullBody, viewCookie)
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer POST %s = %d (%s), want 403", path, rec.Code, rec.Body.String())
		}
	}

	// Operator may trigger a pull; the audit feed records it.
	if rec := doAuthed(srv, http.MethodPost, "/api/v1/pull", pullBody, opCookie); rec.Code != http.StatusAccepted {
		t.Fatalf("operator pull = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAuthed(srv, http.MethodGet, "/api/v1/audit", "", viewCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer audit = %d, want 403", rec.Code)
	}
	rec = doAuthed(srv, http.MethodGet, "/api/v1/audit", "", opCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator audit = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"action":"pull"`) {
		t.Fatalf("audit feed has no pull event: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "admin-pass") {
		t.Fatalf("audit feed contains a password: %s", rec.Body.String())
	}
}

func TestHubLoginRateLimitWiring(t *testing.T) {
	srv, _ := newRBACServer(t, auth.WithRateLimit(auth.RateLimitConfig{
		AttemptsPerMinute: 2,
		BlockDuration:     time.Minute,
		CleanupInterval:   -1, // no background ticker in tests
	}))

	for i := 0; i < 2; i++ {
		if _, body, code := login(t, srv, "admin", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d (%v), want 401", i+1, code, body)
		}
	}
	rec := httptest.NewRecorder()
	body := bytes.NewReader([]byte("{\"username\":\"admin\",\"password\":\"wrong\"}"))
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", body))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third failed login = %d (%s), want 429", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 without Retry-After header")
	}

	// The limiter is per IP and per username: a correct password from a different
	// IP for another account is unaffected by the block on admin.
	body = bytes.NewReader([]byte("{\"username\":\"viewer\",\"password\":\"viewer-pass\"}"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", body)
	req.RemoteAddr = "192.0.2.99:1234"
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer login from another IP = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}
