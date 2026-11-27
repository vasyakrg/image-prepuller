package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Agent wire contract (HTTP/JSON, API version WireAPIVersion):
//
//	GET  /healthz                                  liveness/readiness, no auth
//	GET  /agent/v1/snapshot                        Snapshot
//	GET  /agent/v1/images/{ref}                    ImageInfo of one cached image
//	POST /agent/v1/pulls   {"image": "..."}        pull through CRI, returns PullResult
//
// Everything under /agent/v1 is protected by the optional Bearer token from
// Config.Token; an empty token disables auth for local development. Full auth
// (mTLS/RBAC) lands in WS-F; the wire contract already carries the header.
const (
	agentAPIPrefix  = "/agent/v1/"
	snapshotPath    = "/agent/v1/snapshot"
	imagesPath      = "/agent/v1/images/"
	pullsPath       = "/agent/v1/pulls"
	healthzPath     = "/healthz"
	maxRequestBytes = 1 << 20
	shutdownTimeout = 5 * time.Second
)

// PullResult is the response body of POST /agent/v1/pulls.
type PullResult struct {
	APIVersion string    `json:"apiVersion"`
	Image      string    `json:"image"`
	ImageRef   string    `json:"imageRef"`
	PulledAt   time.Time `json:"pulledAt"`
}

// ImageResult is the response body of GET /agent/v1/images/{ref}.
type ImageResult struct {
	APIVersion string    `json:"apiVersion"`
	Image      ImageInfo `json:"image"`
}

// Health is the response body of GET /healthz.
type Health struct {
	APIVersion   string `json:"apiVersion"`
	Status       string `json:"status"`
	NodeName     string `json:"node"`
	AgentVersion string `json:"agentVersion"`
}

type pullRequest struct {
	Image string `json:"image"`
}

// Server serves the agent wire contract over HTTP/JSON on Config.ListenAddr.
type Server struct {
	cfg     Config
	cri     CRI
	handler http.Handler
	metrics *Metrics
}

// NewServer builds the agent HTTP server. cri is the CRI backend, real or fake.
func NewServer(cfg Config, cri CRI) (*Server, error) {
	if cri == nil {
		return nil, errors.New("agent: nil CRI backend")
	}
	cfg = cfg.withDefaults()
	s := &Server{cfg: cfg, cri: cri, metrics: NewMetrics(cfg.NodeName, Version)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthzPath, s.handleHealth)
	mux.HandleFunc("GET "+snapshotPath, s.withAuth(s.handleSnapshot))
	mux.HandleFunc("GET "+imagesPath+"{ref...}", s.withAuth(s.handleImageStatus))
	mux.HandleFunc("POST "+pullsPath, s.withAuth(s.handlePull))
	s.handler = s.observe(mux)
	return s, nil
}

// Handler exposes the router for tests and for embedding behind another server.
func (s *Server) Handler() http.Handler { return s.handler }

// Metrics exposes the recorder for tests and for embedding.
func (s *Server) Metrics() *Metrics { return s.metrics }

// MetricsHandler serves the Prometheus exposition on the separate, unauth
// listener from Config.MetricsAddr.
func (s *Server) MetricsHandler() http.Handler { return s.metrics.Handler() }

// Serve listens on cfg.ListenAddr and, when cfg.MetricsAddr is set, serves the
// metrics exposition on that second unauthenticated listener. Both listeners
// shut down together on ctx cancel. A metrics listen error is returned at
// startup: losing the metrics endpoint silently is worse than a crashloop the
// DaemonSet shows.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("agent: listen %s: %w", s.cfg.ListenAddr, err)
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	var metricsSrv *http.Server
	if s.cfg.MetricsAddr != "" {
		metricsLn, err := net.Listen("tcp", s.cfg.MetricsAddr)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("agent: listen metrics %s: %w", s.cfg.MetricsAddr, err)
		}
		metricsSrv = &http.Server{
			Handler:           s.MetricsHandler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() { errCh <- metricsSrv.Serve(metricsLn) }()
	}

	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		return s.shutdown(srv, metricsSrv)
	case err := <-errCh:
		// One listener failed: stop the other one instead of leaving it
		// serving half of the agent.
		_ = s.shutdown(srv, metricsSrv)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("agent: serve: %w", err)
	}
}

// shutdown stops the API server and, when enabled, the metrics server.
func (s *Server) shutdown(api, metricsSrv *http.Server) error {
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	var firstErr error
	for _, srv := range []*http.Server{api, metricsSrv} {
		if srv == nil {
			continue
		}
		if err := srv.Shutdown(shutCtx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("agent: shutdown: %w", err)
		}
	}
	return firstErr
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Health{
		APIVersion:   WireAPIVersion,
		Status:       "ok",
		NodeName:     s.cfg.NodeName,
		AgentVersion: Version,
	})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := BuildSnapshot(r.Context(), s.cri, s.cfg)
	if err != nil {
		s.metrics.ObserveSnapshot(false, 0, 0, 0)
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	s.metrics.ObserveSnapshot(true, int64(snap.CachedImageCount), snap.ImageFS.UsedBytes, snap.ImageFS.CapacityBytes)
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleImageStatus(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if strings.TrimSpace(ref) == "" {
		writeError(w, http.StatusBadRequest, "image reference is required")
		return
	}
	img, ok, err := s.cri.ImageStatus(r.Context(), ref)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "image not found")
		return
	}
	writeJSON(w, http.StatusOK, ImageResult{APIVersion: WireAPIVersion, Image: img})
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	var req pullRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		writeError(w, http.StatusBadRequest, "image is required")
		return
	}

	fs, err := s.cri.ImageFS(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if ShouldPause(fs.UsedBytes, fs.CapacityBytes, s.cfg.DiskPauseRatio) {
		s.metrics.PullFinished("disk_paused")
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"auto-prepull paused: imagefs usage %.1f%% >= %.1f%% threshold",
			UsedRatio(fs.UsedBytes, fs.CapacityBytes)*100, s.cfg.DiskPauseRatio*100))
		return
	}

	// A dead registry must fail the pull fast instead of holding the slot for
	// the whole PullTimeout; the probe answers reachability only.
	if !s.cfg.DisableRegistryProbe {
		if err := ProbeRegistry(r.Context(), image, s.cfg.RegistryProbeTimeout); err != nil {
			s.metrics.PullFinished("registry_unreachable")
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}

	s.metrics.PullStarted()
	imageRef, err := s.pullImage(r.Context(), image)
	if err != nil {
		status := http.StatusBadGateway
		result := "cri_error"
		if errors.Is(err, errPullStalled) || errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
			result = "stalled"
		}
		s.metrics.PullFinished(result)
		writeError(w, status, err.Error())
		return
	}
	s.metrics.PullFinished("success")
	writeJSON(w, http.StatusOK, PullResult{
		APIVersion: WireAPIVersion,
		Image:      image,
		ImageRef:   imageRef,
		PulledAt:   time.Now().UTC(),
	})
}

// errPullStalled reports a CRI pull that stopped growing the image filesystem
// for Config.PullStallTimeout. The runtime is alive, the pull is not, so the
// hub gets 504 instead of 502 and can tell it apart from a CRI failure.
var errPullStalled = errors.New("pull stalled: image filesystem stopped growing")

// stallCancelGrace bounds how long the handler waits for the runtime to unwind
// a cancelled pull before it answers the hub anyway.
const stallCancelGrace = 5 * time.Second

type pullOutcome struct {
	ref string
	err error
}

// pullImage runs one CRI pull under the configured deadline and, unless the
// stall watchdog is disabled, cancels it when the image filesystem stops
// growing. The CRI pull API has no progress callback, so filesystem growth is
// the only progress signal the agent can observe.
func (s *Server) pullImage(ctx context.Context, image string) (string, error) {
	pullCtx, cancel := context.WithTimeout(ctx, s.cfg.PullTimeout)
	defer cancel()

	done := make(chan pullOutcome, 1)
	go func() {
		ref, err := s.cri.PullImage(pullCtx, image)
		done <- pullOutcome{ref: ref, err: err}
	}()

	if s.cfg.DisablePullStallWatch {
		out := <-done
		return out.ref, out.err
	}
	return s.watchPullProgress(ctx, cancel, done)
}

// watchPullProgress samples ImageFS usage while the pull runs and cancels it
// once nothing has changed for the stall window. Sampling errors fail open: a
// watchdog that cannot see the filesystem must never fail a healthy pull.
// The signal is node-wide, so a parallel pull on the same node can mask a
// stalled one; pulls are throttled per node by the reconciler and sharing the
// image filesystem is exactly what the guard is about.
func (s *Server) watchPullProgress(ctx context.Context, cancel context.CancelFunc, done <-chan pullOutcome) (string, error) {
	stall := s.cfg.PullStallTimeout
	interval := stallPollInterval
	if quarter := stall / 4; quarter < interval {
		interval = quarter
	}
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	used, sampled := s.imageFSUsed(ctx)
	lastProgress := time.Now()
	for {
		select {
		case out := <-done:
			return out.ref, out.err
		case <-ticker.C:
			current, ok := s.imageFSUsed(ctx)
			if !ok || !sampled {
				used, sampled, lastProgress = current, ok, time.Now()
				continue
			}
			if current != used {
				used, lastProgress = current, time.Now()
				continue
			}
			if time.Since(lastProgress) < stall {
				continue
			}
			cancel()
			select {
			case <-done:
			case <-time.After(stallCancelGrace):
			}
			return "", fmt.Errorf("%w: no image filesystem growth for %s", errPullStalled, stall)
		}
	}
}

// imageFSUsed samples image filesystem usage for the stall watchdog. The second
// return value is false when the runtime could not answer.
func (s *Server) imageFSUsed(ctx context.Context) (int64, bool) {
	fsCtx, cancel := context.WithTimeout(ctx, stallPollInterval)
	defer cancel()
	fs, err := s.cri.ImageFS(fsCtx)
	if err != nil {
		return 0, false
	}
	return fs.UsedBytes, true
}

// observe records every API request and its status code. The middleware is
// outside the mux, so the 401 produced by withAuth is counted too: a sudden
// spike of 401 is token rotation, a flat line of 0 is a dead link.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		s.metrics.ObserveRequest(endpointForPath(r.URL.Path), rec.Status())
	})
}

// statusRecorder captures the status code written by a handler without
// changing any of the ResponseWriter behavior the handlers rely on.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

// Status reports the captured code; a handler that wrote nothing is a 200.
func (r *statusRecorder) Status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

// endpointForPath maps an API path to the small endpoint label set of the
// agent metrics. The images route is a subtree, everything else is exact.
func endpointForPath(path string) string {
	switch {
	case path == healthzPath:
		return endpointHealthz
	case path == snapshotPath:
		return endpointSnapshot
	case strings.HasPrefix(path, imagesPath):
		return endpointImages
	case path == pullsPath:
		return endpointPulls
	default:
		return endpointOther
	}
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	if s.cfg.Token == "" {
		return next
	}
	expected := []byte(s.cfg.Token)
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(token), expected) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

func writeJSON(w http.ResponseWriter, statusCode int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]string{"error": message})
}
