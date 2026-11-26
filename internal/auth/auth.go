package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const CookieName = "prepuller_session"

// Session token layouts. v1 is the legacy single-user format without a role:
//
//	base64(username)|exp.sig
//
// v2 adds the role:
//
//	v2|base64(username)|role|exp.sig
//
// Both are accepted: v1 cookies are still valid for one release and map to the
// operator role (AGENTS.md backward-compatibility rule).
const (
	tokenVersionV2 = "v2"
	tokenFieldsV1  = 2
	tokenFieldsV2  = 4
)

// Input bounds keep hashing, cookie size and memory use predictable, and stop
// oversized credentials from reaching the constant-time compare.
const (
	maxUsernameLen = 256
	maxPasswordLen = 1024
	// dummyPassword is compared against when a user does not exist so that
	// "no such user" and "wrong password" cost the same time.
	dummyPassword = "prepuller-invalid-user-dummy-password"
)

// Config is the single-user configuration kept for backward compatibility.
// New code should use NewManagerFromConfig with an explicit User list.
type Config struct {
	Username      string
	Password      string
	SessionSecret []byte
	TTL           time.Duration
	// Users, when non-empty, replaces Username/Password with a multi-user set.
	Users []User
	// SecureCookie forces the Secure cookie attribute when TLS terminates in
	// front of the hub. When false it is derived from TLS/X-Forwarded-Proto.
	SecureCookie bool
	// Auditor receives login/authorization/action events. Nil disables audit.
	Auditor Auditor
}

// Manager issues and verifies HMAC-signed session cookies and authorizes them.
type Manager struct {
	users        map[string]User
	secret       []byte
	ttl          time.Duration
	secureCookie bool
	now          func() time.Time
	auditor      Auditor
	limiter      *RateLimiter
}

// Session is the authenticated identity carried by the session cookie.
type Session struct {
	Username string
	Role     Role
	Expires  time.Time
}

// Option customises a Manager created by NewManagerFromConfig.
type Option func(*Manager)

// WithTTL overrides the session lifetime (default 24h).
func WithTTL(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.ttl = d
		}
	}
}

// WithSecureCookie forces the Secure cookie attribute on (TLS terminates in
// front of the hub). Without it Secure is derived from the request.
func WithSecureCookie(secure bool) Option {
	return func(m *Manager) { m.secureCookie = secure }
}

// WithAuditor attaches an audit sink.
func WithAuditor(a Auditor) Option {
	return func(m *Manager) { m.auditor = a }
}

// WithNow injects the clock used for cookie expiry and session verification.
func WithNow(now func() time.Time) Option {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

// WithRateLimit enables login rate limiting with the given configuration.
func WithRateLimit(cfg RateLimitConfig) Option {
	return func(m *Manager) { m.limiter = NewRateLimiter(cfg) }
}

// WithRateLimiter attaches an existing login rate limiter. The Manager takes
// ownership: Close stops it.
func WithRateLimiter(rl *RateLimiter) Option {
	return func(m *Manager) { m.limiter = rl }
}

// NewManagerFromConfig builds a multi-user Manager. Users must be non-empty;
// an empty role defaults to operator for backward compatibility.
func NewManagerFromConfig(secret string, users []User, opts ...Option) (*Manager, error) {
	m := &Manager{
		users: make(map[string]User, len(users)),
		ttl:   24 * time.Hour,
		now:   time.Now,
	}
	if len(users) == 0 {
		return nil, errors.New("at least one user must be configured")
	}
	for _, u := range users {
		name := strings.TrimSpace(u.Name)
		if name == "" {
			return nil, errors.New("user name must not be empty")
		}
		if len(name) > maxUsernameLen {
			return nil, fmt.Errorf("user name is longer than %d bytes", maxUsernameLen)
		}
		if u.Password == "" {
			return nil, fmt.Errorf("user %q: password must not be empty", name)
		}
		if len(u.Password) > maxPasswordLen {
			return nil, fmt.Errorf("user %q: password is longer than %d bytes", name, maxPasswordLen)
		}
		role := u.Role
		if role == "" {
			role = RoleOperator
		}
		if !role.Valid() {
			return nil, fmt.Errorf("user %q: unknown role %q", name, u.Role)
		}
		if _, dup := m.users[name]; dup {
			return nil, fmt.Errorf("duplicate user %q", name)
		}
		m.users[name] = User{Name: name, Password: u.Password, Role: role}
	}

	if secret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("generate session secret: %w", err)
		}
		m.secret = buf
	} else {
		m.secret = []byte(secret)
	}

	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.ttl <= 0 {
		m.ttl = 24 * time.Hour
	}
	return m, nil
}

// NewManager keeps the legacy single-user constructor. It fails when neither
// legacy credentials nor a Users list are provided.
func NewManager(cfg Config) (*Manager, error) {
	users := cfg.Users
	if len(users) == 0 {
		if strings.TrimSpace(cfg.Username) == "" || cfg.Password == "" {
			return nil, errors.New("PREPULLER_AUTH_USER and PREPULLER_AUTH_PASS must be set")
		}
		users = []User{{Name: cfg.Username, Password: cfg.Password, Role: RoleOperator}}
	}
	opts := make([]Option, 0, 3)
	if cfg.TTL > 0 {
		opts = append(opts, WithTTL(cfg.TTL))
	}
	if cfg.SecureCookie {
		opts = append(opts, WithSecureCookie(true))
	}
	if cfg.Auditor != nil {
		opts = append(opts, WithAuditor(cfg.Auditor))
	}
	return NewManagerFromConfig(string(cfg.SessionSecret), users, opts...)
}

// ValidateCredentials reports whether the pair belongs to a configured user.
// It never distinguishes "unknown user" from "wrong password" and never logs
// the password.
func (m *Manager) ValidateCredentials(username, password string) bool {
	_, ok := m.authenticate(strings.TrimSpace(username), password)
	return ok
}

// Login authenticates one credential pair, writes an audit event and returns
// the session identity. The password never reaches the audit sink.
func (m *Manager) Login(r *http.Request, username, password string) (Session, bool) {
	username = strings.TrimSpace(username)
	u, ok := m.authenticate(username, password)
	ev := AuditEvent{
		Actor:      username,
		Action:     ActionLogin,
		Result:     ResultDenied,
		RemoteAddr: ClientIP(r),
	}
	if ok {
		ev.Role = string(u.Role)
		ev.Result = ResultOK
	}
	m.logAudit(r, ev)
	if !ok {
		return Session{}, false
	}
	return Session{Username: u.Name, Role: u.Role}, true
}

// Audit records one authenticated action: who did what to which target.
func (m *Manager) Audit(r *http.Request, action, target, result string) {
	if m == nil || m.auditor == nil {
		return
	}
	sess, _ := m.AuthenticateSession(r)
	m.logAudit(r, AuditEvent{
		Actor:      sess.Username,
		Role:       string(sess.Role),
		Action:     action,
		Target:     target,
		Result:     result,
		RemoteAddr: ClientIP(r),
	})
}

func (m *Manager) logAudit(r *http.Request, ev AuditEvent) {
	if m == nil || m.auditor == nil {
		return
	}
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	m.auditor.Log(ctx, ev)
}

func (m *Manager) authenticate(username, password string) (User, bool) {
	if len(username) > maxUsernameLen || len(password) > maxPasswordLen {
		// Burn the same constant-time compare so oversized input is not a
		// distinguishable fast path.
		subtle.ConstantTimeCompare([]byte(password), []byte(dummyPassword))
		return User{}, false
	}
	u, ok := m.users[username]
	if !ok {
		subtle.ConstantTimeCompare([]byte(password), []byte(dummyPassword))
		return User{}, false
	}
	if subtle.ConstantTimeCompare([]byte(password), []byte(u.Password)) != 1 {
		return User{}, false
	}
	return u, true
}

// RoleFor returns the configured role of one user.
func (m *Manager) RoleFor(username string) (Role, bool) {
	u, ok := m.users[strings.TrimSpace(username)]
	if !ok {
		return "", false
	}
	return u.Role, true
}

// IssueCookie writes a signed session cookie carrying the user's role.
func (m *Manager) IssueCookie(w http.ResponseWriter, r *http.Request, username string) {
	username = strings.TrimSpace(username)
	role, ok := m.RoleFor(username)
	if !ok {
		// Legacy call sites only pass the configured name; an unknown name keeps
		// the historical operator role so old flows keep working.
		role = RoleOperator
	}
	exp := m.now().Add(m.ttl)
	http.SetCookie(w, m.sessionCookie(r, m.issueToken(username, role, exp), int(m.ttl.Seconds())))
}

func (m *Manager) issueToken(username string, role Role, exp time.Time) string {
	payload := strings.Join([]string{
		tokenVersionV2,
		base64.RawURLEncoding.EncodeToString([]byte(username)),
		string(role),
		strconv.FormatInt(exp.Unix(), 10),
	}, "|")
	return payload + "." + m.sign(payload)
}

// ClearCookie expires the session cookie.
func (m *Manager) ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, m.sessionCookie(r, "", -1))
}

func (m *Manager) sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   m.secureFor(r),
		MaxAge:   maxAge,
	}
}

func (m *Manager) secureFor(r *http.Request) bool {
	if m.secureCookie {
		return true
	}
	if r == nil {
		return false
	}
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// AuthenticateSession verifies the cookie and returns the full session,
// including the role. Both token v1 (legacy, no role) and v2 are accepted.
func (m *Manager) AuthenticateSession(r *http.Request) (Session, bool) {
	if r == nil {
		return Session{}, false
	}
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return Session{}, false
	}
	return m.parseSessionToken(c.Value)
}

func (m *Manager) parseSessionToken(token string) (Session, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return Session{}, false
	}
	payload, gotSig := parts[0], parts[1]
	wantSig := m.sign(payload)
	if subtle.ConstantTimeCompare([]byte(gotSig), []byte(wantSig)) != 1 {
		return Session{}, false
	}

	fields := strings.Split(payload, "|")
	var rawUser, roleField, expField string
	switch {
	case len(fields) == tokenFieldsV1:
		// Legacy v1 cookie: no role in the payload, operator for compatibility.
		rawUser, expField = fields[0], fields[1]
		roleField = string(RoleOperator)
	case len(fields) == tokenFieldsV2 && fields[0] == tokenVersionV2:
		rawUser, roleField, expField = fields[1], fields[2], fields[3]
	default:
		return Session{}, false
	}

	exp, err := strconv.ParseInt(expField, 10, 64)
	if err != nil {
		return Session{}, false
	}
	if m.now().Unix() > exp {
		return Session{}, false
	}
	rawName, err := base64.RawURLEncoding.DecodeString(rawUser)
	if err != nil {
		return Session{}, false
	}
	username := string(rawName)
	if username == "" {
		return Session{}, false
	}
	role := Role(roleField)
	if !role.Valid() {
		return Session{}, false
	}
	if _, ok := m.users[username]; !ok {
		// The user was removed after the cookie was issued.
		return Session{}, false
	}
	return Session{Username: username, Role: role, Expires: time.Unix(exp, 0)}, true
}

// AuthenticateRequest keeps the legacy signature: it returns the user name only.
func (m *Manager) AuthenticateRequest(r *http.Request) (string, bool) {
	sess, ok := m.AuthenticateSession(r)
	if !ok {
		return "", false
	}
	return sess.Username, true
}

// LoginMiddleware wraps the login handler with login rate limiting. Without a
// configured limiter it is a pass-through.
func (m *Manager) LoginMiddleware(next http.HandlerFunc) http.HandlerFunc {
	if m == nil || m.limiter == nil {
		return next
	}
	return m.limiter.LoginMiddleware(next)
}

// Auditor returns the configured audit sink (nil when disabled).
func (m *Manager) Auditor() Auditor {
	if m == nil {
		return nil
	}
	return m.auditor
}

// RecentAudit returns buffered audit events when the configured auditor keeps
// them (JSONAuditor does not, MemoryAuditor/TeeAuditor do).
func (m *Manager) RecentAudit() []AuditEvent {
	if m == nil || m.auditor == nil {
		return nil
	}
	if reader, ok := m.auditor.(interface{ Recent() []AuditEvent }); ok {
		return reader.Recent()
	}
	return nil
}

// Limiter returns the configured login rate limiter (nil when disabled).
func (m *Manager) Limiter() *RateLimiter {
	if m == nil {
		return nil
	}
	return m.limiter
}

// Close stops the background rate-limiter cleanup goroutine.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	if m.limiter != nil {
		m.limiter.Stop()
	}
	return nil
}

func (m *Manager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
