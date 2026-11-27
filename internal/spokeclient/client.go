// Package spokeclient connects a managed cluster's spoke to the hub's ingress
// from the outside: the spoke dials the hub, pushes cluster state periodically
// and long-polls for commands, so the hub never needs a route into the managed
// cluster. The hub side of the channel lives in internal/provider/spoke_hub.go
// and internal/web/server_spoke_hub.go.
package spokeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

const (
	// defaultSyncInterval is how often cluster state is pushed to the hub.
	defaultSyncInterval = 3 * time.Second
	// defaultMaxConcurrent bounds how many commands execute at once (a live
	// pull blocks on the node agent, so a single poller must not be serialized).
	defaultMaxConcurrent = 8
	// commandPollTimeout bounds one long-poll request: the hub answers after its
	// own wait (default 20s) or as soon as a command is queued.
	commandPollTimeout = 70 * time.Second
	// resultPostTimeout bounds one result delivery.
	resultPostTimeout = 30 * time.Second
	// maxResultBytes bounds a recorded command response body.
	maxResultBytes = 8 << 20
	// maxSeenCommands is how many executed command ids are remembered for
	// redelivery deduplication.
	maxSeenCommands = 512
	// maxBackoff caps the reconnect delay.
	maxBackoff = 30 * time.Second
	// logInterval throttles repeated transport errors.
	logInterval = time.Minute
)

// Config configures the outbound spoke channel.
type Config struct {
	// HubURL is the hub base URL reachable from this cluster, usually the hub
	// ingress (https://prepuller.example.com). Required.
	HubURL string
	// Cluster is this cluster's name; it must be registered on the hub with the
	// same token.
	Cluster string
	// Token is the cluster token the hub expects (PREPULLER_CLUSTER_TOKEN).
	Token string
	// Handler executes commands: the spoke's own API (web.NewSpokeServer).
	Handler http.Handler
	// Provider is the local provider whose state is pushed.
	Provider provider.Provider
	// SyncInterval is the state push cadence (default 3s).
	SyncInterval time.Duration
	// MaxConcurrent bounds parallel command execution (default 8).
	MaxConcurrent int
	// Logger overrides the process logger (tests).
	Logger *log.Logger
	// HTTPClient overrides the client used to reach the hub (tests).
	HTTPClient *http.Client
}

func (c Config) withDefaults() Config {
	if c.SyncInterval <= 0 {
		c.SyncInterval = defaultSyncInterval
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = defaultMaxConcurrent
	}
	if c.Logger == nil {
		c.Logger = log.Default()
	}
	if c.HTTPClient == nil {
		// No client-wide timeout: a command long poll legitimately waits, and
		// every request carries its own context deadline.
		c.HTTPClient = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}}
	}
	return c
}

// Client is the spoke half of the outbound channel. Create it with New and run
// it with Run.
type Client struct {
	cfg     Config
	baseURL string
	started time.Time
	http    *http.Client

	sem chan struct{}
	wg  sync.WaitGroup

	mu        sync.Mutex
	seen      map[string]provider.SpokeCommandResult
	seenOrder []string
	inflight  map[string]struct{}

	logMu   sync.Mutex
	logged  map[string]time.Time
	lastErr string
}

// New validates the configuration and builds a client.
func New(cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	base := strings.TrimSpace(cfg.HubURL)
	if base == "" {
		return nil, fmt.Errorf("hub url is required for the outbound spoke mode")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("hub url %q: %w", base, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("hub url %q: scheme must be http or https", base)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("hub url %q: host is required", base)
	}
	if strings.TrimSpace(cfg.Cluster) == "" {
		return nil, fmt.Errorf("cluster name is required for the outbound spoke mode")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("cluster token is required for the outbound spoke mode (PREPULLER_CLUSTER_TOKEN)")
	}
	if cfg.Handler == nil {
		return nil, fmt.Errorf("command handler is required for the outbound spoke mode")
	}
	if cfg.Provider == nil {
		return nil, fmt.Errorf("provider is required for the outbound spoke mode")
	}
	return &Client{
		cfg:      cfg,
		baseURL:  strings.TrimRight(base, "/"),
		started:  time.Now(),
		http:     cfg.HTTPClient,
		sem:      make(chan struct{}, cfg.MaxConcurrent),
		seen:     make(map[string]provider.SpokeCommandResult),
		inflight: make(map[string]struct{}),
		logged:   make(map[string]time.Time),
	}, nil
}

// Run keeps the channel alive until ctx is done. Both loops are independent: a
// failing state push never stalls command delivery and vice versa.
func (c *Client) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.stateLoop(ctx) }()
	go func() { defer wg.Done(); c.commandLoop(ctx) }()
	wg.Wait()
	// In-flight command executions are not awaited: a live pull may legitimately
	// take minutes and shutdown must not hang on it.
	c.cfg.Logger.Printf("spoke outbound channel to %s stopped", c.cfg.HubURL)
}

// stateLoop pushes cluster state immediately and then every SyncInterval.
func (c *Client) stateLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := c.pushState(ctx); err != nil {
			c.logThrottled("state-push", err)
		}
		timer.Reset(c.cfg.SyncInterval)
	}
}

// pushState posts one snapshot + counters envelope to the hub.
func (c *Client) pushState(ctx context.Context) error {
	state := provider.SpokeState{
		APIVersion:    model.APIVersion,
		ClusterName:   c.cfg.Cluster,
		SentAt:        time.Now(),
		Version:       model.APIVersion,
		UptimeSeconds: int64(time.Since(c.started).Seconds()),
		Snapshot:      c.cfg.Provider.Snapshot(ctx),
		Counters:      provider.AggregateCounters(ctx, c.cfg.Provider),
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.stateURL(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("state push: hub answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// commandLoop long-polls for commands and executes them concurrently.
func (c *Client) commandLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		commands, err := c.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logThrottled("command-poll", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = time.Second
		for _, cmd := range commands {
			c.dispatch(ctx, cmd)
		}
	}
}

// poll performs one long-poll request and returns the delivered commands.
func (c *Client) poll(ctx context.Context) ([]provider.SpokeCommand, error) {
	reqCtx, cancel := context.WithTimeout(ctx, commandPollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.commandsURL(), nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		return nil, nil
	case http.StatusOK:
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		return nil, fmt.Errorf("command poll: hub answered HTTP %d", resp.StatusCode)
	}
	var answer provider.SpokeCommands
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&answer); err != nil {
		return nil, fmt.Errorf("decode command poll answer: %w", err)
	}
	return answer.Commands, nil
}

// dispatch executes one command in the background unless it is a redelivery of
// a command that already ran here.
func (c *Client) dispatch(ctx context.Context, cmd provider.SpokeCommand) {
	if cmd.ID == "" {
		return
	}
	if res, ok := c.seenResult(cmd.ID); ok {
		// Redelivery after a lost result: report the cached answer, never re-run.
		go c.postResult(ctx, cmd.ID, res)
		return
	}
	if !c.beginCommand(cmd.ID) {
		// Already executing: its goroutine posts the result.
		return
	}
	c.sem <- struct{}{}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() { <-c.sem }()
		defer c.finishCommand(cmd.ID)
		res := c.execute(ctx, cmd)
		c.remember(cmd.ID, res)
		c.postResult(ctx, cmd.ID, res)
	}()
}

// execute runs one command through the local spoke handler and records the
// answer as the wire result.
func (c *Client) execute(ctx context.Context, cmd provider.SpokeCommand) (result provider.SpokeCommandResult) {
	defer func() {
		if r := recover(); r != nil {
			result = provider.SpokeCommandResult{Status: http.StatusInternalServerError, Error: fmt.Sprintf("command handler panic: %v", r)}
		}
	}()
	if err := validateCommand(cmd); err != nil {
		return provider.SpokeCommandResult{Status: http.StatusBadRequest, Error: err.Error()}
	}
	path := cmd.Path
	if cmd.Query != "" {
		path += "?" + cmd.Query
	}
	var body io.Reader
	if len(cmd.Body) > 0 {
		body = bytes.NewReader(cmd.Body)
	}
	req, err := http.NewRequestWithContext(ctx, cmd.Method, "http://spoke"+path, body)
	if err != nil {
		return provider.SpokeCommandResult{Status: http.StatusBadRequest, Error: err.Error()}
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Prepuller-APIVersion", model.APIVersion)

	rec := newRecorder()
	c.cfg.Handler.ServeHTTP(rec, req)

	out := provider.SpokeCommandResult{Status: rec.statusCode()}
	switch {
	case rec.overflow:
		return provider.SpokeCommandResult{Status: http.StatusBadGateway, Error: "command response body is too large"}
	case rec.statusCode() >= 300:
		// Error responses are JSON on the spoke contract; keep them for the hub.
		out.Body = jsonBody(rec.body)
	case len(rec.body) > 0:
		if body := jsonBody(rec.body); body != nil {
			out.Body = body
		}
	}
	return out
}

// postResult delivers the answer of one command to the hub.
func (c *Client) postResult(ctx context.Context, id string, res provider.SpokeCommandResult) {
	payload, err := json.Marshal(res)
	if err != nil {
		c.logThrottled("result-encode", err)
		return
	}
	reqCtx, cancel := context.WithTimeout(ctx, resultPostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.resultURL(id), bytes.NewReader(payload))
	if err != nil {
		c.logThrottled("result-request", err)
		return
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.logThrottled("result-post", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		c.logThrottled("result-post", fmt.Errorf("hub answered HTTP %d", resp.StatusCode))
	}
}

func (c *Client) stateURL() string {
	return c.baseURL + "/internal/v1/spokes/" + url.PathEscape(c.cfg.Cluster) + "/state"
}

func (c *Client) commandsURL() string {
	return c.baseURL + "/internal/v1/spokes/" + url.PathEscape(c.cfg.Cluster) + "/commands"
}

func (c *Client) resultURL(id string) string {
	return c.baseURL + "/internal/v1/spokes/" + url.PathEscape(c.cfg.Cluster) + "/commands/" + url.PathEscape(id) + "/result"
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
}

// seenResult returns the recorded answer of an already executed command.
func (c *Client) seenResult(id string) (provider.SpokeCommandResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, ok := c.seen[id]
	return res, ok
}

// beginCommand marks a command as executing; false means another goroutine
// already runs it.
func (c *Client) beginCommand(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, busy := c.inflight[id]; busy {
		return false
	}
	c.inflight[id] = struct{}{}
	return true
}

func (c *Client) finishCommand(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, id)
}

// remember caches a command answer for redelivery deduplication, bounded to the
// last maxSeenCommands ids.
func (c *Client) remember(id string, res provider.SpokeCommandResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.seen[id]; exists {
		return
	}
	c.seen[id] = res
	c.seenOrder = append(c.seenOrder, id)
	if len(c.seenOrder) > maxSeenCommands {
		oldest := c.seenOrder[0]
		c.seenOrder = c.seenOrder[1:]
		delete(c.seen, oldest)
	}
}

// logThrottled logs one transport failure per key at most once a minute so a
// hub outage does not flood the log every sync interval.
func (c *Client) logThrottled(key string, err error) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	now := time.Now()
	if last, ok := c.logged[key]; ok && now.Sub(last) < logInterval && c.lastErr == err.Error() {
		c.logged[key] = now
		return
	}
	c.logged[key] = now
	c.lastErr = err.Error()
	c.cfg.Logger.Printf("spoke outbound channel: %s: %v", key, err)
}

// validateCommand keeps the command surface to the spoke's own API: the hub
// cannot ask the spoke to serve files or any non-contract path.
func validateCommand(cmd provider.SpokeCommand) error {
	switch cmd.Method {
	case http.MethodGet, http.MethodPost:
	default:
		return fmt.Errorf("unsupported command method %q", cmd.Method)
	}
	if !strings.HasPrefix(cmd.Path, "/internal/v1/") {
		return fmt.Errorf("unsupported command path %q", cmd.Path)
	}
	return nil
}

// jsonBody returns b as a raw JSON message, or nil when it is not JSON.
func jsonBody(b []byte) json.RawMessage {
	if len(b) == 0 || !json.Valid(b) {
		return nil
	}
	return json.RawMessage(append([]byte(nil), b...))
}

// recorder is a minimal http.ResponseWriter that captures a handler answer for
// the wire instead of writing it to a socket.
type recorder struct {
	header   http.Header
	status   int
	body     []byte
	overflow bool
}

func newRecorder() *recorder { return &recorder{header: make(http.Header)} }

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if len(r.body)+len(p) > maxResultBytes {
		r.overflow = true
		return len(p), nil
	}
	r.body = append(r.body, p...)
	return len(p), nil
}

func (r *recorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// sleepCtx waits for d or ctx cancellation; false means ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
