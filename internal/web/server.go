package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

//go:embed static/*
var staticFiles embed.FS

// Server is the hub HTTP surface: Web UI, cookie-authenticated JSON API and the
// multi-cluster registry. The headless spoke surface lives in server_spoke.go.
type Server struct {
	registry *provider.Registry
	auth     *auth.Manager
	mux      *http.ServeMux
}

func NewServer(reg *provider.Registry, a *auth.Manager) (*Server, error) {
	s := &Server{
		registry: reg,
		auth:     a,
		mux:      http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Auth endpoints
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/v1/auth/me", s.handleMe)

	// Protected multi-cluster API endpoints
	s.mux.HandleFunc("GET /api/v1/clusters", auth.RequireAuth(s.auth, s.handleClusters))
	s.mux.HandleFunc("GET /api/v1/snapshot", auth.RequireAuth(s.auth, s.handleSnapshot))
	s.mux.HandleFunc("POST /api/v1/pull", auth.RequireAuth(s.auth, s.handleManualPull))
	s.mux.HandleFunc("POST /api/v1/workloads/pull", auth.RequireAuth(s.auth, s.handleWorkloadPull))
	s.mux.HandleFunc("POST /api/v1/reconcile", auth.RequireAuth(s.auth, s.handleReconcile))

	// Contract v2 stubs consumed by WS-D (UI), WS-C (reconciler) and WS-H.
	s.mux.HandleFunc("GET /api/v1/pull-tasks", auth.RequireAuth(s.auth, s.handlePullTasks))
	s.mux.HandleFunc("POST /api/v1/pulls/{id}/cancel", auth.RequireAuth(s.auth, s.handleCancelPull))
	s.mux.HandleFunc("GET /api/v1/nodes/{node}/images", auth.RequireAuth(s.auth, s.handleNodeImages))
	s.mux.HandleFunc("GET /api/v1/health", auth.RequireAuth(s.auth, s.handleHealth))

	// Embedded static Web UI
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	s.mux.Handle("/", http.FileServer(http.FS(sub)))

	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if !s.auth.ValidateCredentials(strings.TrimSpace(req.Username), req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "неверный логин или пароль"})
		return
	}
	s.auth.IssueCookie(w, r, strings.TrimSpace(req.Username))
	writeJSON(w, http.StatusOK, map[string]any{"username": strings.TrimSpace(req.Username)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.ClearCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := s.auth.AuthenticateRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": user})
}

func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request, _ string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"defaultCluster": s.registry.DefaultID(),
		"apiVersion":     model.APIVersion,
		"clusters":       s.registry.List(r.Context()),
	})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request, _ string) {
	clusterID := r.URL.Query().Get("cluster")
	entry, ok := s.registry.Get(clusterID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "cluster not found"})
		return
	}
	writeJSON(w, http.StatusOK, entry.Provider.Snapshot(r.Context()))
}

func (s *Server) handleManualPull(w http.ResponseWriter, r *http.Request, user string) {
	var req struct {
		Cluster string `json:"cluster"`
		Image   string `json:"image"`
		Node    string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Image == "" || req.Node == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "image and node are required"})
		return
	}
	task, err := s.registry.TriggerPull(r.Context(), req.Cluster, model.PullRequest{
		Image:   req.Image,
		Node:    req.Node,
		Actor:   user,
		Trigger: model.TriggerManual,
	})
	if err != nil {
		writeJSON(w, pullErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "pulling",
		"image":  req.Image,
		"node":   req.Node,
		"task":   task,
	})
}

func (s *Server) handleWorkloadPull(w http.ResponseWriter, r *http.Request, user string) {
	var req struct {
		Cluster   string `json:"cluster"`
		Namespace string `json:"namespace"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	count, err := s.registry.TriggerWorkloadPull(r.Context(), req.Cluster, model.WorkloadPullRequest{
		Namespace: req.Namespace,
		Kind:      req.Kind,
		Name:      req.Name,
		Actor:     user,
		Trigger:   model.TriggerManual,
	})
	if err != nil {
		writeJSON(w, pullErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"triggered": count})
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request, user string) {
	var req struct {
		Cluster string `json:"cluster"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := s.registry.SyncNow(r.Context(), req.Cluster, user); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePullTasks returns the recent asynchronous pull tasks of one cluster.
func (s *Server) handlePullTasks(w http.ResponseWriter, r *http.Request, _ string) {
	entry, ok := s.registry.Get(r.URL.Query().Get("cluster"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "cluster not found"})
		return
	}
	snap := entry.Provider.Snapshot(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"cluster": entry.ID,
		"tasks":   snap.Tasks,
	})
}

// handleCancelPull cancels one queued or running pull task.
func (s *Server) handleCancelPull(w http.ResponseWriter, r *http.Request, _ string) {
	taskID := r.PathValue("id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task id is required"})
		return
	}
	cluster := r.URL.Query().Get("cluster")
	if cluster == "" {
		var req struct {
			Cluster string `json:"cluster"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		cluster = req.Cluster
	}
	if err := s.registry.CancelPull(r.Context(), cluster, taskID); err != nil {
		writeJSON(w, pullErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"taskId": taskID, "status": "canceled"})
}

// handleNodeImages returns cached images and ImageFS numbers for one node.
func (s *Server) handleNodeImages(w http.ResponseWriter, r *http.Request, _ string) {
	nodeName := r.PathValue("node")
	entry, ok := s.registry.Get(r.URL.Query().Get("cluster"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "cluster not found"})
		return
	}
	snap := entry.Provider.Snapshot(r.Context())
	for _, n := range snap.Nodes {
		if n.Name != nodeName {
			continue
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"cluster":      entry.ID,
			"node":         n.Name,
			"ready":        n.Ready,
			"agentUp":      n.AgentUp,
			"agentVersion": n.AgentVersion,
			"conditions":   n.Conditions,
			"diskPaused":   n.DiskPaused,
			"imageFsUsed":  n.ImageFSUsedBytes,
			"imageFsTotal": n.ImageFSCapacityBytes,
			"cachedImages": n.CachedImages,
			"lastSeen":     n.LastSeen,
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
}

// handleHealth reports the provider liveness view for one cluster.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request, _ string) {
	health, err := s.registry.Health(r.Context(), r.URL.Query().Get("cluster"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, health)
}

// pullErrorStatus maps errors from the pull layer onto HTTP statuses so the UI
// can tell a node whose disk is above the pause threshold (409), an unreachable
// or unauthorized node agent (502) and genuinely bad input (400) apart instead
// of collapsing everything into 400.
func pullErrorStatus(err error) int {
	switch {
	case errors.Is(err, provider.ErrTaskNotFound):
		return http.StatusNotFound
	case errors.Is(err, provider.ErrCancelNotSupported):
		return http.StatusConflict
	case errors.Is(err, provider.ErrAgentDiskPaused):
		return http.StatusConflict
	case errors.Is(err, provider.ErrAgentUnauthorized):
		return http.StatusBadGateway
	case errors.Is(err, provider.ErrAgentUnavailable):
		return http.StatusBadGateway
	case errors.Is(err, provider.ErrNotImplemented):
		return http.StatusNotImplemented
	default:
		return http.StatusBadRequest
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
