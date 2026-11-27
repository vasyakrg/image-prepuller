package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// The statistics tab is served by the optional PostgreSQL pipeline (ADR-002).
// internal/web is stdlib-only and must not import internal/stats (that would
// pull pgx in), so the read side arrives as this port; main wires
// stats.Service, tests wire a fake. The port is deliberately narrow: every
// method is read-only and returns model types, so the UI contract stays in one
// place.
type StatsSource interface {
	// Enabled reports whether the administrator configured a DSN at all.
	Enabled() bool
	// Info reports the pipeline state and pings the database.
	Info(ctx context.Context) model.StatsInfo
	// Overview returns the aggregate header of the tab for one window, narrowed
	// to the query's cluster when it is set (the KPI cards follow the filter).
	Overview(ctx context.Context, q model.StatsOverviewQuery) (model.StatsOverview, error)
	// Nodes returns the newest sample of every known node, of one cluster when
	// cluster is not empty.
	Nodes(ctx context.Context, cluster string) ([]model.StatsNodeSample, error)
	// Images returns one page of image presence intervals.
	Images(ctx context.Context, q model.StatsImageQuery) (model.StatsImagePage, error)
	// GCEvents returns one page of image-collector events.
	GCEvents(ctx context.Context, q model.StatsEventQuery) (model.StatsGCPage, error)
	// Repulls returns one page of re-pulls after a collector removal.
	Repulls(ctx context.Context, q model.StatsEventQuery) (model.StatsRepullPage, error)
	// Tasks returns one page of the pull-task log.
	Tasks(ctx context.Context, q model.StatsTaskQuery) (model.StatsTaskPage, error)
}

// WithStats injects the statistics read port. Without it (or with a disabled
// source) the tab renders the "administrator disabled statistics" state instead
// of failing.
func WithStats(src StatsSource) Option {
	return func(s *Server) { s.stats = src }
}

const (
	// statsQueryTimeout bounds one statistics query; the tab must never hang on a
	// slow or wedged database.
	statsQueryTimeout = 5 * time.Second
	// statsStatusTimeout bounds the ping behind GET /api/v1/stats/status.
	statsStatusTimeout = 3 * time.Second

	statsDefaultWindow = 24 * time.Hour
	statsMinWindow     = 5 * time.Minute
	statsMaxWindow     = 90 * 24 * time.Hour

	statsDefaultLimit = 50
	statsMaxLimit     = 200
	statsMaxSearchLen = 200
)

var errBadWindow = errors.New("invalid window: use a Go duration like 24h, 7d or 30d")

// handleStatsStatus reports whether statistics are enabled and whether the
// database answers. It is the only stats endpoint that is useful when the
// pipeline is disabled: the UI renders the "admin disabled it" state from here.
func (s *Server) handleStatsStatus(w http.ResponseWriter, r *http.Request, _ string) {
	info := model.StatsInfo{Enabled: false, Clusters: []string{}, CheckedAt: time.Now()}
	if s.stats != nil {
		ctx, cancel := context.WithTimeout(r.Context(), statsStatusTimeout)
		defer cancel()
		info = s.stats.Info(ctx)
	}
	if info.Clusters == nil {
		info.Clusters = []string{}
	}
	writeJSON(w, http.StatusOK, info)
}

// handleStatsOverview returns the aggregate header of the tab. The optional
// cluster parameter narrows every aggregate to one cluster, so selecting a
// cluster in the UI moves the KPI cards together with the lists below them.
func (s *Server) handleStatsOverview(w http.ResponseWriter, r *http.Request, _ string) {
	ctx, cancel, ok := s.statsContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	query := r.URL.Query()
	window, err := parseStatsWindow(query.Get("window"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out, err := s.stats.Overview(ctx, model.StatsOverviewQuery{
		Window: window,
		// Same trim/bound as the search box: a cluster id is a short opaque key.
		Cluster: statsSearch(query.Get("cluster")),
	})
	if err != nil {
		writeStatsError(w, err)
		return
	}
	if out.Clusters == nil {
		out.Clusters = []string{}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleStatsNodes returns the newest inventory sample of every node, or only
// one cluster when the cluster parameter is set.
func (s *Server) handleStatsNodes(w http.ResponseWriter, r *http.Request, _ string) {
	ctx, cancel, ok := s.statsContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	rows, err := s.stats.Nodes(ctx, statsSearch(r.URL.Query().Get("cluster")))
	if err != nil {
		writeStatsError(w, err)
		return
	}
	if rows == nil {
		rows = []model.StatsNodeSample{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// handleStatsImages returns one page of image presence intervals.
func (s *Server) handleStatsImages(w http.ResponseWriter, r *http.Request, _ string) {
	ctx, cancel, ok := s.statsContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	query := r.URL.Query()
	window, err := parseStatsWindow(query.Get("window"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	state, err := parseStatsImageState(query.Get("state"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	limit, offset, err := parseStatsPage(query)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	page, err := s.stats.Images(ctx, model.StatsImageQuery{
		Window:  window,
		Cluster: strings.TrimSpace(query.Get("cluster")),
		State:   state,
		Search:  statsSearch(query.Get("q")),
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		writeStatsError(w, err)
		return
	}
	if page.Rows == nil {
		page.Rows = []model.StatsImage{}
	}
	if page.Limit <= 0 {
		page.Limit = limit
	}
	page.Offset = offset
	writeJSON(w, http.StatusOK, page)
}

// handleStatsGCEvents returns one page of image-collector events.
func (s *Server) handleStatsGCEvents(w http.ResponseWriter, r *http.Request, _ string) {
	ctx, cancel, ok := s.statsContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	query := r.URL.Query()
	window, err := parseStatsWindow(query.Get("window"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	limit, offset, err := parseStatsPage(query)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	page, err := s.stats.GCEvents(ctx, model.StatsEventQuery{
		Window:  window,
		Cluster: strings.TrimSpace(query.Get("cluster")),
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		writeStatsError(w, err)
		return
	}
	if page.Rows == nil {
		page.Rows = []model.StatsGCEvent{}
	}
	if page.Limit <= 0 {
		page.Limit = limit
	}
	page.Offset = offset
	writeJSON(w, http.StatusOK, page)
}

// handleStatsRepulls returns one page of re-pulls after a collector removal.
func (s *Server) handleStatsRepulls(w http.ResponseWriter, r *http.Request, _ string) {
	ctx, cancel, ok := s.statsContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	query := r.URL.Query()
	window, err := parseStatsWindow(query.Get("window"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	limit, offset, err := parseStatsPage(query)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	page, err := s.stats.Repulls(ctx, model.StatsEventQuery{
		Window:  window,
		Cluster: strings.TrimSpace(query.Get("cluster")),
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		writeStatsError(w, err)
		return
	}
	if page.Rows == nil {
		page.Rows = []model.StatsRepull{}
	}
	if page.Limit <= 0 {
		page.Limit = limit
	}
	page.Offset = offset
	writeJSON(w, http.StatusOK, page)
}

// handleStatsTasks returns one page of the pull-task log kept in PostgreSQL.
func (s *Server) handleStatsTasks(w http.ResponseWriter, r *http.Request, _ string) {
	ctx, cancel, ok := s.statsContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	query := r.URL.Query()
	window, err := parseStatsWindow(query.Get("window"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	state, err := parseStatsTaskState(query.Get("state"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	trigger, err := parseStatsTrigger(query.Get("trigger"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	limit, offset, err := parseStatsPage(query)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	page, err := s.stats.Tasks(ctx, model.StatsTaskQuery{
		Window:  window,
		Cluster: strings.TrimSpace(query.Get("cluster")),
		State:   state,
		Trigger: trigger,
		Search:  statsSearch(query.Get("q")),
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		writeStatsError(w, err)
		return
	}
	if page.Rows == nil {
		page.Rows = []model.StatsTask{}
	}
	if page.Limit <= 0 {
		page.Limit = limit
	}
	page.Offset = offset
	writeJSON(w, http.StatusOK, page)
}

// statsContext resolves the read port and the request deadline. A disabled
// pipeline is an honest state, not an error: the UI shows "the administrator
// disabled statistics collection" and stops asking for data.
func (s *Server) statsContext(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc, bool) {
	if s.stats == nil || !s.stats.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "сбор статистики выключен: администратор не настроил DSN",
		})
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), statsQueryTimeout)
	return ctx, cancel, true
}

// writeStatsError maps a database failure to the tab's "database unavailable"
// state. The raw error may carry host names, so it is not echoed to the UI.
func writeStatsError(w http.ResponseWriter, _ error) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "база статистики недоступна",
	})
}

// parseStatsWindow parses the ?window= parameter. Empty means the default 24h;
// "7d"/"30d"/"90d" are accepted as day durations; otherwise a Go duration. The
// result is clamped to [5m, 90d] so one request cannot scan a decade.
func parseStatsWindow(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return statsDefaultWindow, nil
	}
	var window time.Duration
	if strings.HasSuffix(raw, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil || days <= 0 {
			return 0, errBadWindow
		}
		window = time.Duration(days) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return 0, errBadWindow
		}
		window = parsed
	}
	if window < statsMinWindow {
		window = statsMinWindow
	}
	if window > statsMaxWindow {
		window = statsMaxWindow
	}
	return window, nil
}

// parseStatsPage parses limit/offset for the paged lists.
func parseStatsPage(query map[string][]string) (limit, offset int, err error) {
	get := func(key string) string {
		if values := query[key]; len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
		return ""
	}
	limit = statsDefaultLimit
	if raw := get("limit"); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed <= 0 {
			return 0, 0, errors.New("invalid limit: expected a positive integer")
		}
		if parsed > statsMaxLimit {
			parsed = statsMaxLimit
		}
		limit = parsed
	}
	if raw := get("offset"); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed < 0 {
			return 0, 0, errors.New("invalid offset: expected a non-negative integer")
		}
		offset = parsed
	}
	return limit, offset, nil
}

// parseStatsImageState validates the presence filter.
func parseStatsImageState(raw string) (string, error) {
	switch state := strings.ToLower(strings.TrimSpace(raw)); state {
	case "", "open", "removed":
		return state, nil
	default:
		return "", errors.New("invalid state: expected open, removed or empty")
	}
}

// parseStatsTaskState validates the pull-task filter. "active" means queued or
// running, mirroring the provider's TaskState.Active().
func parseStatsTaskState(raw string) (string, error) {
	switch state := strings.ToLower(strings.TrimSpace(raw)); state {
	case "", "queued", "running", "active", "succeeded", "failed", "canceled":
		return state, nil
	default:
		return "", errors.New("invalid state: expected queued, running, active, succeeded, failed, canceled or empty")
	}
}

func parseStatsTrigger(raw string) (string, error) {
	switch trigger := strings.ToLower(strings.TrimSpace(raw)); trigger {
	case "", "manual", "auto", "reconcile":
		return trigger, nil
	default:
		return "", errors.New("invalid trigger: expected manual, auto, reconcile or empty")
	}
}

// statsSearch caps the free-text filter so a pasted document cannot reach SQL.
func statsSearch(raw string) string {
	search := strings.TrimSpace(raw)
	if len(search) > statsMaxSearchLen {
		search = search[:statsMaxSearchLen]
	}
	return search
}
