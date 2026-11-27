package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

const (
	// maxSpokeStateBody bounds a state push: it carries the whole cluster
	// snapshot, which grows with the number of workloads and nodes.
	maxSpokeStateBody = 32 << 20
	// maxSpokeCommandBody bounds a command result body.
	maxSpokeCommandBody = 8 << 20
)

// registerSpokeHubRoutes mounts the hub side of the outbound spoke channel.
// These routes are the only inbound surface a managed cluster needs: it dials
// the hub ingress, pushes state and long-polls for commands, so no route into
// the managed cluster is required.
//
// Every route authenticates the caller by the per-cluster token registered with
// SpokeHub.AddCluster (auth.RequireSpokeToken), never by a hub cookie.
func (s *Server) registerSpokeHubRoutes() {
	if s.spokeHub == nil {
		return
	}
	lookup := auth.SpokeTokenLookup(s.spokeHub.Token)

	s.mux.HandleFunc("POST /internal/v1/spokes/{cluster}/state", auth.RequireSpokeToken(lookup, s.handleSpokeState))
	s.mux.HandleFunc("GET /internal/v1/spokes/{cluster}/commands", auth.RequireSpokeToken(lookup, s.handleSpokeCommands))
	s.mux.HandleFunc("POST /internal/v1/spokes/{cluster}/commands/{id}/result", auth.RequireSpokeToken(lookup, s.handleSpokeCommandResult))
}

// handleSpokeState accepts one periodic state push (snapshot + counters).
func (s *Server) handleSpokeState(w http.ResponseWriter, r *http.Request) {
	var state provider.SpokeState
	if !decodeLimitedJSON(w, r, &state, maxSpokeStateBody) {
		return
	}
	if !validAPIVersion(state.APIVersion) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported apiVersion " + state.APIVersion})
		return
	}
	if err := s.spokeHub.AcceptState(r.PathValue("cluster"), state); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, provider.SpokeStateAck{APIVersion: model.APIVersion, OK: true})
}

// handleSpokeCommands long-polls the command mailbox of one managed cluster.
// It answers 204 when nothing is queued before the hub's wait expires.
func (s *Server) handleSpokeCommands(w http.ResponseWriter, r *http.Request) {
	commands, err := s.spokeHub.PollCommands(r.Context(), r.PathValue("cluster"))
	if err != nil {
		if r.Context().Err() != nil {
			// The spoke gave up on this poll (shutdown, network) - nobody to answer.
			return
		}
		if errors.Is(err, provider.ErrUnknownSpokeCluster) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown managed cluster"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "command poll failed"})
		return
	}
	if len(commands) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, provider.SpokeCommands{Commands: commands})
}

// handleSpokeCommandResult accepts the recorded HTTP answer of one command. An
// id the hub already gave up on is a no-op, so redelivery stays idempotent.
func (s *Server) handleSpokeCommandResult(w http.ResponseWriter, r *http.Request) {
	var result provider.SpokeCommandResult
	if !decodeLimitedJSON(w, r, &result, maxSpokeCommandBody) {
		return
	}
	if err := s.spokeHub.AcceptResult(r.PathValue("cluster"), r.PathValue("id"), result); err != nil {
		if errors.Is(err, provider.ErrUnknownSpokeCluster) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown managed cluster"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// decodeLimitedJSON reads at most limit JSON bytes into dst. It writes the error
// response itself and reports whether the caller may continue. An empty body is
// rejected: unlike the spoke contract there is no pre-v1 client on this channel.
func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	err := json.NewDecoder(r.Body).Decode(dst)
	switch {
	case err == nil:
		return true
	case errors.Is(err, io.EOF):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty request body"})
		return false
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
	return false
}

