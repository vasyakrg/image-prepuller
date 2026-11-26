package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitConfig configures the in-memory login rate limiter.
type RateLimitConfig struct {
	// AttemptsPerMinute is the refill rate; the bucket starts full with this
	// many attempts. Default 5.
	AttemptsPerMinute int
	// BlockDuration is how long a key stays blocked after the bucket empties.
	// Default 5 minutes.
	BlockDuration time.Duration
	// CleanupInterval is how often idle buckets are evicted. Default 5 minutes;
	// a negative value disables the background ticker.
	CleanupInterval time.Duration
	// Now overrides the clock (tests). Default time.Now.
	Now func() time.Time
}

// DefaultRateLimitConfig returns the production defaults: 5 attempts per
// minute, 5 minute block, 5 minute cleanup.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		AttemptsPerMinute: 5,
		BlockDuration:     5 * time.Minute,
		CleanupInterval:   5 * time.Minute,
	}
}

// Environment variables for the login rate limiter.
const (
	EnvLoginAttempts = "PREPULLER_LOGIN_ATTEMPTS_PER_MINUTE"
	EnvLoginBlock    = "PREPULLER_LOGIN_BLOCK"
)

// RateLimitConfigFromEnv builds the limiter configuration from the environment,
// falling back to DefaultRateLimitConfig.
func RateLimitConfigFromEnv() (RateLimitConfig, error) {
	return ParseRateLimitConfigFromEnv(os.Getenv)
}

// ParseRateLimitConfigFromEnv is RateLimitConfigFromEnv with an injectable
// lookup for tests.
func ParseRateLimitConfigFromEnv(getenv func(string) string) (RateLimitConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := DefaultRateLimitConfig()
	if raw := strings.TrimSpace(getenv(EnvLoginAttempts)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid %s %q: want a positive integer", EnvLoginAttempts, raw)
		}
		cfg.AttemptsPerMinute = n
	}
	if raw := strings.TrimSpace(getenv(EnvLoginBlock)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("invalid %s %q: want a Go duration such as 5m", EnvLoginBlock, raw)
		}
		cfg.BlockDuration = d
	}
	return cfg, nil
}

func (c RateLimitConfig) withDefaults() RateLimitConfig {
	if c.AttemptsPerMinute <= 0 {
		c.AttemptsPerMinute = 5
	}
	if c.BlockDuration <= 0 {
		c.BlockDuration = 5 * time.Minute
	}
	if c.CleanupInterval == 0 {
		c.CleanupInterval = 5 * time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

type loginBucket struct {
	tokens       float64
	last         time.Time
	lastSeen     time.Time
	blockedUntil time.Time
}

// RateLimiter is an in-memory token bucket keyed by client IP and by username.
// A successful login resets both counters. It is safe for concurrent use.
type RateLimiter struct {
	cfg        RateLimitConfig
	ratePerSec float64
	capacity   float64

	mu          sync.Mutex
	ipBuckets   map[string]*loginBucket
	userBuckets map[string]*loginBucket

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// NewRateLimiter creates a limiter and starts its background cleanup goroutine.
// Call Stop (or Manager.Close) to release the goroutine.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	cfg = cfg.withDefaults()
	rl := &RateLimiter{
		cfg:         cfg,
		ratePerSec:  float64(cfg.AttemptsPerMinute) / 60,
		capacity:    float64(cfg.AttemptsPerMinute),
		ipBuckets:   make(map[string]*loginBucket),
		userBuckets: make(map[string]*loginBucket),
	}
	if cfg.CleanupInterval > 0 {
		rl.stopCh = make(chan struct{})
		rl.doneCh = make(chan struct{})
		go rl.cleanupLoop()
	}
	return rl
}

// Allow consumes one attempt for ip and username. When the request is denied it
// returns how long the caller should wait before retrying.
func (rl *RateLimiter) Allow(ip, username string) (time.Duration, bool) {
	if rl == nil {
		return 0, true
	}
	now := rl.cfg.Now()
	ipKey := strings.TrimSpace(ip)
	userKey := normalizeUsername(username)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	ipBucket := rl.bucket(rl.ipBuckets, ipKey, now)
	ipRetry, ipOK := rl.check(ipBucket, now)

	var (
		userBucket *loginBucket
		userRetry  time.Duration
		userOK     = true
	)
	if userKey != "" {
		userBucket = rl.bucket(rl.userBuckets, userKey, now)
		userRetry, userOK = rl.check(userBucket, now)
	}
	if !ipOK || !userOK {
		retry := ipRetry
		if !userOK && userRetry > retry {
			retry = userRetry
		}
		if retry <= 0 {
			retry = rl.cfg.BlockDuration
		}
		return retry, false
	}
	ipBucket.tokens--
	if userBucket != nil {
		userBucket.tokens--
	}
	return 0, true
}

// Reset clears the counters for a successful login.
func (rl *RateLimiter) Reset(ip, username string) {
	if rl == nil {
		return
	}
	ipKey := strings.TrimSpace(ip)
	userKey := normalizeUsername(username)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.ipBuckets, ipKey)
	if userKey != "" {
		delete(rl.userBuckets, userKey)
	}
}

// Len reports how many buckets are tracked, for tests and diagnostics.
func (rl *RateLimiter) Len() (ips, users int) {
	if rl == nil {
		return 0, 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.ipBuckets), len(rl.userBuckets)
}

// Stop terminates the background cleanup goroutine. It is idempotent.
func (rl *RateLimiter) Stop() {
	if rl == nil {
		return
	}
	rl.stopOnce.Do(func() {
		if rl.stopCh == nil {
			return
		}
		close(rl.stopCh)
		<-rl.doneCh
	})
}

// LoginMiddleware wraps a login handler with rate limiting. Denied attempts get
// 429 with a Retry-After header; a 2xx response resets the counters.
func (rl *RateLimiter) LoginMiddleware(next http.HandlerFunc) http.HandlerFunc {
	if rl == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		username := loginUsername(r)
		retry, ok := rl.Allow(ip, username)
		if !ok {
			secs := int(math.Ceil(retry.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many login attempts, try again later"})
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)
		if rec.status >= 200 && rec.status < 300 {
			rl.Reset(ip, username)
		}
	}
}

// ClientIP extracts the client address from RemoteAddr. Proxy headers are not
// trusted: deployments terminating TLS in front of the hub should rate limit
// at the proxy as well.
func ClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	addr := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

const (
	maxLoginBodyBytes = 64 << 10
)

// loginUsername extracts the username from a JSON login body so the limiter can
// also key by account. The body is restored for the real handler.
func loginUsername(r *http.Request) string {
	if r == nil || r.Body == nil {
		return ""
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "json") {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBodyBytes+1))
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > maxLoginBodyBytes {
		return ""
	}
	var req struct {
		Username string `json:"username"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	return normalizeUsername(req.Username)
}

func normalizeUsername(username string) string {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) > maxUsernameLen {
		username = username[:maxUsernameLen]
	}
	return username
}

func (rl *RateLimiter) bucket(buckets map[string]*loginBucket, key string, now time.Time) *loginBucket {
	b := buckets[key]
	if b == nil {
		b = &loginBucket{tokens: rl.capacity, last: now}
		buckets[key] = b
	}
	b.lastSeen = now
	if now.After(b.last) {
		b.tokens = math.Min(rl.capacity, b.tokens+now.Sub(b.last).Seconds()*rl.ratePerSec)
		b.last = now
	}
	return b
}

func (rl *RateLimiter) check(b *loginBucket, now time.Time) (time.Duration, bool) {
	if now.Before(b.blockedUntil) {
		return b.blockedUntil.Sub(now), false
	}
	if b.tokens >= 1 {
		return 0, true
	}
	b.blockedUntil = now.Add(rl.cfg.BlockDuration)
	return rl.cfg.BlockDuration, false
}

func (rl *RateLimiter) cleanupLoop() {
	defer close(rl.doneCh)
	ticker := time.NewTicker(rl.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.cleanup()
		}
	}
}

func (rl *RateLimiter) cleanup() {
	now := rl.cfg.Now()
	idle := rl.cfg.BlockDuration
	if idle < time.Minute {
		idle = time.Minute
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	evictIdle(rl.ipBuckets, now, idle)
	evictIdle(rl.userBuckets, now, idle)
}

func evictIdle(buckets map[string]*loginBucket, now time.Time, idle time.Duration) {
	for key, b := range buckets {
		if now.Before(b.blockedUntil) {
			continue
		}
		if now.Sub(b.lastSeen) > idle {
			delete(buckets, key)
		}
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(p)
}
