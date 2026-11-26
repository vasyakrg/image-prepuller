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
	pullTimeout     = 10 * time.Minute
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
}

// NewServer builds the agent HTTP server. cri is the CRI backend, real or fake.
func NewServer(cfg Config, cri CRI) (*Server, error) {
	if cri == nil {
		return nil, errors.New("agent: nil CRI backend")
	}
	s := &Server{cfg: cfg, cri: cri}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthzPath, s.handleHealth)
	mux.HandleFunc("GET "+snapshotPath, s.withAuth(s.handleSnapshot))
	mux.HandleFunc("GET "+imagesPath+"{ref...}", s.withAuth(s.handleImageStatus))
	mux.HandleFunc("POST "+pullsPath, s.withAuth(s.handlePull))
	s.handler = mux
	return s, nil
}

// Handler exposes the router for tests and for embedding behind another server.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve listens on cfg.ListenAddr and serves until ctx is canceled, then
// shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("agent: listen %s: %w", s.cfg.ListenAddr, err)
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("agent: shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("agent: serve: %w", err)
	}
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
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
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
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"auto-prepull paused: imagefs usage %.1f%% >= %.1f%% threshold",
			UsedRatio(fs.UsedBytes, fs.CapacityBytes)*100, s.cfg.DiskPauseRatio*100))
		return
	}

	pullCtx, cancel := context.WithTimeout(r.Context(), pullTimeout)
	defer cancel()
	imageRef, err := s.cri.PullImage(pullCtx, image)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, PullResult{
		APIVersion: WireAPIVersion,
		Image:      image,
		ImageRef:   imageRef,
		PulledAt:   time.Now().UTC(),
	})
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
