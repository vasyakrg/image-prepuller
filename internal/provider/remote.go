package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// Transport-level sentinel errors. They live next to the implementation so hub
// handlers can map them onto HTTP statuses (502/401/409) without importing
// anything else; moving them into provider.go is an integrator decision.
var (
	// ErrSpokeUnavailable reports that the spoke could not be reached, timed out
	// or answered with a gateway-style 5xx after all retries. Hub handlers turn
	// it into HTTP 502.
	ErrSpokeUnavailable = errors.New("spoke unavailable")
	// ErrSpokeUnauthorized reports that the spoke rejected the cluster token.
	ErrSpokeUnauthorized = errors.New("spoke unauthorized")
	// ErrTaskNotActive reports that a cancel request hit a task that already
	// finished (spoke answers 409 Conflict).
	ErrTaskNotActive = errors.New("pull task is not active")
)

const (
	// defaultRemoteTimeout bounds a single HTTP attempt to a spoke.
	defaultRemoteTimeout = 5 * time.Second
	// defaultRemoteAttempts is the total number of attempts for state-changing
	// calls (pull, workload pull, cancel, reconcile). They are safe to retry:
	// the spoke makes TriggerPull idempotent and cancel of a finished task is
	// reported as 409 instead of creating a second task.
	defaultRemoteAttempts = 3
	// defaultRemoteBackoff is the first retry delay; it doubles per attempt.
	defaultRemoteBackoff = 150 * time.Millisecond
	// maxRemoteBackoff caps the exponential retry delay.
	maxRemoteBackoff = 2 * time.Second
	// maxSpokeResponseBytes bounds a decoded spoke response body.
	maxSpokeResponseBytes = 8 << 20
	// drainResponseBytes is how much of an unwanted body is read and dropped so
	// the underlying connection can be reused.
	drainResponseBytes = 4 << 10
)

// RemoteConfig tunes the hub -> spoke transport. The zero value is usable:
// every field falls back to a production default.
type RemoteConfig struct {
	// Timeout bounds one HTTP attempt (default 5s).
	Timeout time.Duration
	// MaxAttempts is the total attempt count for retryable calls (default 3).
	MaxAttempts int
	// Backoff is the base retry delay, doubled on every next attempt.
	Backoff time.Duration
	// MaxIdleConns / MaxIdleConnsPerHost / IdleConnTimeout configure the
	// per-provider connection pool (defaults 16 / 8 / 90s).
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
}

func (c RemoteConfig) withDefaults() RemoteConfig {
	if c.Timeout <= 0 {
		c.Timeout = defaultRemoteTimeout
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultRemoteAttempts
	}
	if c.Backoff <= 0 {
		c.Backoff = defaultRemoteBackoff
	}
	if c.MaxIdleConns <= 0 {
		c.MaxIdleConns = 16
	}
	if c.MaxIdleConnsPerHost <= 0 {
		c.MaxIdleConnsPerHost = 8
	}
	if c.IdleConnTimeout <= 0 {
		c.IdleConnTimeout = 90 * time.Second
	}
	return c
}

// RemoteProvider connects a Hub controller to a headless Spoke controller
// over the internal API authenticated with a Bearer token. Wire contract v2:
// see docs/PLAN.md section 10 and internal/web/server_spoke.go.
//
// One RemoteProvider owns exactly one http.Client and one bounded connection
// pool; every request carries a timeout and safe calls are retried with an
// exponential backoff that respects context cancellation.
type RemoteProvider struct {
	clusterName string
	baseURL     string
	token       string
	client      *http.Client
	cfg         RemoteConfig

	mu      sync.RWMutex
	lastErr string
}

// NewRemoteProvider builds a provider with the default transport policy.
func NewRemoteProvider(clusterName, baseURL, token string) *RemoteProvider {
	return NewRemoteProviderWithConfig(clusterName, baseURL, token, RemoteConfig{})
}

// NewRemoteProviderWithConfig builds a provider with an explicit transport
// policy; zero fields of cfg fall back to defaults.
func NewRemoteProviderWithConfig(clusterName, baseURL, token string, cfg RemoteConfig) *RemoteProvider {
	return newRemoteProviderWithTransport(clusterName, baseURL, token, nil, cfg)
}

// newRemoteProviderWithTransport builds a provider on top of an explicit
// RoundTripper. A nil rt keeps the real HTTP transport; the outbound spoke
// channel (spoke_hub.go) passes its command mailbox so that every request rule
// in this file - timeouts, retries, status mapping - applies unchanged.
func newRemoteProviderWithTransport(clusterName, baseURL, token string, rt http.RoundTripper, cfg RemoteConfig) *RemoteProvider {
	cfg = cfg.withDefaults()
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if rt != nil {
		return &RemoteProvider{
			clusterName: clusterName,
			baseURL:     strings.TrimRight(baseURL, "/"),
			token:       token,
			client:      &http.Client{Transport: rt},
			cfg:         cfg,
		}
	}
	return &RemoteProvider{
		clusterName: clusterName,
		baseURL:     strings.TrimRight(baseURL, "/"),
		token:       token,
		client:      &http.Client{Transport: transport},
		cfg:         cfg,
	}
}

// Timeout is the configured per-attempt timeout (exposed for health reporting
// and tests).
func (p *RemoteProvider) Timeout() time.Duration { return p.cfg.Timeout }

// LastError is the most recent transport error seen by this provider ("" after
// the last successful call). It lets the hub explain an offline cluster without
// putting an error field into model.Snapshot.
func (p *RemoteProvider) LastError() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastErr
}

func (p *RemoteProvider) setLastError(msg string) {
	p.mu.Lock()
	p.lastErr = msg
	p.mu.Unlock()
}

// HTTPError is a non-2xx spoke response; callers map specific statuses onto
// provider sentinel errors (for example 404 -> ErrTaskNotFound).
type HTTPError struct {
	Cluster    string
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("spoke %s returned HTTP %d", e.Cluster, e.StatusCode)
}

// SpokeCounters is the aggregate envelope of GET /internal/v1/counters.
// Samples keeps the exact per image/node/result series behind
// prepuller_pull_total; the aggregate fields let the UI show totals without
// recomputing them. Older spokes answer with a bare sample array, which the
// client still accepts.
type SpokeCounters struct {
	PullTotal     uint64                   `json:"pullTotal"`
	PullSucceeded uint64                   `json:"pullSucceeded"`
	PullFailed    uint64                   `json:"pullFailed"`
	PullCanceled  uint64                   `json:"pullCanceled"`
	PullInFlight  int                      `json:"pullInFlight"`
	ImagesCached  int                      `json:"imagesCached"`
	LastSyncAt    time.Time                `json:"lastSyncAt"`
	Samples       []model.PullMetricSample `json:"samples"`
}

// HealthReport is the extended spoke health view: model.Health plus the wire
// contract version, the spoke uptime and the configured request timeout.
type HealthReport struct {
	model.Health
	APIVersion    string `json:"apiVersion,omitempty"`
	UptimeSeconds int64  `json:"uptimeSeconds,omitempty"`
	TimeoutMs     int64  `json:"timeoutMs,omitempty"`
}

// spokeHealthBody mirrors the spoke's health payload.
type spokeHealthBody struct {
	model.Health
	APIVersion    string `json:"apiVersion"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}

// doJSON performs one spoke request with retries. attempts is the total number
// of tries; pass 1 for read-only paths (snapshot, counters, health) so a dead
// spoke can never stall a metrics scrape or the Web UI.
func (p *RemoteProvider) doJSON(ctx context.Context, method, path string, reqBody, respBody any, attempts int) error {
	var payload []byte
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return p.failf(fmt.Errorf("encode %s %s: %w", method, path, err))
		}
		payload = encoded
	}
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return p.failf(fmt.Errorf("%s %s: %w", method, path, err))
		}
		lastErr = p.doOnce(ctx, method, path, payload, respBody)
		if lastErr == nil {
			p.setLastError("")
			return nil
		}
		// A cancelled/deadline-exceeded parent context is returned as-is: retries
		// and callers must see context.Canceled, never an "unavailable" wrapper.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return p.failf(fmt.Errorf("%s %s: %w", method, path, ctxErr))
		}
		if attempt == attempts || !retryable(lastErr) {
			break
		}
		delay := p.retryDelay(attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return p.failf(fmt.Errorf("%s %s: %w", method, path, ctx.Err()))
		case <-timer.C:
		}
	}
	return p.failf(p.classify(method, path, lastErr))
}

// retryDelay grows the backoff exponentially but never above maxRemoteBackoff.
func (p *RemoteProvider) retryDelay(attempt int) time.Duration {
	delay := p.cfg.Backoff
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= maxRemoteBackoff {
			return maxRemoteBackoff
		}
	}
	return delay
}

// retryable reports whether an attempt may be repeated safely: transport
// failures and gateway-style answers are transient, everything else is a
// decision of the spoke and must be surfaced to the caller.
func retryable(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusTooManyRequests,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// Transport failures and per-attempt timeouts are transient. A cancelled
	// parent context never reaches this point: doJSON checks ctx.Err() first.
	return true
}

// classify wraps an exhausted error into the typed transport errors.
func (p *RemoteProvider) classify(method, path string, err error) error {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("%w: cluster %s: %s %s: HTTP %d",
				ErrSpokeUnauthorized, p.clusterName, method, path, httpErr.StatusCode)
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return fmt.Errorf("%w: cluster %s: %w", ErrSpokeUnavailable, p.clusterName, httpErr)
		default:
			return err
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return fmt.Errorf("%w: cluster %s: %s %s: %w", ErrSpokeUnavailable, p.clusterName, method, path, err)
}

func (p *RemoteProvider) failf(err error) error {
	p.setLastError(err.Error())
	return err
}

// doOnce performs exactly one timed HTTP attempt.
func (p *RemoteProvider) doOnce(ctx context.Context, method, path string, payload []byte, respBody any) error {
	attemptCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(attemptCtx, method, p.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Prepuller-APIVersion", model.APIVersion)
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainResponseBytes))
		return &HTTPError{Cluster: p.clusterName, StatusCode: resp.StatusCode}
	}
	if respBody == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainResponseBytes))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSpokeResponseBytes)).Decode(respBody); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

// Snapshot fetches the whole cluster view. A transport failure is reported as
// an empty offline snapshot (the Provider interface cannot return an error);
// LastError() carries the reason and Health() reports the offline state.
// It deliberately makes a single attempt: /metrics and the UI call it often.
func (p *RemoteProvider) Snapshot(ctx context.Context) model.Snapshot {
	var snap model.Snapshot
	if err := p.doJSON(ctx, http.MethodGet, "/internal/v1/snapshot", nil, &snap, 1); err != nil {
		return offlineSnapshot(p.clusterName)
	}
	normalizeSnapshot(p.clusterName, &snap)
	return snap
}

// offlineSnapshot is the provider-interface stand-in for an unreachable cluster:
// an empty snapshot marked offline and partial, never an error (Provider cannot
// return one).
func offlineSnapshot(cluster string) model.Snapshot {
	snap := model.Snapshot{
		ClusterName: cluster,
		APIVersion:  model.APIVersion,
		UpdatedAt:   time.Now(),
		SyncStatus:  model.SyncOffline,
		Partial:     true,
	}
	snap.Normalize()
	return snap
}

// normalizeSnapshot fills the fields a spoke or state push may omit, so the UI
// and metrics always see a consistent snapshot.
func normalizeSnapshot(cluster string, snap *model.Snapshot) {
	if snap.ClusterName == "" {
		snap.ClusterName = cluster
	}
	if snap.APIVersion == "" {
		snap.APIVersion = model.PreviousAPIVersion
	}
	if snap.SyncStatus == "" {
		snap.SyncStatus = model.SyncUnknown
	}
	snap.Normalize()
}

func (p *RemoteProvider) TriggerPull(ctx context.Context, req model.PullRequest) (model.PullTask, error) {
	body := struct {
		APIVersion string            `json:"apiVersion"`
		Image      string            `json:"image"`
		Node       string            `json:"node"`
		Actor      string            `json:"actor,omitempty"`
		Trigger    model.PullTrigger `json:"trigger,omitempty"`
		Policy     model.PullPolicy  `json:"policy,omitempty"`
	}{APIVersion: model.APIVersion, Image: req.Image, Node: req.Node, Actor: req.Actor, Trigger: req.Trigger, Policy: req.Policy}

	var task model.PullTask
	// Retrying is safe: the spoke answers an active image+node pull with the
	// existing task, so a retry can never create a second pull task.
	if err := p.doJSON(ctx, http.MethodPost, "/internal/v1/pull", body, &task, p.cfg.MaxAttempts); err != nil {
		return model.PullTask{}, err
	}
	if task.ID == "" {
		// Spoke running the previous contract answered {"ok":true}.
		task = model.PullTask{
			Cluster:   p.clusterName,
			Image:     req.Image,
			Node:      req.Node,
			State:     model.TaskRunning,
			Attempt:   1,
			Trigger:   req.Trigger,
			Actor:     req.Actor,
			CreatedAt: time.Now(),
		}
	}
	if task.Cluster == "" {
		task.Cluster = p.clusterName
	}
	return task, nil
}

func (p *RemoteProvider) TriggerWorkloadPull(ctx context.Context, req model.WorkloadPullRequest) (int, error) {
	var out struct {
		Triggered int `json:"triggered"`
	}
	body := struct {
		APIVersion string            `json:"apiVersion"`
		Namespace  string            `json:"namespace"`
		Kind       string            `json:"kind"`
		Name       string            `json:"name"`
		Actor      string            `json:"actor,omitempty"`
		Trigger    model.PullTrigger `json:"trigger,omitempty"`
	}{APIVersion: model.APIVersion, Namespace: req.Namespace, Kind: req.Kind, Name: req.Name, Actor: req.Actor, Trigger: req.Trigger}
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/workloads/pull", body, &out, p.cfg.MaxAttempts)
	return out.Triggered, err
}

func (p *RemoteProvider) CancelPull(ctx context.Context, taskID string) error {
	body := struct {
		APIVersion string `json:"apiVersion"`
	}{APIVersion: model.APIVersion}
	// Cancel is idempotent: a repeated cancel of a finished task is answered
	// with 409, never with a second side effect.
	err := p.doJSON(ctx, http.MethodPost, "/internal/v1/pulls/"+taskID+"/cancel", body, nil, p.cfg.MaxAttempts)
	if err == nil {
		return nil
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", ErrTaskNotActive, taskID)
		}
	}
	return err
}

func (p *RemoteProvider) SyncNow(ctx context.Context, actor string) error {
	body := struct {
		APIVersion string `json:"apiVersion"`
		Actor      string `json:"actor,omitempty"`
	}{APIVersion: model.APIVersion, Actor: actor}
	return p.doJSON(ctx, http.MethodPost, "/internal/v1/reconcile", body, nil, p.cfg.MaxAttempts)
}

// HealthReport probes GET /internal/v1/health (one attempt, no snapshots) and
// reports online/latency/version/timeout. A dead spoke yields Online=false with
// the transport error in Error, never a panic. Older spokes without the
// endpoint fall back to the public /healthz probe.
func (p *RemoteProvider) HealthReport(ctx context.Context) HealthReport {
	started := time.Now()
	report := HealthReport{TimeoutMs: p.cfg.Timeout.Milliseconds()}
	report.CheckedAt = started

	var body spokeHealthBody
	err := p.doJSON(ctx, http.MethodGet, "/internal/v1/health", nil, &body, 1)
	if err == nil {
		report.Health = body.Health
		report.Health.Online = true
		report.Health.CheckedAt = started
		report.Health.LatencyMs = time.Since(started).Milliseconds()
		report.APIVersion = body.APIVersion
		report.UptimeSeconds = body.UptimeSeconds
		if report.Health.Version == "" {
			report.Health.Version = model.APIVersion
		}
		return report
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		return p.healthzFallback(ctx, started)
	}

	report.Online = false
	report.Error = err.Error()
	report.LatencyMs = time.Since(started).Milliseconds()
	p.setLastError(err.Error())
	return report
}

// Health returns the model subset of HealthReport (Provider interface).
func (p *RemoteProvider) Health(ctx context.Context) model.Health {
	return p.HealthReport(ctx).Health
}

// healthzFallback probes the pre-v2 public health endpoint of an older spoke.
func (p *RemoteProvider) healthzFallback(ctx context.Context, started time.Time) HealthReport {
	report := HealthReport{TimeoutMs: p.cfg.Timeout.Milliseconds()}
	report.CheckedAt = started
	report.LatencyMs = time.Since(started).Milliseconds()

	reqCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.baseURL+"/healthz", nil)
	if err == nil {
		var resp *http.Response
		resp, err = p.client.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainResponseBytes))
			resp.Body.Close()
			report.Online = resp.StatusCode == http.StatusOK
			report.Version = resp.Header.Get("X-Prepuller-Version")
			if !report.Online {
				err = fmt.Errorf("spoke healthz returned HTTP %d", resp.StatusCode)
			}
		}
	}
	report.LatencyMs = time.Since(started).Milliseconds()
	if err != nil {
		report.Online = false
		report.Error = err.Error()
		p.setLastError(err.Error())
		return report
	}
	if report.Version == "" {
		report.Version = model.APIVersion
	}
	return report
}

// Counters fetches the aggregate pull counters of the spoke
// (GET /internal/v1/counters). It accepts both the v2 envelope and the bare
// sample array returned by older spokes.
func (p *RemoteProvider) Counters(ctx context.Context) (SpokeCounters, error) {
	var raw json.RawMessage
	if err := p.doJSON(ctx, http.MethodGet, "/internal/v1/counters", nil, &raw, 1); err != nil {
		return SpokeCounters{}, err
	}
	var envelope SpokeCounters
	if err := json.Unmarshal(raw, &envelope); err == nil {
		return envelope, nil
	}
	var samples []model.PullMetricSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		return SpokeCounters{}, p.failf(fmt.Errorf("decode counters: %w", err))
	}
	return SpokeCounters{Samples: samples}, nil
}

// PullCounters returns the per image/node/result series used for
// prepuller_pull_total. nil means "no counters available" (a dead spoke), not
// "zero".
func (p *RemoteProvider) PullCounters() []model.PullMetricSample {
	counters, err := p.Counters(context.Background())
	if err != nil {
		return nil
	}
	if counters.Samples == nil {
		return []model.PullMetricSample{}
	}
	return counters.Samples
}
