package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthCookieFlow(t *testing.T) {
	mgr, err := NewManager(Config{
		Username:      "admin",
		Password:      "s3cr3t",
		SessionSecret: []byte("test-secret-key"),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !mgr.ValidateCredentials("admin", "s3cr3t") {
		t.Fatal("expected valid credentials")
	}
	if mgr.ValidateCredentials("admin", "wrong") {
		t.Fatal("expected invalid credentials")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	rec := httptest.NewRecorder()
	mgr.IssueCookie(rec, req, "admin")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName {
		t.Fatalf("expected %s cookie, got %+v", CookieName, cookies)
	}

	authedReq := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	authedReq.AddCookie(cookies[0])
	user, ok := mgr.AuthenticateRequest(authedReq)
	if !ok || user != "admin" {
		t.Fatalf("expected authenticated admin, got (%q, %v)", user, ok)
	}

	tampered := *cookies[0]
	tampered.Value += "x"
	badReq := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	badReq.AddCookie(&tampered)
	if _, ok := mgr.AuthenticateRequest(badReq); ok {
		t.Fatal("expected tampered cookie to fail authentication")
	}
}
