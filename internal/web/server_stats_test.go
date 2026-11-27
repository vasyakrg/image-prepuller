package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

// fakeStats is a read-only StatsSource that records the query it received and
// returns canned pages, so the HTTP wiring (auth, parsing, envelopes) is tested
// without a database.
type fakeStats struct {
	enabled bool
	info    model.StatsInfo

	overviewQuery *model.StatsOverviewQuery
	overview      model.StatsOverview
	nodesCluster  string
	nodes         []model.StatsNodeSample
	imagesQuery   *model.StatsImageQuery
	imagesPage    model.StatsImagePage
	gcQuery       *model.StatsEventQuery
	gcPage        model.StatsGCPage
	repullsQuery  *model.StatsEventQuery
	repullsPage   model.StatsRepullPage
	tasksQuery    *model.StatsTaskQuery
	tasksPage     model.StatsTaskPage

	err error
}

func (f *fakeStats) Enabled() bool { return f.enabled }

func (f *fakeStats) Info(context.Context) model.StatsInfo { return f.info }

func (f *fakeStats) Overview(_ context.Context, q model.StatsOverviewQuery) (model.StatsOverview, error) {
	f.overviewQuery = &q
	return f.overview, f.err
}

func (f *fakeStats) Nodes(_ context.Context, cluster string) ([]model.StatsNodeSample, error) {
	f.nodesCluster = cluster
	return f.nodes, f.err
}

func (f *fakeStats) Images(_ context.Context, q model.StatsImageQuery) (model.StatsImagePage, error) {
	f.imagesQuery = &q
	return f.imagesPage, f.err
}

func (f *fakeStats) GCEvents(_ context.Context, q model.StatsEventQuery) (model.StatsGCPage, error) {
	f.gcQuery = &q
	return f.gcPage, f.err
}

func (f *fakeStats) Repulls(_ context.Context, q model.StatsEventQuery) (model.StatsRepullPage, error) {
	f.repullsQuery = &q
	return f.repullsPage, f.err
}

func (f *fakeStats) Tasks(_ context.Context, q model.StatsTaskQuery) (model.StatsTaskPage, error) {
	f.tasksQuery = &q
	return f.tasksPage, f.err
}

// newStatsServer builds a hub with one operator and one viewer, optionally with
// a statistics source; the cookie of the operator is returned for convenience.
func newStatsServer(t *testing.T, stats StatsSource) (*Server, *http.Cookie) {
	t.Helper()
	users := []auth.User{
		{Name: "admin", Password: "admin-pass", Role: auth.RoleOperator},
		{Name: "viewer", Password: "viewer-pass", Role: auth.RoleViewer},
	}
	mgr, err := auth.NewManagerFromConfig("stats-test-secret", users)
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	reg := provider.NewRegistry()
	reg.Register(provider.ClusterEntry{ID: "talos-hub", Name: "talos-hub", Role: "local", Provider: provider.NewMockProvider("talos-hub")})
	opts := []Option{}
	if stats != nil {
		opts = append(opts, WithStats(stats))
	}
	srv, err := NewServer(reg, mgr, opts...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cookie, body, code := login(t, srv, "admin", "admin-pass")
	if code != http.StatusOK {
		t.Fatalf("login = %d: %v", code, body)
	}
	return srv, cookie
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

// TestStatsEndpointsRequireAuth keeps the whole tab private: every route is a
// hub API call, so an anonymous request must be a 401 and never reach the port.
func TestStatsEndpointsRequireAuth(t *testing.T) {
	srv, _ := newStatsServer(t, nil)
	for _, path := range []string{
		"/api/v1/stats/status",
		"/api/v1/stats/overview",
		"/api/v1/stats/nodes",
		"/api/v1/stats/images",
		"/api/v1/stats/gc",
		"/api/v1/stats/repulls",
		"/api/v1/stats/tasks",
	} {
		rec := doAuthed(srv, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without cookie = %d, want 401", path, rec.Code)
		}
	}
}

// TestStatsDisabledState is the "administrator disabled statistics" contract:
// the status endpoint answers 200 with enabled=false, and the data endpoints say
// the same instead of a bare 500.
func TestStatsDisabledState(t *testing.T) {
	srv, cookie := newStatsServer(t, nil)

	rec := doAuthed(srv, http.MethodGet, "/api/v1/stats/status", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var info model.StatsInfo
	decodeJSON(t, rec, &info)
	if info.Enabled {
		t.Fatalf("disabled status reports enabled=true: %+v", info)
	}
	if info.Clusters == nil {
		t.Fatalf("disabled status clusters = null, want []")
	}

	for _, path := range []string{
		"/api/v1/stats/overview",
		"/api/v1/stats/nodes",
		"/api/v1/stats/images",
		"/api/v1/stats/gc",
		"/api/v1/stats/repulls",
		"/api/v1/stats/tasks",
	} {
		rec := doAuthed(srv, http.MethodGet, path, "", cookie)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s with disabled stats = %d, want 503", path, rec.Code)
		}
		var payload map[string]string
		decodeJSON(t, rec, &payload)
		if payload["error"] == "" {
			t.Fatalf("%s disabled error is empty: %s", path, rec.Body.String())
		}
	}
}

// TestStatsStatusUnavailable keeps "the DB is down" distinct from "disabled":
// enabled=true, ok=false, error set, HTTP still 200 so the UI can render it.
func TestStatsStatusUnavailable(t *testing.T) {
	fake := &fakeStats{enabled: true, info: model.StatsInfo{
		Enabled:  true,
		OK:       false,
		Error:    "база статистики недоступна",
		Clusters: []string{"talos-hub"},
	}}
	srv, cookie := newStatsServer(t, fake)
	rec := doAuthed(srv, http.MethodGet, "/api/v1/stats/status", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var info model.StatsInfo
	decodeJSON(t, rec, &info)
	if !info.Enabled || info.OK || info.Error == "" {
		t.Fatalf("unavailable status = %+v", info)
	}
}

// TestStatsOverviewWindow covers the default window and the day-suffix parsing.
func TestStatsOverviewWindow(t *testing.T) {
	fake := &fakeStats{enabled: true, overview: model.StatsOverview{
		WindowSeconds: int64((7 * 24 * time.Hour) / time.Second),
		Clusters:      []string{"talos-hub"},
	}}
	srv, cookie := newStatsServer(t, fake)

	rec := doAuthed(srv, http.MethodGet, "/api/v1/stats/overview", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d: %s", rec.Code, rec.Body.String())
	}
	if fake.overviewQuery == nil || fake.overviewQuery.Window != 24*time.Hour || fake.overviewQuery.Cluster != "" {
		t.Fatalf("default overview query = %+v, want 24h and no cluster", fake.overviewQuery)
	}

	rec = doAuthed(srv, http.MethodGet, "/api/v1/stats/overview?window=7d", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview 7d = %d: %s", rec.Code, rec.Body.String())
	}
	if fake.overviewQuery.Window != 7*24*time.Hour {
		t.Fatalf("7d window = %s, want 168h", fake.overviewQuery.Window)
	}

	// The cluster filter must reach the source: the KPI cards follow it.
	rec = doAuthed(srv, http.MethodGet, "/api/v1/stats/overview?window=7d&cluster=%20talos-kz%20", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview cluster = %d: %s", rec.Code, rec.Body.String())
	}
	if fake.overviewQuery.Cluster != "talos-kz" {
		t.Fatalf("overview cluster = %q, want the trimmed talos-kz", fake.overviewQuery.Cluster)
	}

	rec = doAuthed(srv, http.MethodGet, "/api/v1/stats/overview?window=banana", "", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad window = %d, want 400", rec.Code)
	}
}

// TestStatsImagesQueryWiring checks the filters and pagination reach the source
// and that nil rows become [] in the response.
func TestStatsImagesQueryWiring(t *testing.T) {
	fake := &fakeStats{enabled: true, imagesPage: model.StatsImagePage{Total: 1, Rows: []model.StatsImage{{
		Cluster: "talos-hub", Node: "worker-1", Image: "nginx:1.29",
	}}}}
	srv, cookie := newStatsServer(t, fake)

	rec := doAuthed(srv, http.MethodGet,
		"/api/v1/stats/images?window=30d&cluster=talos-hub&state=removed&q=nginx&limit=500&offset=50", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("images = %d: %s", rec.Code, rec.Body.String())
	}
	if fake.imagesQuery == nil {
		t.Fatal("images query was not passed to the source")
	}
	q := fake.imagesQuery
	if q.Window != 30*24*time.Hour || q.Cluster != "talos-hub" || q.State != "removed" || q.Search != "nginx" {
		t.Fatalf("images query = %+v", q)
	}
	if q.Limit != statsMaxLimit {
		t.Fatalf("limit = %d, want clamp to %d", q.Limit, statsMaxLimit)
	}
	if q.Offset != 50 {
		t.Fatalf("offset = %d, want 50", q.Offset)
	}

	var page model.StatsImagePage
	decodeJSON(t, rec, &page)
	if page.Limit != statsMaxLimit || page.Offset != 50 {
		t.Fatalf("page envelope = %+v", page)
	}

	if rec := doAuthed(srv, http.MethodGet, "/api/v1/stats/images?state=bogus", "", cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad state = %d, want 400", rec.Code)
	}
	if rec := doAuthed(srv, http.MethodGet, "/api/v1/stats/images?limit=0", "", cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit = %d, want 400", rec.Code)
	}

	// An empty reader page must still be an array, not null.
	empty := &fakeStats{enabled: true, imagesPage: model.StatsImagePage{}}
	srv2, cookie2 := newStatsServer(t, empty)
	rec = doAuthed(srv2, http.MethodGet, "/api/v1/stats/images", "", cookie2)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty images = %d: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	decodeJSON(t, rec, &raw)
	if string(raw["rows"]) != "[]" {
		t.Fatalf("empty rows = %s, want []", raw["rows"])
	}
}

// TestStatsTasksQueryWiring covers the state/trigger/search filters.
func TestStatsTasksQueryWiring(t *testing.T) {
	fake := &fakeStats{enabled: true, tasksPage: model.StatsTaskPage{Total: 2, Rows: []model.StatsTask{}}}
	srv, cookie := newStatsServer(t, fake)

	rec := doAuthed(srv, http.MethodGet,
		"/api/v1/stats/tasks?window=24h&cluster=talos-hub&state=active&trigger=manual&q=nginx&limit=100&offset=0", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("tasks = %d: %s", rec.Code, rec.Body.String())
	}
	q := fake.tasksQuery
	if q == nil {
		t.Fatal("tasks query was not passed to the source")
	}
	if q.State != "active" || q.Trigger != "manual" || q.Search != "nginx" || q.Cluster != "talos-hub" {
		t.Fatalf("tasks query = %+v", q)
	}

	if rec := doAuthed(srv, http.MethodGet, "/api/v1/stats/tasks?state=bogus", "", cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad task state = %d, want 400", rec.Code)
	}
	if rec := doAuthed(srv, http.MethodGet, "/api/v1/stats/tasks?trigger=bogus", "", cookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad trigger = %d, want 400", rec.Code)
	}
}

// TestStatsNodesEmptyArray keeps the UI's .map() safe on an empty database.
func TestStatsNodesEmptyArray(t *testing.T) {
	srv, cookie := newStatsServer(t, &fakeStats{enabled: true})
	rec := doAuthed(srv, http.MethodGet, "/api/v1/stats/nodes", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("nodes = %d: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	decodeJSON(t, rec, &raw)
	if string(raw["rows"]) != "[]" {
		t.Fatalf("nodes rows = %s, want []", raw["rows"])
	}
}

// TestStatsNodesClusterWiring checks the nodes endpoint takes the same cluster
// filter as the lists, so the nodes table follows the tab's selection.
func TestStatsNodesClusterWiring(t *testing.T) {
	fake := &fakeStats{enabled: true, nodes: []model.StatsNodeSample{{
		Cluster: "talos-kz", Node: "worker-1", Ready: true, AgentUp: true,
	}}}
	srv, cookie := newStatsServer(t, fake)
	rec := doAuthed(srv, http.MethodGet, "/api/v1/stats/nodes?cluster=talos-kz", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("nodes = %d: %s", rec.Code, rec.Body.String())
	}
	if fake.nodesCluster != "talos-kz" {
		t.Fatalf("nodes cluster = %q, want talos-kz", fake.nodesCluster)
	}
	var payload struct {
		Rows []model.StatsNodeSample `json:"rows"`
	}
	decodeJSON(t, rec, &payload)
	if len(payload.Rows) != 1 || payload.Rows[0].Cluster != "talos-kz" {
		t.Fatalf("rows = %+v", payload.Rows)
	}
}

// TestStatsDatabaseError maps every reader failure to the tab's "database
// unavailable" state, with no raw driver error leaking into the response.
func TestStatsDatabaseError(t *testing.T) {
	fake := &fakeStats{enabled: true, err: errors.New("dial tcp 10.0.0.1:5432: connect: connection refused")}
	srv, cookie := newStatsServer(t, fake)

	for _, path := range []string{
		"/api/v1/stats/overview",
		"/api/v1/stats/nodes",
		"/api/v1/stats/images",
		"/api/v1/stats/gc",
		"/api/v1/stats/repulls",
		"/api/v1/stats/tasks",
	} {
		rec := doAuthed(srv, http.MethodGet, path, "", cookie)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s error = %d, want 503", path, rec.Code)
		}
		if body := rec.Body.String(); len(body) == 0 || strings.Contains(body, "10.0.0.1") || strings.Contains(body, "5432") {
			t.Fatalf("%s leaked the driver error: %s", path, body)
		}
	}
}
