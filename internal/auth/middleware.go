package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"encoding/json"
)

// AuthedHandler is a hub handler that runs only for authenticated sessions.
type AuthedHandler func(w http.ResponseWriter, r *http.Request, user string)

// RequireAuth wraps a hub handler with cookie-session authentication.
// Every protected hub route must go through it (AGENTS.md invariant 6).
func RequireAuth(m *Manager, next AuthedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := m.AuthenticateRequest(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r, user)
	}
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
