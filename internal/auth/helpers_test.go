package auth

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for expiry/rate-limit tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{t: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testUsers() []User {
	return []User{
		{Name: "admin", Password: "s3cr3t", Role: RoleOperator},
		{Name: "view", Password: "viewpass", Role: RoleViewer},
	}
}

// newTestManager builds a manager with the standard test users and registers
// cleanup so rate-limiter goroutines never leak between tests.
func newTestManager(t *testing.T, opts ...Option) *Manager {
	t.Helper()
	m, err := NewManagerFromConfig("test-secret-key", testUsers(), opts...)
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// issueTestCookie issues a session cookie for name and returns it.
func issueTestCookie(t *testing.T, m *Manager, name string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	m.IssueCookie(rec, httptest.NewRequest(http.MethodGet, "/", nil), name)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	return cookies[0]
}

// requestWithCookie builds a request carrying cookie.
func requestWithCookie(cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}
