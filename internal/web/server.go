package web

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/provider"
)

//go:embed static/*
var staticFiles embed.FS

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
	s.mux.HandleFunc("GET /api/v1/clusters", s.requireAuth(s.handleClusters))
	s.mux.HandleFunc("GET /api/v1/snapshot", s.requireAuth(s.handleSnapshot))
	s.mux.HandleFunc("POST /api/v1/pull", s.requireAuth(s.handleManualPull))
	s.mux.HandleFunc("POST /api/v1/workloads/pull", s.requireAuth(s.handleWorkloadPull))
	s.mux.HandleFunc("POST /api/v1/reconcile", s.requireAuth(s.handleReconcile))

	// Embedded static Web UI
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	s.mux.Handle("/", http.FileServer(http.FS(sub)))

	return s, nil
}

// NewSpokeServer creates a headless backend-only HTTP server for remote spoke clusters
// (no Web UI; only /healthz, /readyz, and Bearer-protected /internal/v1/* endpoints).
func NewSpokeServer(p provider.Provider, clusterToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	requireToken := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if clusterToken != "" {
				got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				if subtle.ConstantTimeCompare([]byte(got), []byte(clusterToken)) != 1 {
					writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid cluster token"})
					return
				}
			}
			next(w, r)
		}
	}

	mux.HandleFunc("GET /internal/v1/snapshot", requireToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Snapshot(r.Context()))
	}))
	mux.HandleFunc("POST /internal/v1/pull", requireToken(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Image string `json:"image"`
			Node  string `json:"node"`
			Actor string `json:"actor"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if err := p.TriggerPull(r.Context(), req.Image, req.Node, req.Actor); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("POST /internal/v1/workloads/pull", requireToken(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Namespace string `json:"namespace"`
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Actor     string `json:"actor"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		n, err := p.TriggerWorkloadPull(r.Context(), req.Namespace, req.Kind, req.Name, req.Actor)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]int{"triggered": n})
	}))
	mux.HandleFunc("POST /internal/v1/reconcile", requireToken(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Actor string `json:"actor"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := p.SyncNow(r.Context(), req.Actor); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))

	return mux
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

type authedHandler func(w http.ResponseWriter, r *http.Request, user string)

func (s *Server) requireAuth(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.auth.AuthenticateRequest(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r, user)
	}
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
	if err := s.registry.TriggerPull(r.Context(), req.Cluster, req.Image, req.Node, user); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "pulling", "image": req.Image, "node": req.Node})
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
	count, err := s.registry.TriggerWorkloadPull(r.Context(), req.Cluster, req.Namespace, req.Kind, req.Name, user)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
