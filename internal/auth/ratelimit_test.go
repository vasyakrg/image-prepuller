package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testLimiter(clock *fakeClock, attempts int, block time.Duration) *RateLimiter {
	return NewRateLimiter(RateLimitConfig{
		AttemptsPerMinute: attempts,
		BlockDuration:     block,
		CleanupInterval:   -1, // no background goroutine in unit tests
		Now:               clock.Now,
	})
}

func TestRateLimiterTokenBucket(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	rl := testLimiter(clock, 5, 5*time.Minute)

	for i := 0; i < 5; i++ {
		if retry, ok := rl.Allow("10.0.0.1", "alice"); !ok || retry != 0 {
			t.Fatalf("attempt %d: allowed=%v retry=%v", i+1, ok, retry)
		}
	}
	retry, ok := rl.Allow("10.0.0.1", "alice")
	if ok {
		t.Fatal("6th attempt must be denied")
	}
	if retry != 5*time.Minute {
		t.Fatalf("retry = %v, want 5m", retry)
	}

	// While blocked even a partial refill must not let the request through.
	clock.Advance(4 * time.Minute)
	if _, ok := rl.Allow("10.0.0.1", "alice"); ok {
		t.Fatal("request inside the block window must be denied")
	}

	// After the block the bucket is full again.
	clock.Advance(2 * time.Minute)
	if retry, ok := rl.Allow("10.0.0.1", "alice"); !ok || retry != 0 {
		t.Fatalf("request after block: allowed=%v retry=%v", ok, retry)
	}
}

func TestRateLimiterRefillsSmoothly(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	rl := testLimiter(clock, 5, 5*time.Minute)
	for i := 0; i < 3; i++ {
		rl.Allow("10.0.0.2", "bob")
	}
	clock.Advance(12 * time.Second) // 5/min == 1 token per 12s
	if _, ok := rl.Allow("10.0.0.2", "bob"); !ok {
		t.Fatal("expected one token to refill after 12s")
	}
}

func TestRateLimiterPerUsernameAndPerIP(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))

	t.Run("username bucket spans IPs", func(t *testing.T) {
		rl := testLimiter(clock, 3, time.Minute)
		if _, ok := rl.Allow("10.1.0.1", "alice"); !ok {
			t.Fatal("ip1 attempt 1 must pass")
		}
		if _, ok := rl.Allow("10.1.0.1", "alice"); !ok {
			t.Fatal("ip1 attempt 2 must pass")
		}
		if _, ok := rl.Allow("10.1.0.2", "alice"); !ok {
			t.Fatal("ip2 attempt must pass (username has 1 token left)")
		}
		retry, ok := rl.Allow("10.1.0.3", "alice")
		if ok {
			t.Fatal("expected username bucket to deny a fresh IP")
		}
		if retry != time.Minute {
			t.Fatalf("retry = %v, want 1m", retry)
		}
	})

	t.Run("IP bucket spans usernames", func(t *testing.T) {
		rl := testLimiter(clock, 2, time.Minute)
		if _, ok := rl.Allow("10.2.0.1", "u1"); !ok {
			t.Fatal("attempt 1 must pass")
		}
		if _, ok := rl.Allow("10.2.0.1", "u2"); !ok {
			t.Fatal("attempt 2 must pass")
		}
		if _, ok := rl.Allow("10.2.0.1", "u3"); ok {
			t.Fatal("expected IP bucket to deny a fresh username")
		}
	})
}

func TestRateLimiterResetOnSuccess(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	rl := testLimiter(clock, 5, 5*time.Minute)
	for i := 0; i < 5; i++ {
		rl.Allow("10.3.0.1", "carol")
	}
	if _, ok := rl.Allow("10.3.0.1", "carol"); ok {
		t.Fatal("expected denial before reset")
	}
	rl.Reset("10.3.0.1", "carol")
	for i := 0; i < 5; i++ {
		if _, ok := rl.Allow("10.3.0.1", "carol"); !ok {
			t.Fatalf("attempt %d after reset must pass", i+1)
		}
	}
}

type loginBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func loginRequest(t *testing.T, username, password string) *http.Request {
	t.Helper()
	body, err := json.Marshal(loginBody{Username: username, Password: password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// loginHandler is a minimal stand-in for the hub login handler.
func loginHandler(okUser, okPass string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginBody
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Username == okUser && req.Password == okPass {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}
}

func TestLoginMiddleware429AndRetryAfter(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	rl := testLimiter(clock, 2, 5*time.Minute)
	calls := 0
	h := rl.LoginMiddleware(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h(rec, loginRequest(t, "dave", "wrong"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	h(rec, loginRequest(t, "dave", "wrong"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != strconv.Itoa(int((5 * time.Minute).Seconds())) {
		t.Fatalf("Retry-After = %q, want 300", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Fatalf("expected JSON error body, got %q (err %v)", rec.Body.String(), err)
	}
	if calls != 2 {
		t.Fatalf("handler must not run for the 429, calls = %d", calls)
	}
}

func TestLoginMiddlewareSuccessResetsCounter(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	rl := testLimiter(clock, 5, 5*time.Minute)
	h := rl.LoginMiddleware(loginHandler("erin", "correct"))

	send := func(pass string) int {
		rec := httptest.NewRecorder()
		h(rec, loginRequest(t, "erin", pass))
		return rec.Code
	}
	for i := 0; i < 4; i++ {
		if code := send("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("failed attempt %d: status %d", i+1, code)
		}
	}
	if code := send("correct"); code != http.StatusOK {
		t.Fatalf("successful login: status %d", code)
	}
	// Counter was reset, so five more failures are allowed.
	for i := 0; i < 5; i++ {
		if code := send("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("post-reset failure %d: status %d, want 401", i+1, code)
		}
	}
	if code := send("wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after the reset window is used, got %d", code)
	}
}

func TestLoginMiddlewareRestoresRequestBody(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	rl := testLimiter(clock, 5, 5*time.Minute)
	var seen loginBody
	h := rl.LoginMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("handler could not read the restored body: %v", err)
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	rec := httptest.NewRecorder()
	h(rec, loginRequest(t, "frank", "pa:ss"))
	if seen.Username != "frank" || seen.Password != "pa:ss" {
		t.Fatalf("handler saw %+v", seen)
	}
}

func TestLoginMiddlewareIgnoresNonJSONBody(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	rl := testLimiter(clock, 2, time.Minute)
	h := rl.LoginMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=gina&password=x"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d", i+1, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("garbage"))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected IP-based 429, got %d", rec.Code)
	}
}

func TestManagerLoginMiddlewarePassThroughWithoutLimiter(t *testing.T) {
	m := newTestManager(t)
	called := false
	h := m.LoginMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/login", nil))
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("expected pass-through, called=%v status=%d", called, rec.Code)
	}
}

func TestRateLimiterCleanupEvictsIdleBuckets(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	rl := NewRateLimiter(RateLimitConfig{
		AttemptsPerMinute: 5,
		BlockDuration:     time.Minute,
		CleanupInterval:   5 * time.Millisecond,
		Now:               clock.Now,
	})
	defer rl.Stop()
	rl.Allow("10.4.0.1", "henry")
	if ips, users := rl.Len(); ips != 1 || users != 1 {
		t.Fatalf("expected 1/1 buckets, got %d/%d", ips, users)
	}
	clock.Advance(10 * time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ips, users := rl.Len(); ips == 0 && users == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	ips, users := rl.Len()
	t.Fatalf("idle buckets were not evicted: %d/%d", ips, users)
}

func TestRateLimiterStopDoesNotLeakGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		rl := NewRateLimiter(RateLimitConfig{CleanupInterval: 5 * time.Millisecond})
		rl.Stop()
		rl.Stop() // idempotent
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before+2 {
		t.Fatalf("goroutines leaked: before=%d after=%d", before, got)
	}
}

func TestManagerCloseStopsLimiter(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{CleanupInterval: 5 * time.Millisecond})
	m, err := NewManagerFromConfig("secret", testUsers(), WithRateLimiter(rl))
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	if m.Limiter() != rl {
		t.Fatal("expected the configured limiter")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Stop was already called by Close; a second Stop must not block.
	rl.Stop()
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		remoteAddr string
		want       string
	}{
		{"192.0.2.10:5678", "192.0.2.10"},
		{"192.0.2.10", "192.0.2.10"},
		{"", ""},
	}
	for _, tc := range tests {
		req := &http.Request{RemoteAddr: tc.remoteAddr}
		if got := ClientIP(req); got != tc.want {
			t.Errorf("ClientIP(%q) = %q, want %q", tc.remoteAddr, got, tc.want)
		}
	}
	if ClientIP(nil) != "" {
		t.Error("ClientIP(nil) must be empty")
	}
}

func TestDefaultRateLimitConfig(t *testing.T) {
	cfg := DefaultRateLimitConfig()
	if cfg.AttemptsPerMinute != 5 || cfg.BlockDuration != 5*time.Minute || cfg.CleanupInterval != 5*time.Minute {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if got := (RateLimitConfig{}).withDefaults(); got.AttemptsPerMinute != 5 || got.BlockDuration != 5*time.Minute {
		t.Fatalf("unexpected applied defaults: %+v", got)
	}
}

func TestParseRateLimitConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    RateLimitConfig
		wantErr bool
	}{
		{name: "defaults", env: map[string]string{}, want: DefaultRateLimitConfig()},
		{
			name: "custom",
			env:  map[string]string{EnvLoginAttempts: "10", EnvLoginBlock: "90s"},
			want: RateLimitConfig{AttemptsPerMinute: 10, BlockDuration: 90 * time.Second, CleanupInterval: 5 * time.Minute},
		},
		{name: "bad attempts", env: map[string]string{EnvLoginAttempts: "zero"}, wantErr: true},
		{name: "non-positive attempts", env: map[string]string{EnvLoginAttempts: "0"}, wantErr: true},
		{name: "bad block", env: map[string]string{EnvLoginBlock: "5 minutes"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string { return tc.env[key] }
			cfg, err := ParseRateLimitConfigFromEnv(getenv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRateLimitConfigFromEnv: %v", err)
			}
			if cfg.AttemptsPerMinute != tc.want.AttemptsPerMinute || cfg.BlockDuration != tc.want.BlockDuration {
				t.Fatalf("cfg = %+v, want %+v", cfg, tc.want)
			}
		})
	}
}
