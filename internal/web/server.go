package web

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
	"github.com/local/image-prepuller/internal/tasks"
)

//go:embed static/*
var staticFiles embed.FS

// Server is the hub HTTP surface: Web UI, cookie-authenticated JSON API and the
// multi-cluster registry. The headless spoke surface lives in server_spoke.go.
type Server struct {
	registry *provider.Registry
	auth     *auth.Manager
	spokeHub *provider.SpokeHub
	history  *tasks.Store
	stats    StatsSource
	mux      *http.ServeMux
}

// Option customizes the hub HTTP server.
type Option func(*Server)

// WithSpokeHub enables the hub side of the outbound spoke channel: managed
// clusters that dial this hub's ingress instead of being reachable over HTTP
// (see provider.SpokeHub and server_spoke_hub.go).
func WithSpokeHub(h *provider.SpokeHub) Option {
	return func(s *Server) { s.spokeHub = h }
}

// WithTaskHistory makes the hub keep a bounded pull-task history and serve it
// from GET /api/v1/pull-tasks with server-side pagination. Without it the
// endpoint keeps answering from the provider snapshot (the pre-history
// behaviour). A nil store is the same as not passing the option.
func WithTaskHistory(h *tasks.Store) Option {
	return func(s *Server) { s.history = h }
}

func NewServer(reg *provider.Registry, a *auth.Manager, opts ...Option) (*Server, error) {
	s := &Server{
		registry: reg,
		auth:     a,
		mux:      http.NewServeMux(),
	}
	for _, opt := range opts {
		opt(s)
	}

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Auth endpoints. Login is rate limited (429 + Retry-After) per IP and username.
	s.mux.HandleFunc("POST /api/v1/auth/login", s.auth.LoginMiddleware(s.handleLogin))
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/v1/auth/me", s.handleMe)

	// Protected read-only endpoints: any authenticated role (viewer included).
	s.mux.HandleFunc("GET /api/v1/clusters", auth.RequireAuth(s.auth, s.handleClusters))
	s.mux.HandleFunc("GET /api/v1/snapshot", auth.RequireAuth(s.auth, s.handleSnapshot))

	// State-changing endpoints: operator only (viewer gets 403).
	s.mux.HandleFunc("POST /api/v1/pull", auth.RequireOperator(s.auth)(s.handleManualPull))
	s.mux.HandleFunc("POST /api/v1/workloads/pull", auth.RequireOperator(s.auth)(s.handleWorkloadPull))
	s.mux.HandleFunc("POST /api/v1/reconcile", auth.RequireOperator(s.auth)(s.handleReconcile))

	// Contract v2 stubs consumed by WS-D (UI), WS-C (reconciler) and WS-H.
	s.mux.HandleFunc("GET /api/v1/pull-tasks", auth.RequireAuth(s.auth, s.handlePullTasks))
	s.mux.HandleFunc("POST /api/v1/pulls/{id}/cancel", auth.RequireOperator(s.auth)(s.handleCancelPull))
	s.mux.HandleFunc("GET /api/v1/nodes/{node}/images", auth.RequireAuth(s.auth, s.handleNodeImages))
	s.mux.HandleFunc("GET /api/v1/health", auth.RequireAuth(s.auth, s.handleHealth))

	// Operator-only feed of the recent security audit events.
	s.mux.HandleFunc("GET /api/v1/audit", auth.RequireOperator(s.auth)(s.handleAudit))

	// Read-only statistics tab over the optional PostgreSQL pipeline (ADR-002).
	// Authenticated like every other hub endpoint; a disabled pipeline answers the
	// tab's "administrator disabled it" state from /stats/status instead of data.
	s.mux.HandleFunc("GET /api/v1/stats/status", auth.RequireAuth(s.auth, s.handleStatsStatus))
	s.mux.HandleFunc("GET /api/v1/stats/overview", auth.RequireAuth(s.auth, s.handleStatsOverview))
	s.mux.HandleFunc("GET /api/v1/stats/nodes", auth.RequireAuth(s.auth, s.handleStatsNodes))
	s.mux.HandleFunc("GET /api/v1/stats/images", auth.RequireAuth(s.auth, s.handleStatsImages))
	s.mux.HandleFunc("GET /api/v1/stats/gc", auth.RequireAuth(s.auth, s.handleStatsGCEvents))
	s.mux.HandleFunc("GET /api/v1/stats/repulls", auth.RequireAuth(s.auth, s.handleStatsRepulls))
	s.mux.HandleFunc("GET /api/v1/stats/tasks", auth.RequireAuth(s.auth, s.handleStatsTasks))

	// Inbound endpoints of managed clusters that connect outbound (optional).
	s.registerSpokeHubRoutes()

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
	sess, ok := s.auth.Login(r, strings.TrimSpace(req.Username), req.Password)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "неверный логин или пароль"})
		return
	}
	s.auth.IssueCookie(w, r, sess.Username)
	writeJSON(w, http.StatusOK, map[string]any{"username": sess.Username, "role": sess.Role})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Audit(r, auth.ActionLogout, "", auth.ResultOK)
	s.auth.ClearCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auth.AuthenticateSession(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": sess.Username, "role": string(sess.Role)})
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
	snap := entry.Provider.Snapshot(r.Context())
	snap.Normalize()
	// Every snapshot is also a feed for the bounded hub-side pull-task history.
	if s.history != nil {
		s.history.Observe(entry.ID, snap.Tasks)
	}
	writeJSON(w, http.StatusOK, snap)
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
		s.auth.Audit(r, auth.ActionPull, req.Image+"@"+req.Node, auth.ResultError)
		writeJSON(w, pullErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	s.auth.Audit(r, auth.ActionPull, req.Image+"@"+req.Node, auth.ResultOK)
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
		s.auth.Audit(r, auth.ActionWorkloadPull, req.Namespace+"/"+req.Kind+"/"+req.Name, auth.ResultError)
		writeJSON(w, pullErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	s.auth.Audit(r, auth.ActionWorkloadPull, req.Namespace+"/"+req.Kind+"/"+req.Name, auth.ResultOK)
	writeJSON(w, http.StatusAccepted, map[string]any{"triggered": count})
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request, user string) {
	var req struct {
		Cluster string `json:"cluster"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := s.registry.SyncNow(r.Context(), req.Cluster, user); err != nil {
		s.auth.Audit(r, auth.ActionReconcile, req.Cluster, auth.ResultError)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.auth.Audit(r, auth.ActionReconcile, req.Cluster, auth.ResultOK)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Pagination and state filter of GET /api/v1/pull-tasks.
const (
	defaultPullTasksLimit = 50
	maxPullTasksLimit     = 200
)

// validTaskFilters are the state values handlePullTasks accepts; "" means all.
var validTaskFilters = map[string]bool{
	"":                          true,
	"all":                       true,
	"active":                    true,
	string(model.TaskQueued):    true,
	string(model.TaskRunning):   true,
	string(model.TaskSucceeded): true,
	string(model.TaskFailed):    true,
	string(model.TaskCanceled):  true,
}

// handlePullTasks returns one page of the asynchronous pull tasks of one
// cluster. With a task history (web.WithTaskHistory) the answer comes from the
// bounded hub-side store, so it survives a provider restart and is paginated;
// without one it falls back to the provider snapshot, as before.
func (s *Server) handlePullTasks(w http.ResponseWriter, r *http.Request, _ string) {
	query := r.URL.Query()
	entry, ok := s.registry.Get(query.Get("cluster"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "cluster not found"})
		return
	}

	limit, err := intQueryParam(query.Get("limit"), defaultPullTasksLimit)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid limit"})
		return
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxPullTasksLimit {
		limit = maxPullTasksLimit
	}
	offset, err := intQueryParam(query.Get("offset"), 0)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid offset"})
		return
	}
	if offset < 0 {
		offset = 0
	}
	state := strings.TrimSpace(query.Get("state"))
	if !validTaskFilters[state] {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid state (expected all, active, queued, running, succeeded, failed or canceled)",
		})
		return
	}

	snap := entry.Provider.Snapshot(r.Context())
	snap.Normalize()
	if s.history != nil {
		// A poll of the task list is also a feed: the history keeps the tasks the
		// provider no longer returns once they fall out of its own bounded list.
		s.history.Observe(entry.ID, snap.Tasks)
		page, total, active := s.history.Query(entry.ID, state, offset, limit)
		writeJSON(w, http.StatusOK, map[string]any{
			"cluster": entry.ID,
			"tasks":   page,
			"total":   total,
			"limit":   limit,
			"offset":  offset,
			"active":  active,
			"source":  "history",
		})
		return
	}

	all := filterTaskState(snap.Tasks, state)
	active := 0
	for _, task := range snap.Tasks {
		if task.State.Active() {
			active++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cluster": entry.ID,
		"tasks":   paginateTasks(all, offset, limit),
		"total":   len(all),
		"limit":   limit,
		"offset":  offset,
		"active":  active,
		"source":  "snapshot",
	})
}

// intQueryParam parses an optional integer query parameter, returning fallback
// when it is absent.
func intQueryParam(raw string, fallback int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	return strconv.Atoi(strings.TrimSpace(raw))
}

// filterTaskState applies the state filter of handlePullTasks to a task slice.
func filterTaskState(tasks []model.PullTask, state string) []model.PullTask {
	out := make([]model.PullTask, 0, len(tasks))
	for _, task := range tasks {
		switch state {
		case "", "all":
			out = append(out, task)
		case "active":
			if task.State.Active() {
				out = append(out, task)
			}
		default:
			if string(task.State) == state {
				out = append(out, task)
			}
		}
	}
	return out
}

// paginateTasks slices one page out of a newest-first task list.
func paginateTasks(tasks []model.PullTask, offset, limit int) []model.PullTask {
	if offset >= len(tasks) {
		return []model.PullTask{}
	}
	end := offset + limit
	if end > len(tasks) {
		end = len(tasks)
	}
	return tasks[offset:end]
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
		s.auth.Audit(r, auth.ActionCancelPull, taskID, auth.ResultError)
		writeJSON(w, pullErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	s.auth.Audit(r, auth.ActionCancelPull, taskID, auth.ResultOK)
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
			// True when ImageFS reported used bytes only (Talos) and the capacity
			// is the node's ephemeral-storage: the UI renders it as "~" total.
			"imageFsCapacityEstimated": n.ImageFSCapacityEstimated,
			"cachedImages":             n.CachedImages,
			"lastSeen":                 n.LastSeen,
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

// handleAudit returns the recent security audit events to operators.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, _ string) {
	events := s.auth.RecentAudit()
	if events == nil {
		events = []auth.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
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
	case errors.Is(err, provider.ErrAgentPullStalled):
		return http.StatusGatewayTimeout
	case errors.Is(err, provider.ErrAgentRegistryAuth):
		return http.StatusBadGateway
	case errors.Is(err, provider.ErrAgentUnavailable):
		return http.StatusBadGateway
	case errors.Is(err, provider.ErrSpokeUnauthorized):
		return http.StatusBadGateway
	case errors.Is(err, provider.ErrSpokeUnavailable):
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
