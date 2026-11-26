package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoleAllows(t *testing.T) {
	tests := []struct {
		role     Role
		required Role
		want     bool
	}{
		{RoleViewer, RoleViewer, true},
		{RoleViewer, RoleOperator, false},
		{RoleOperator, RoleViewer, true},
		{RoleOperator, RoleOperator, true},
		{"", RoleViewer, false},
		{RoleViewer, "", false},
		{"root", RoleViewer, false},
	}
	for _, tc := range tests {
		if got := tc.role.Allows(tc.required); got != tc.want {
			t.Errorf("%q.Allows(%q) = %v, want %v", tc.role, tc.required, got, tc.want)
		}
	}
}

func TestParseRole(t *testing.T) {
	for _, raw := range []string{"viewer", "VIEWER", " operator "} {
		role, err := ParseRole(raw)
		if err != nil || !role.Valid() {
			t.Errorf("ParseRole(%q) = (%q, %v)", raw, role, err)
		}
	}
	if _, err := ParseRole("root"); err == nil {
		t.Fatal("expected unknown role error")
	}
}

func TestRoleFromContext(t *testing.T) {
	if got := RoleFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty role without session, got %q", got)
	}
	ctx := WithSession(context.Background(), Session{Username: "view", Role: RoleViewer})
	if got := RoleFromContext(ctx); got != RoleViewer {
		t.Fatalf("expected viewer role, got %q", got)
	}
	sess, ok := SessionFromContext(ctx)
	if !ok || sess.Username != "view" {
		t.Fatalf("unexpected session: %+v ok=%v", sess, ok)
	}
}

// TestRequireRoleStatusCodes is the core RBAC table: 401 without a session,
// 403 with an insufficient role, 200 when the role is enough.
func TestRequireRoleStatusCodes(t *testing.T) {
	m := newTestManager(t)

	tests := []struct {
		name       string
		middleware AuthedMiddleware
		user       string // "" means no cookie
		tamper     bool
		wantStatus int
		wantError  string
		wantRole   Role
	}{
		{"no session on operator route", RequireOperator(m), "", false, http.StatusUnauthorized, "unauthorized", ""},
		{"viewer on operator route", RequireOperator(m), "view", false, http.StatusForbidden, "forbidden", ""},
		{"operator on operator route", RequireOperator(m), "admin", false, http.StatusOK, "", RoleOperator},
		{"viewer on viewer route", RequireRole(m, RoleViewer), "view", false, http.StatusOK, "", RoleViewer},
		{"operator on viewer route", RequireRole(m, RoleViewer), "admin", false, http.StatusOK, "", RoleOperator},
		{"tampered cookie", RequireOperator(m), "admin", true, http.StatusUnauthorized, "unauthorized", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				gotUser string
				gotRole Role
				called  bool
			)
			h := tc.middleware(func(w http.ResponseWriter, r *http.Request, user string) {
				called = true
				gotUser = user
				gotRole = RoleFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			var cookie *http.Cookie
			if tc.user != "" {
				cookie = issueTestCookie(t, m, tc.user)
				if tc.tamper {
					cookie.Value += "x"
				}
			}
			rec := httptest.NewRecorder()
			h(rec, requestWithCookie(cookie))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusOK {
				if !called || gotUser != tc.user || gotRole != tc.wantRole {
					t.Fatalf("handler got called=%v user=%q role=%q, want %q/%q", called, gotUser, gotRole, tc.user, tc.wantRole)
				}
				return
			}
			if called {
				t.Fatal("handler must not run when authorization fails")
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body.String())
			}
			if body["error"] != tc.wantError {
				t.Fatalf("error = %q, want %q", body["error"], tc.wantError)
			}
		})
	}
}

func TestRequireAuthInjectsSession(t *testing.T) {
	m := newTestManager(t)
	var (
		gotUser string
		gotRole Role
	)
	h := RequireAuth(m, func(w http.ResponseWriter, r *http.Request, user string) {
		gotUser = user
		gotRole = RoleFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	h(rec, requestWithCookie(issueTestCookie(t, m, "view")))
	if rec.Code != http.StatusOK || gotUser != "view" || gotRole != RoleViewer {
		t.Fatalf("got status=%d user=%q role=%q", rec.Code, gotUser, gotRole)
	}
}

func TestRequireRoleAuditsDeniedRequest(t *testing.T) {
	auditor := NewMemoryAuditor(10)
	m := newTestManager(t, WithAuditor(auditor))
	h := RequireOperator(m)(func(http.ResponseWriter, *http.Request, string) {})
	rec := httptest.NewRecorder()
	h(rec, requestWithCookie(issueTestCookie(t, m, "view")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	events := auditor.Recent()
	if len(events) != 1 {
		t.Fatalf("expected 1 audit event, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Action != ActionAuthorize || ev.Result != ResultDenied || ev.Actor != "view" || ev.Role != string(RoleViewer) {
		t.Fatalf("unexpected audit event: %+v", ev)
	}
}
