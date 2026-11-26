package auth

import (
	"context"
	"fmt"
	"strings"
)

// Role is the authorization level carried by a session.
type Role string

const (
	// RoleViewer may read the UI/API but may not change cluster state.
	RoleViewer Role = "viewer"
	// RoleOperator may trigger pulls, reconciles and cancels in addition to reads.
	RoleOperator Role = "operator"
)

// ParseRole validates a role name.
func ParseRole(s string) (Role, error) {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case RoleViewer:
		return RoleViewer, nil
	case RoleOperator:
		return RoleOperator, nil
	default:
		return "", fmt.Errorf("unknown role %q (want %q or %q)", s, RoleViewer, RoleOperator)
	}
}

func (r Role) String() string { return string(r) }

// Valid reports whether the role is one of the known roles.
func (r Role) Valid() bool { return r.level() > 0 }

func (r Role) level() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	default:
		return 0
	}
}

// Allows reports whether r satisfies required. Unknown roles allow nothing.
func (r Role) Allows(required Role) bool {
	if !r.Valid() || !required.Valid() {
		return false
	}
	return r.level() >= required.level()
}

type sessionContextKey struct{}

// WithSession stores the authenticated session in the request context.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, s)
}

// SessionFromContext returns the session injected by RequireAuth/RequireRole.
func SessionFromContext(ctx context.Context) (Session, bool) {
	if ctx == nil {
		return Session{}, false
	}
	s, ok := ctx.Value(sessionContextKey{}).(Session)
	return s, ok
}

// RoleFromContext returns the role of the authenticated session, or an empty
// Role when the request was not authenticated. Handlers registered through
// RequireAuth/RequireRole always see a valid role.
func RoleFromContext(ctx context.Context) Role {
	s, ok := SessionFromContext(ctx)
	if !ok {
		return ""
	}
	return s.Role
}
