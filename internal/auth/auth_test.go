package auth

import (
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
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
	defer func() { _ = mgr.Close() }()
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

func TestNewManagerLegacyDefaultsToOperator(t *testing.T) {
	m, err := NewManager(Config{Username: "admin", Password: "s3cr3t", SessionSecret: []byte("k")})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer func() { _ = m.Close() }()
	role, ok := m.RoleFor("admin")
	if !ok || role != RoleOperator {
		t.Fatalf("expected legacy user to be operator, got (%q, %v)", role, ok)
	}
	if _, err := NewManager(Config{}); err == nil || !strings.Contains(err.Error(), "PREPULLER_AUTH_USER") {
		t.Fatalf("expected legacy credentials error, got %v", err)
	}
}

func TestNewManagerFromConfigValidation(t *testing.T) {
	tests := []struct {
		name  string
		users []User
	}{
		{"no users", nil},
		{"empty name", []User{{Name: " ", Password: "p", Role: RoleViewer}}},
		{"empty password", []User{{Name: "u", Password: "", Role: RoleViewer}}},
		{"unknown role", []User{{Name: "u", Password: "p", Role: "root"}}},
		{"duplicate user", []User{{Name: "u", Password: "p"}, {Name: "u", Password: "q"}}},
		{"long name", []User{{Name: strings.Repeat("a", maxUsernameLen+1), Password: "p"}}},
		{"long password", []User{{Name: "u", Password: strings.Repeat("p", maxPasswordLen+1)}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewManagerFromConfig("secret", tc.users); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}

func TestNewManagerFromConfigEmptyRoleDefaultsToOperator(t *testing.T) {
	m, err := NewManagerFromConfig("secret", []User{{Name: "ops", Password: "p"}})
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	defer func() { _ = m.Close() }()
	role, ok := m.RoleFor("ops")
	if !ok || role != RoleOperator {
		t.Fatalf("expected operator role, got (%q, %v)", role, ok)
	}
}

func TestSessionCarriesRole(t *testing.T) {
	m := newTestManager(t)
	for _, tc := range []struct {
		user string
		want Role
	}{
		{"admin", RoleOperator},
		{"view", RoleViewer},
	} {
		req := requestWithCookie(issueTestCookie(t, m, tc.user))
		sess, ok := m.AuthenticateSession(req)
		if !ok {
			t.Fatalf("%s: expected valid session", tc.user)
		}
		if sess.Username != tc.user || sess.Role != tc.want {
			t.Fatalf("%s: got session %+v", tc.user, sess)
		}
		if name, ok := m.AuthenticateRequest(req); !ok || name != tc.user {
			t.Fatalf("%s: legacy AuthenticateRequest returned (%q, %v)", tc.user, name, ok)
		}
	}
}

// TestLegacyV1CookieCompatibility proves that cookies issued before roles
// existed are still accepted and map to the operator role.
func TestLegacyV1CookieCompatibility(t *testing.T) {
	m := newTestManager(t)
	exp := time.Now().Add(time.Hour).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte("view")) + "|" + strconv.FormatInt(exp, 10)
	token := payload + "." + m.sign(payload)

	req := requestWithCookie(&http.Cookie{Name: CookieName, Value: token})
	sess, ok := m.AuthenticateSession(req)
	if !ok {
		t.Fatal("expected legacy v1 cookie to authenticate")
	}
	if sess.Username != "view" || sess.Role != RoleOperator {
		t.Fatalf("legacy v1 must map to operator, got %+v", sess)
	}

	// Tampered legacy signature must fail.
	bad := requestWithCookie(&http.Cookie{Name: CookieName, Value: token + "x"})
	if _, ok := m.AuthenticateSession(bad); ok {
		t.Fatal("expected tampered v1 cookie to fail")
	}

	// Expired legacy cookie must fail.
	expiredPayload := base64.RawURLEncoding.EncodeToString([]byte("view")) + "|" + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	expired := expiredPayload + "." + m.sign(expiredPayload)
	if _, ok := m.AuthenticateSession(requestWithCookie(&http.Cookie{Name: CookieName, Value: expired})); ok {
		t.Fatal("expected expired v1 cookie to fail")
	}

	// Legacy cookie for a user that no longer exists must fail.
	ghostPayload := base64.RawURLEncoding.EncodeToString([]byte("ghost")) + "|" + strconv.FormatInt(exp, 10)
	ghost := ghostPayload + "." + m.sign(ghostPayload)
	if _, ok := m.AuthenticateSession(requestWithCookie(&http.Cookie{Name: CookieName, Value: ghost})); ok {
		t.Fatal("expected v1 cookie for removed user to fail")
	}
}

func TestSessionExpiryWithInjectedClock(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	m := newTestManager(t, WithNow(clock.Now), WithTTL(time.Hour))
	cookie := issueTestCookie(t, m, "admin")

	if _, ok := m.AuthenticateSession(requestWithCookie(cookie)); !ok {
		t.Fatal("expected fresh session to authenticate")
	}
	clock.Advance(59 * time.Minute)
	if _, ok := m.AuthenticateSession(requestWithCookie(cookie)); !ok {
		t.Fatal("expected session inside TTL to authenticate")
	}
	clock.Advance(2 * time.Minute)
	if _, ok := m.AuthenticateSession(requestWithCookie(cookie)); ok {
		t.Fatal("expected expired session to fail")
	}
}

func TestCookieSecurityAttributes(t *testing.T) {
	m := newTestManager(t, WithTTL(time.Hour))

	rec := httptest.NewRecorder()
	m.IssueCookie(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil), "admin")
	c := rec.Result().Cookies()[0]
	if c.Name != CookieName || c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected cookie attributes: %+v", c)
	}
	if c.MaxAge != int(time.Hour.Seconds()) {
		t.Fatalf("expected MaxAge %d, got %d", int(time.Hour.Seconds()), c.MaxAge)
	}
	if c.Secure {
		t.Fatal("plain HTTP request must not set Secure")
	}

	tlsReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	rec = httptest.NewRecorder()
	m.IssueCookie(rec, tlsReq, "admin")
	if !rec.Result().Cookies()[0].Secure {
		t.Fatal("TLS request must set Secure")
	}

	proxyReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	proxyReq.Header.Set("X-Forwarded-Proto", "https")
	rec = httptest.NewRecorder()
	m.IssueCookie(rec, proxyReq, "admin")
	if !rec.Result().Cookies()[0].Secure {
		t.Fatal("X-Forwarded-Proto=https must set Secure")
	}

	forced := newTestManager(t, WithSecureCookie(true))
	rec = httptest.NewRecorder()
	forced.IssueCookie(rec, httptest.NewRequest(http.MethodPost, "/", nil), "admin")
	if !rec.Result().Cookies()[0].Secure {
		t.Fatal("WithSecureCookie(true) must force Secure")
	}

	rec = httptest.NewRecorder()
	m.ClearCookie(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	cleared := rec.Result().Cookies()[0]
	if cleared.Value != "" || cleared.MaxAge != -1 {
		t.Fatalf("unexpected cleared cookie: %+v", cleared)
	}
}

func TestLoginReturnsSession(t *testing.T) {
	m := newTestManager(t)
	sess, ok := m.Login(httptest.NewRequest(http.MethodPost, "/", nil), " view ", "viewpass")
	if !ok || sess.Username != "view" || sess.Role != RoleViewer {
		t.Fatalf("unexpected login result: %+v ok=%v", sess, ok)
	}
	if _, ok := m.Login(httptest.NewRequest(http.MethodPost, "/", nil), "view", "wrong"); ok {
		t.Fatal("expected wrong password to fail")
	}
	if _, ok := m.Login(httptest.NewRequest(http.MethodPost, "/", nil), "nobody", "viewpass"); ok {
		t.Fatal("expected unknown user to fail")
	}
}

func TestInputLengthLimits(t *testing.T) {
	m := newTestManager(t)
	if m.ValidateCredentials("admin", strings.Repeat("x", maxPasswordLen+1)) {
		t.Fatal("oversized password must not authenticate")
	}
	if m.ValidateCredentials(strings.Repeat("u", maxUsernameLen+1), "s3cr3t") {
		t.Fatal("oversized username must not authenticate")
	}
	if m.ValidateCredentials("", "") {
		t.Fatal("empty credentials must not authenticate")
	}
}

func TestParseSessionTokenMalformed(t *testing.T) {
	m := newTestManager(t)
	exp := time.Now().Add(time.Hour).Unix()
	user := base64.RawURLEncoding.EncodeToString([]byte("admin"))
	validSigPayload := tokenVersionV2 + "|" + user + "|" + string(RoleOperator) + "|" + strconv.FormatInt(exp, 10)
	badRolePayload := tokenVersionV2 + "|" + user + "|superuser|" + strconv.FormatInt(exp, 10)
	badExpPayload := tokenVersionV2 + "|" + user + "|" + string(RoleOperator) + "|not-a-number"
	badUserPayload := tokenVersionV2 + "|!!!" + "|" + string(RoleOperator) + "|" + strconv.FormatInt(exp, 10)
	unknownVersionPayload := "v3|" + user + "|" + string(RoleOperator) + "|" + strconv.FormatInt(exp, 10)

	tokens := map[string]string{
		"empty":           "",
		"no dot":          "abc",
		"three parts":     "a.b.c",
		"bad signature":   validSigPayload + ".deadbeef",
		"bad role":        badRolePayload + "." + m.sign(badRolePayload),
		"bad expiry":      badExpPayload + "." + m.sign(badExpPayload),
		"bad base64 user": badUserPayload + "." + m.sign(badUserPayload),
		"unknown version": unknownVersionPayload + "." + m.sign(unknownVersionPayload),
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			if _, ok := m.parseSessionToken(token); ok {
				t.Fatal("expected malformed token to be rejected")
			}
		})
	}
}

func TestRoleForUnknownUser(t *testing.T) {
	m := newTestManager(t)
	if _, ok := m.RoleFor("ghost"); ok {
		t.Fatal("expected unknown user to have no role")
	}
	if _, ok := m.RoleFor("admin"); !ok {
		t.Fatal("expected known user to have a role")
	}
}
