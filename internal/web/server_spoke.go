package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

const (
	// maxSpokeRequestBody bounds every JSON request body accepted by the spoke
	// API; a larger body is answered with 413 instead of being buffered.
	maxSpokeRequestBody = 1 << 20
	// spokeVersion is reported by /internal/v1/health and in the
	// X-Prepuller-Version header of /healthz.
	spokeVersion = model.APIVersion
)

// spokeHealthView is the /internal/v1/health payload: model.Health plus the
// wire contract version and the process uptime. It is answered from memory and
// never calls Provider.Snapshot, so a busy or broken cluster backend cannot
// make the liveness probe expensive.
type spokeHealthView struct {
	model.Health
	APIVersion    string `json:"apiVersion"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}

// spokeRequest is the common apiVersion prelude of every spoke request body.
type spokeRequest struct {
	APIVersion string `json:"apiVersion"`
}

// NewSpokeServer creates a headless backend-only HTTP server for remote spoke
// clusters: no Web UI, only /healthz, /readyz and Bearer-protected
// /internal/v1/* endpoints (contract v2).
func NewSpokeServer(p provider.Provider, clusterToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", spokeHealth)
	mux.HandleFunc("GET /readyz", spokeHealth)

	startedAt := time.Now()

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
		if !decodeSpokeBody(w, r, &req) || rejectUnsupportedAPIVersion(w, req.APIVersion) {
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
			status := pullErrorStatus(err)
			if status >= http.StatusInternalServerError {
				logSpokeError(r, "trigger pull", err)
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
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
		if !decodeSpokeBody(w, r, &req) || rejectUnsupportedAPIVersion(w, req.APIVersion) {
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
			writeJSON(w, pullErrorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]int{"triggered": n})
	}))

	mux.HandleFunc("POST /internal/v1/reconcile", requireToken(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIVersion string `json:"apiVersion"`
			Actor      string `json:"actor"`
		}
		if !decodeSpokeBody(w, r, &req) || rejectUnsupportedAPIVersion(w, req.APIVersion) {
			return
		}
		if err := p.SyncNow(r.Context(), req.Actor); err != nil {
			logSpokeError(r, "sync", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))

	// GET /internal/v1/counters exposes pull counters so the hub can export
	// prepuller_pull_total for remote clusters (contract v2). The answer is
	// always 200 with every field set, even on an idle cluster.
	mux.HandleFunc("GET /internal/v1/counters", requireToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, spokeCounters(r.Context(), p))
	}))

	// GET /internal/v1/health is the cheap liveness/version view used by
	// Provider.Health on the hub. It must not touch Snapshot.
	mux.HandleFunc("GET /internal/v1/health", requireToken(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, spokeHealthView{
			Health: model.Health{
				Online:    true,
				Version:   spokeVersion,
				CheckedAt: time.Now(),
			},
			APIVersion:    model.APIVersion,
			UptimeSeconds: int64(time.Since(startedAt).Seconds()),
		})
	}))

	// POST /internal/v1/pulls/{id}/cancel cancels a queued or running task.
	// 202 success, 404 unknown task, 409 already finished; a repeated cancel is
	// a 409, never a 500.
	mux.HandleFunc("POST /internal/v1/pulls/{id}/cancel", requireToken(func(w http.ResponseWriter, r *http.Request) {
		taskID := r.PathValue("id")
		if taskID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task id is required"})
			return
		}
		var req spokeRequest
		if !decodeSpokeBody(w, r, &req) || rejectUnsupportedAPIVersion(w, req.APIVersion) {
			return
		}
		err := p.CancelPull(r.Context(), taskID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusAccepted, map[string]any{"taskId": taskID, "status": "canceled"})
		case errors.Is(err, provider.ErrTaskNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, provider.ErrTaskNotActive), errors.Is(err, provider.ErrCancelNotSupported), spokeTaskFinished(r.Context(), p, taskID):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "pull task " + taskID + " is already finished"})
		default:
			logSpokeError(r, "cancel pull task", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to cancel pull task"})
		}
	}))

	return mux
}

// decodeSpokeBody reads at most maxSpokeRequestBody JSON bytes into dst. It
// writes the error response itself and reports whether the caller may continue.
// An empty body is accepted (older clients send no apiVersion); unknown fields
// are ignored on purpose so a newer hub can add fields without breaking an old
// spoke.
func decodeSpokeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxSpokeRequestBody)
	err := json.NewDecoder(r.Body).Decode(dst)
	switch {
	case err == nil:
		return true
	case errors.Is(err, io.EOF):
		return true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
	return false
}

// rejectUnsupportedAPIVersion answers 400 for an unknown wire version and
// reports whether the request was rejected.
func rejectUnsupportedAPIVersion(w http.ResponseWriter, apiVersion string) bool {
	if validAPIVersion(apiVersion) {
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported apiVersion " + apiVersion})
	return true
}

// spokeTaskFinished reports whether a task exists and already reached a terminal
// state, which lets the handler answer 409 for providers that return an untyped
// "already finished" error.
func spokeTaskFinished(ctx context.Context, p provider.Provider, taskID string) bool {
	for _, task := range p.Snapshot(ctx).Tasks {
		if task.ID == taskID {
			return !task.State.Active()
		}
	}
	return false
}

// spokeCounters builds the aggregate counters answer from the provider series
// and the current task summary.
func spokeCounters(ctx context.Context, p provider.Provider) provider.SpokeCounters {
	out := provider.SpokeCounters{Samples: []model.PullMetricSample{}}
	for _, sample := range p.PullCounters() {
		if sample.Count == 0 {
			continue
		}
		out.PullTotal += sample.Count
		switch strings.ToLower(sample.Result) {
		case "success", "succeeded":
			out.PullSucceeded += sample.Count
		case "error", "failed", "failure":
			out.PullFailed += sample.Count
		case "canceled", "cancelled":
			out.PullCanceled += sample.Count
		}
		out.Samples = append(out.Samples, sample)
	}

	snap := p.Snapshot(ctx)
	out.PullInFlight = snap.Summary.RunningTasks + snap.Summary.QueuedTasks

	// Canceled tasks are not part of the per-result pull series on every
	// provider, so fall back to the retained task list and keep the total
	// consistent with the aggregate fields.
	var canceledTasks uint64
	for _, task := range snap.Tasks {
		if task.State == model.TaskCanceled {
			canceledTasks++
		}
	}
	if canceledTasks > out.PullCanceled {
		out.PullTotal += canceledTasks - out.PullCanceled
		out.PullCanceled = canceledTasks
	}
	out.ImagesCached = cachedImageCount(snap)
	out.LastSyncAt = snap.LastSyncAt
	if out.LastSyncAt.IsZero() {
		out.LastSyncAt = snap.UpdatedAt
	}
	if out.LastSyncAt.IsZero() {
		out.LastSyncAt = time.Now()
	}
	return out
}

// cachedImageCount counts images present on at least one node.
func cachedImageCount(snap model.Snapshot) int {
	unique := make(map[string]struct{})
	for _, workload := range snap.Workloads {
		for _, image := range workload.Images {
			for _, node := range image.Nodes {
				if node.State == model.StatePresent {
					unique[image.Image] = struct{}{}
					break
				}
			}
		}
	}
	return len(unique)
}

// logSpokeError logs a 5xx cause without request bodies, tokens or headers.
func logSpokeError(r *http.Request, action string, err error) {
	log.Printf("spoke: %s: %s %s: %v", action, r.Method, r.URL.Path, err)
}

func spokeHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Prepuller-Version", spokeVersion)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// validAPIVersion accepts the current contract, the previous one during the
// rollout window, and an omitted version (older clients).
func validAPIVersion(v string) bool {
	return v == "" || v == model.APIVersion || v == model.PreviousAPIVersion
}
