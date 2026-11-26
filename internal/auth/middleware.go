package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// AuthedHandler is a hub handler that runs only for authenticated sessions.
type AuthedHandler func(w http.ResponseWriter, r *http.Request, user string)

// AuthedMiddleware wraps an authenticated hub handler with an authorization
// check. Build one with RequireRole or RequireOperator.
type AuthedMiddleware func(next AuthedHandler) http.HandlerFunc

// RequireAuth wraps a hub handler with cookie-session authentication.
// Every protected hub route must go through it (AGENTS.md invariant 6).
// The verified session (user + role) is injected into the request context, so
// handlers may use RoleFromContext/SessionFromContext.
func RequireAuth(m *Manager, next AuthedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := m.AuthenticateSession(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r.WithContext(WithSession(r.Context(), sess)), sess.Username)
	}
}

// RequireRole authenticates the request and rejects sessions whose role does
// not satisfy required: 401 without a session, 403 with {"error":"forbidden"}
// when the role is insufficient.
func RequireRole(m *Manager, required Role) AuthedMiddleware {
	return func(next AuthedHandler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			sess, ok := m.AuthenticateSession(r)
			if !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			if !sess.Role.Allows(required) {
				m.Audit(r, ActionAuthorize, required.String(), ResultDenied)
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				return
			}
			next(w, r.WithContext(WithSession(r.Context(), sess)), sess.Username)
		}
	}
}

// RequireOperator is RequireRole with the operator role: only operators may
// change cluster state (pull, workload pull, reconcile, cancel).
func RequireOperator(m *Manager) AuthedMiddleware {
	return RequireRole(m, RoleOperator)
}

// RequireToken wraps a spoke handler with constant-time Bearer token checks.
// An empty token disables the check for local development only (AGENTS.md
// invariant 7).
func RequireToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid cluster token"})
				return
			}
		}
		next(w, r)
	}
}

// writeJSON keeps the middleware package free of a web import cycle. The
// response shape matches internal/web.writeJSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
