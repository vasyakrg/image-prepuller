package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestConcurrentSessionIssueAndVerify(t *testing.T) {
	m := newTestManager(t)
	const goroutines = 32
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			user := "admin"
			if g%2 == 0 {
				user = "view"
			}
			for i := 0; i < 50; i++ {
				rec := httptest.NewRecorder()
				m.IssueCookie(rec, httptest.NewRequest(http.MethodPost, "/", nil), user)
				cookies := rec.Result().Cookies()
				if len(cookies) != 1 {
					errCh <- fmt.Errorf("goroutine %d: expected a cookie", g)
					return
				}
				req := requestWithCookie(cookies[0])
				sess, ok := m.AuthenticateSession(req)
				if !ok || sess.Username != user {
					errCh <- fmt.Errorf("goroutine %d: session (%+v, %v)", g, sess, ok)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestConcurrentRateLimiter(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(RateLimitConfig{
		AttemptsPerMinute: 1000,
		BlockDuration:     time.Minute,
		CleanupInterval:   -1,
		Now:               clock.Now,
	})
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.5.0.%d", g%8)
			for i := 0; i < 100; i++ {
				rl.Allow(ip, "user")
				if i%10 == 0 {
					rl.Reset(ip, "user")
				}
			}
		}(g)
	}
	wg.Wait()
	if ips, users := rl.Len(); ips > 8 || users > 1 {
		t.Fatalf("unexpected bucket count: %d/%d", ips, users)
	}
}

func TestConcurrentAuditAndLogin(t *testing.T) {
	auditor := NewMemoryAuditor(50)
	m := newTestManager(t, WithAuditor(auditor))
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				user := "admin"
				pass := "s3cr3t"
				if i%3 == 0 {
					pass = "wrong"
				}
				m.Login(nil, user, pass)
				rec := httptest.NewRecorder()
				m.IssueCookie(rec, httptest.NewRequest(http.MethodPost, "/", nil), "view")
				req := requestWithCookie(rec.Result().Cookies()[0])
				m.Audit(req, ActionPull, "img", ResultDenied)
			}
		}(g)
	}
	wg.Wait()
	if events := auditor.Recent(); len(events) != 50 {
		t.Fatalf("expected the ring buffer to be full, got %d", len(events))
	}
}
