package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

// NewSpokeServer creates a headless backend-only HTTP server for remote spoke
// clusters: no Web UI, only /healthz, /readyz and Bearer-protected
// /internal/v1/* endpoints (contract v2).
func NewSpokeServer(p provider.Provider, clusterToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", spokeHealth)
	mux.HandleFunc("GET /readyz", spokeHealth)

	requireToken := func(next http.HandlerFunc) http.HandlerFunc {
		return auth.RequireToken(clusterToken, next)
	}

	mux.HandleFunc("GET /internal/v1/snapshot", requireToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Snapshot(r.Context()))
	}))

	mux.HandleFunc("POST /internal/v1/pull", requireToken(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIVersion string            `json:"apiVersion"`
			Image      string            `json:"image"`
			Node       string            `json:"node"`
			Actor      string            `json:"actor"`
			Trigger    model.PullTrigger `json:"trigger"`
			Policy     model.PullPolicy  `json:"policy"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if !validAPIVersion(req.APIVersion) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported apiVersion " + req.APIVersion})
			return
		}
		task, err := p.TriggerPull(r.Context(), model.PullRequest{
			Image:   req.Image,
			Node:    req.Node,
			Actor:   req.Actor,
			Trigger: req.Trigger,
			Policy:  req.Policy,
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, task)
	}))

	mux.HandleFunc("POST /internal/v1/workloads/pull", requireToken(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIVersion string            `json:"apiVersion"`
			Namespace  string            `json:"namespace"`
			Kind       string            `json:"kind"`
			Name       string            `json:"name"`
			Actor      string            `json:"actor"`
			Trigger    model.PullTrigger `json:"trigger"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if !validAPIVersion(req.APIVersion) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported apiVersion " + req.APIVersion})
			return
		}
		n, err := p.TriggerWorkloadPull(r.Context(), model.WorkloadPullRequest{
			Namespace: req.Namespace,
			Kind:      req.Kind,
			Name:      req.Name,
			Actor:     req.Actor,
			Trigger:   req.Trigger,
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]int{"triggered": n})
	}))

	mux.HandleFunc("POST /internal/v1/reconcile", requireToken(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIVersion string `json:"apiVersion"`
			Actor      string `json:"actor"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !validAPIVersion(req.APIVersion) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported apiVersion " + req.APIVersion})
			return
		}
		if err := p.SyncNow(r.Context(), req.Actor); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))

	// GET /internal/v1/counters exposes pull counters so the hub can export
	// prepuller_pull_total for remote clusters (contract v2).
	mux.HandleFunc("GET /internal/v1/counters", requireToken(func(w http.ResponseWriter, r *http.Request) {
		counters := p.PullCounters()
		if counters == nil {
			counters = []model.PullMetricSample{}
		}
		writeJSON(w, http.StatusOK, counters)
	}))

	// POST /internal/v1/pulls/{id}/cancel cancels a queued or running task.
	mux.HandleFunc("POST /internal/v1/pulls/{id}/cancel", requireToken(func(w http.ResponseWriter, r *http.Request) {
		taskID := r.PathValue("id")
		if taskID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task id is required"})
			return
		}
		if err := p.CancelPull(r.Context(), taskID); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, provider.ErrTaskNotFound) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"taskId": taskID, "status": "canceled"})
	}))

	return mux
}

func spokeHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Prepuller-Version", model.APIVersion)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// validAPIVersion accepts the current contract, the previous one during the
// rollout window, and an omitted version (older clients).
func validAPIVersion(v string) bool {
	return v == "" || v == model.APIVersion || v == model.PreviousAPIVersion
}
