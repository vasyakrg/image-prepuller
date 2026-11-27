package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

const (
	// defaultSpokeSyncInterval is the state-push cadence the hub expects from a
	// spoke; it only sets the online/stale threshold.
	defaultSpokeSyncInterval = 3 * time.Second
	// defaultCommandWait bounds one command long-poll request.
	defaultCommandWait = 20 * time.Second
	// defaultRedeliverAfter is how long a delivered but unacknowledged command
	// waits before the hub offers it again. Delivery is at-least-once and the
	// spoke deduplicates by command id.
	defaultRedeliverAfter = 10 * time.Second
	// defaultCommandTimeout bounds one command round trip on the hub side: queue
	// wait + spoke execution. It plays the role of the direct transport's
	// per-attempt network timeout, so a caller sees the same overall wait: the
	// retry policy stays on top of it.
	defaultCommandTimeout = 5 * time.Second
	// maxCommandsPerPoll caps how many commands one long-poll answer carries, so
	// a burst cannot make the spoke execute an unbounded batch at once.
	maxCommandsPerPoll = 8
	// maxCommandResponseBytes bounds a recorded command response body.
	maxCommandResponseBytes = 8 << 20
	// minStaleAfter is the floor of the offline threshold.
	minStaleAfter = 10 * time.Second
)

// ErrUnknownSpokeCluster reports a request for a managed cluster that was never
// registered with SpokeHub.AddCluster.
var ErrUnknownSpokeCluster = errors.New("unknown managed cluster")

// SpokeHubConfig tunes the hub side of the outbound spoke channel. The zero
// value is usable: every field falls back to a production default.
type SpokeHubConfig struct {
	// SyncInterval is the expected state-push interval. A spoke silent for more
	// than max(3*SyncInterval, 10s) is reported offline.
	SyncInterval time.Duration
	// CommandWait bounds one long-poll request (default 20s).
	CommandWait time.Duration
	// RedeliverAfter is the redelivery delay for unacknowledged commands
	// (default 10s).
	RedeliverAfter time.Duration
	// CommandTimeout bounds one command round trip (default 5s, the same
	// per-attempt budget the direct transport uses).
	CommandTimeout time.Duration
	// Now is a clock hook for tests; nil means time.Now.
	Now func() time.Time
}

func (c SpokeHubConfig) withDefaults() SpokeHubConfig {
	if c.SyncInterval <= 0 {
		c.SyncInterval = defaultSpokeSyncInterval
	}
	if c.CommandWait <= 0 {
		c.CommandWait = defaultCommandWait
	}
	if c.RedeliverAfter <= 0 {
		c.RedeliverAfter = defaultRedeliverAfter
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = defaultCommandTimeout
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// staleAfter is how long a silent spoke is still reported online.
func (c SpokeHubConfig) staleAfter() time.Duration {
	stale := 3 * c.SyncInterval
	if stale < minStaleAfter {
		stale = minStaleAfter
	}
	return stale
}

// SpokeHub owns the hub-side state of every managed cluster that connects
// outbound: the last pushed snapshot/counters, the command mailbox and the
// per-cluster token. It is safe for concurrent use.
type SpokeHub struct {
	cfg SpokeHubConfig

	mu       sync.Mutex
	clusters map[string]*spokeCluster
	order    []string
}

// spokeCluster is one managed cluster's hub-side session.
type spokeCluster struct {
	name  string
	token string
	prov  *PushProvider

	state   SpokeState
	stateAt time.Time

	pending map[string]*pendingCommand
	order   []string
	wake    chan struct{}
}

// pendingCommand is a hub->spoke call waiting for its result.
type pendingCommand struct {
	cmd    SpokeCommand
	sentAt time.Time
	result chan SpokeCommandResult
}

// NewSpokeHub creates an empty hub-side mailbox; register managed clusters with
// AddCluster before serving requests.
func NewSpokeHub(cfg SpokeHubConfig) *SpokeHub {
	return &SpokeHub{
		cfg:      cfg.withDefaults(),
		clusters: make(map[string]*spokeCluster),
	}
}

// AddCluster registers one managed cluster and the token its spoke must present.
// An empty token is refused: unlike the spoke's own RequireToken, the hub side
// never disables authentication.
func (h *SpokeHub) AddCluster(name, token string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("managed cluster name is required")
	}
	if token == "" {
		return fmt.Errorf("managed cluster %q: token is required", name)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.clusters[name]; exists {
		return fmt.Errorf("managed cluster %q is already registered", name)
	}
	h.clusters[name] = &spokeCluster{
		name:    name,
		token:   token,
		pending: make(map[string]*pendingCommand),
		wake:    make(chan struct{}, 1),
	}
	h.order = append(h.order, name)
	return nil
}

// Clusters lists the registered managed cluster names in registration order.
func (h *SpokeHub) Clusters() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.order...)
}

// Token returns the registered token of one managed cluster. It exists for the
// hub's auth middleware (auth.RequireSpokeToken); an unknown cluster reports
// false.
func (h *SpokeHub) Token(cluster string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.clusters[cluster]
	if !ok {
		return "", false
	}
	return c.token, true
}

// Authorize reports whether token is the cluster token of cluster. Comparison is
// constant time; an unknown cluster or a token mismatch reports false.
func (h *SpokeHub) Authorize(cluster, token string) bool {
	h.mu.Lock()
	c, ok := h.clusters[cluster]
	var want string
	if ok {
		want = c.token
	}
	h.mu.Unlock()
	if !ok || want == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

// Provider returns the hub-side provider of one managed cluster. The same
// instance is returned on every call, so the registry can hold it and the
// mailbox keeps a single queue.
func (h *SpokeHub) Provider(cluster string) (*PushProvider, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.clusters[cluster]
	if !ok {
		return nil, ErrUnknownSpokeCluster
	}
	if c.prov == nil {
		c.prov = h.newPushProvider(c)
	}
	return c.prov, nil
}

// AcceptState stores the state pushed by a spoke and marks it as fresh.
func (h *SpokeHub) AcceptState(cluster string, st SpokeState) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.clusters[cluster]
	if !ok {
		return ErrUnknownSpokeCluster
	}
	if st.ClusterName != "" && st.ClusterName != cluster {
		return fmt.Errorf("spoke identifies itself as %q but is registered as %q", st.ClusterName, cluster)
	}
	st.ClusterName = cluster
	if st.SentAt.IsZero() {
		st.SentAt = h.cfg.Now()
	}
	c.state = st
	c.stateAt = h.cfg.Now()
	return nil
}

// state returns the last pushed state of one cluster.
func (h *SpokeHub) state(cluster string) (SpokeState, time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.clusters[cluster]
	if !ok || c.stateAt.IsZero() {
		return SpokeState{}, time.Time{}, false
	}
	return c.state, c.stateAt, true
}

// PollCommands blocks until a command is queued for the cluster, the wait
// expires (returns nil, nil) or ctx is done. The answer is a batch of at most
// maxCommandsPerPoll commands, including redeliveries of unacknowledged ones.
func (h *SpokeHub) PollCommands(ctx context.Context, cluster string) ([]SpokeCommand, error) {
	h.mu.Lock()
	c, ok := h.clusters[cluster]
	h.mu.Unlock()
	if !ok {
		return nil, ErrUnknownSpokeCluster
	}

	start := h.cfg.Now()
	for {
		cmds, redeliverIn := h.takeCommands(c)
		if len(cmds) > 0 {
			return cmds, nil
		}
		remaining := h.cfg.CommandWait - h.cfg.Now().Sub(start)
		if remaining <= 0 {
			return nil, nil
		}
		wait := remaining
		if redeliverIn > 0 && redeliverIn < wait {
			wait = redeliverIn
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-c.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// takeCommands hands out every due command and reports how long until the next
// redelivery (0 when nothing is pending).
func (h *SpokeHub) takeCommands(c *spokeCluster) ([]SpokeCommand, time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.cfg.Now()
	out := make([]SpokeCommand, 0, maxCommandsPerPoll)
	var next time.Duration
	for _, id := range c.order {
		p := c.pending[id]
		if p == nil {
			continue
		}
		if !p.sentAt.IsZero() {
			due := p.sentAt.Add(h.cfg.RedeliverAfter)
			if now.Before(due) {
				if wait := due.Sub(now); next == 0 || wait < next {
					next = wait
				}
				continue
			}
		}
		if len(out) >= maxCommandsPerPoll {
			break
		}
		p.sentAt = now
		out = append(out, p.cmd)
	}
	return out, next
}

// AcceptResult resolves the waiter of one command. An unknown or expired id is a
// no-op: the hub may have already given up on the caller's context, and result
// delivery must stay idempotent.
func (h *SpokeHub) AcceptResult(cluster, id string, res SpokeCommandResult) error {
	h.mu.Lock()
	c, ok := h.clusters[cluster]
	if !ok {
		h.mu.Unlock()
		return ErrUnknownSpokeCluster
	}
	p := c.pending[id]
	if p != nil {
		delete(c.pending, id)
		for i, queued := range c.order {
			if queued == id {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	h.mu.Unlock()
	if p != nil {
		select {
		case p.result <- res:
		default:
		}
	}
	return nil
}

// enqueue registers a command and waits for its result or ctx cancellation.
func (h *SpokeHub) enqueue(ctx context.Context, c *spokeCluster, cmd SpokeCommand) (SpokeCommandResult, error) {
	entry := &pendingCommand{cmd: cmd, result: make(chan SpokeCommandResult, 1)}

	h.mu.Lock()
	c.pending[cmd.ID] = entry
	c.order = append(c.order, cmd.ID)
	h.mu.Unlock()
	signal(c.wake)

	select {
	case res := <-entry.result:
		return res, nil
	case <-ctx.Done():
		h.mu.Lock()
		delete(c.pending, cmd.ID)
		for i, queued := range c.order {
			if queued == cmd.ID {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		h.mu.Unlock()
		return SpokeCommandResult{}, ctx.Err()
	}
}

// signal wakes the parked long poll without blocking.
func signal(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// commandTransport carries hub->spoke HTTP calls over the outbound channel. It
// is an http.RoundTripper on purpose: RemoteProvider keeps its timeouts, retries
// and HTTP-status-to-error mapping, only the wire underneath changes.
type commandTransport struct {
	hub     *SpokeHub
	cluster *spokeCluster
}

// RoundTrip turns one hub->spoke request into a command and returns the recorded
// answer of the spoke handler.
func (t *commandTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		read, err := io.ReadAll(io.LimitReader(req.Body, maxCommandResponseBytes))
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s %s body: %w", req.Method, req.URL.Path, err)
		}
		body = read
	}
	cmd := SpokeCommand{
		ID:     newCommandID(),
		Method: req.Method,
		Path:   req.URL.Path,
		Query:  req.URL.RawQuery,
	}
	if len(body) > 0 {
		cmd.Body = body
	}

	res, err := t.hub.enqueue(req.Context(), t.cluster, cmd)
	if err != nil {
		return nil, err
	}
	if res.Error != "" {
		return nil, fmt.Errorf("spoke command %s: %s", cmd.ID, res.Error)
	}
	status := res.Status
	if status < 100 || status > 599 {
		status = http.StatusBadGateway
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(res.Body)),
		ContentLength: int64(len(res.Body)),
		Request:       req,
	}, nil
}

// PushProvider is the hub-side Provider of a managed cluster that connects
// outbound. Reads (Snapshot, PullCounters, Health) are served from the last
// pushed state and never block; state-changing calls travel to the spoke as
// commands and reuse the RemoteProvider transport rules.
type PushProvider struct {
	hub     *SpokeHub
	cluster *spokeCluster
	remote  *RemoteProvider
}

// newPushProvider builds the provider pair of one managed cluster.
func (h *SpokeHub) newPushProvider(c *spokeCluster) *PushProvider {
	// The base URL only shapes request paths: commandTransport delivers the
	// method and path to the spoke verbatim, there is no host to dial.
	remote := newRemoteProviderWithTransport(
		c.name,
		"http://spoke.internal",
		"",
		&commandTransport{hub: h, cluster: c},
		RemoteConfig{Timeout: h.cfg.CommandTimeout},
	)
	return &PushProvider{hub: h, cluster: c, remote: remote}
}

// Cluster is the managed cluster id this provider serves.
func (p *PushProvider) Cluster() string { return p.cluster.name }

// SyncAge reports how long ago the spoke last pushed state.
func (p *PushProvider) SyncAge() (time.Duration, bool) {
	_, at, ok := p.hub.state(p.cluster.name)
	if !ok {
		return 0, false
	}
	return p.hub.cfg.Now().Sub(at), true
}

// Snapshot serves the last pushed snapshot, or an offline one when the spoke has
// not been heard from for max(3*syncInterval, 10s).
func (p *PushProvider) Snapshot(ctx context.Context) model.Snapshot {
	st, at, ok := p.hub.state(p.cluster.name)
	if !ok || p.stale(at) {
		return offlineSnapshot(p.cluster.name)
	}
	snap := st.Snapshot
	normalizeSnapshot(p.cluster.name, &snap)
	if snap.UpdatedAt.IsZero() {
		snap.UpdatedAt = at
	}
	return snap
}

// TriggerPull sends POST /internal/v1/pull as a command.
func (p *PushProvider) TriggerPull(ctx context.Context, req model.PullRequest) (model.PullTask, error) {
	return p.remote.TriggerPull(ctx, req)
}

// TriggerWorkloadPull sends POST /internal/v1/workloads/pull as a command.
func (p *PushProvider) TriggerWorkloadPull(ctx context.Context, req model.WorkloadPullRequest) (int, error) {
	return p.remote.TriggerWorkloadPull(ctx, req)
}

// CancelPull sends POST /internal/v1/pulls/{id}/cancel as a command.
func (p *PushProvider) CancelPull(ctx context.Context, taskID string) error {
	return p.remote.CancelPull(ctx, taskID)
}

// SyncNow sends POST /internal/v1/reconcile as a command.
func (p *PushProvider) SyncNow(ctx context.Context, actor string) error {
	return p.remote.SyncNow(ctx, actor)
}

// Health reports the pushed state as online while it is fresh. There is no hub
// side latency measurement on this channel, so LatencyMs stays zero; the age of
// the last push is the real signal and is exposed through SyncAge.
func (p *PushProvider) Health(ctx context.Context) model.Health {
	now := p.hub.cfg.Now()
	age, ok := p.SyncAge()
	if !ok {
		return model.Health{Online: false, Error: "spoke has not connected yet", CheckedAt: now}
	}
	st, _, _ := p.hub.state(p.cluster.name)
	if p.stale(now.Add(-age)) {
		return model.Health{
			Online:    false,
			Error:     fmt.Sprintf("spoke last pushed state %s ago", age.Round(time.Second)),
			CheckedAt: now,
		}
	}
	version := st.Version
	if version == "" {
		version = model.APIVersion
	}
	return model.Health{Online: true, Version: version, CheckedAt: now}
}

// PullCounters returns the pull series from the last pushed state. nil means "no
// counters" (a silent spoke), matching RemoteProvider.
func (p *PushProvider) PullCounters() []model.PullMetricSample {
	st, at, ok := p.hub.state(p.cluster.name)
	if !ok || p.stale(at) {
		return nil
	}
	if st.Counters.Samples == nil {
		return []model.PullMetricSample{}
	}
	return append([]model.PullMetricSample(nil), st.Counters.Samples...)
}

// stale reports whether state received at at is too old to be trusted.
func (p *PushProvider) stale(at time.Time) bool {
	return p.hub.cfg.Now().Sub(at) > p.hub.cfg.staleAfter()
}

// newCommandID returns a random opaque command id.
func newCommandID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("cmd-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
