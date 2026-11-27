package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/metrics"
	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
	"github.com/local/image-prepuller/internal/tasks"
)

func TestServerAuthMultiClusterAndMetrics(t *testing.T) {
	mgr, err := auth.NewManager(auth.Config{
		Username:      "admin",
		Password:      "pass123",
		SessionSecret: []byte("unit-test-secret"),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	reg := provider.NewRegistry()
	reg.Register(provider.ClusterEntry{ID: "talos-hub", Name: "talos-hub", Role: "local", Provider: provider.NewMockProvider("talos-hub")})
	reg.Register(provider.ClusterEntry{ID: "talos-kz", Name: "talos-kz", Role: "remote", Provider: provider.NewMockProvider("talos-kz")})

	srv, err := NewServer(reg, mgr)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Unauthenticated snapshot must return 401
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// Login
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "pass123"})
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body)))
	if loginRec.Code != http.StatusOK {
		t.Fatalf("expected 200 login, got %d", loginRec.Code)
	}
	cookie := loginRec.Result().Cookies()[0]

	// List clusters
	clReq := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
	clReq.AddCookie(cookie)
	clRec := httptest.NewRecorder()
	srv.ServeHTTP(clRec, clReq)
	if clRec.Code != http.StatusOK || !strings.Contains(clRec.Body.String(), "talos-kz") {
		t.Fatalf("expected clusters list with talos-kz, got %d: %s", clRec.Code, clRec.Body.String())
	}

	// Authenticated snapshot for remote cluster
	snapReq := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?cluster=talos-kz", nil)
	snapReq.AddCookie(cookie)
	snapRec := httptest.NewRecorder()
	srv.ServeHTTP(snapRec, snapReq)
	if snapRec.Code != http.StatusOK || !strings.Contains(snapRec.Body.String(), "talos-worker-01") {
		t.Fatalf("expected 200 snapshot for talos-kz, got %d", snapRec.Code)
	}

	// Spoke headless API auth check
	spoke := NewSpokeServer(provider.NewMockProvider("talos-kz"), "spoke-secret")
	unauthSpoke := httptest.NewRecorder()
	spoke.ServeHTTP(unauthSpoke, httptest.NewRequest(http.MethodGet, "/internal/v1/snapshot", nil))
	if unauthSpoke.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for spoke without token, got %d", unauthSpoke.Code)
	}
	authSpokeReq := httptest.NewRequest(http.MethodGet, "/internal/v1/snapshot", nil)
	authSpokeReq.Header.Set("Authorization", "Bearer spoke-secret")
	authSpokeRec := httptest.NewRecorder()
	spoke.ServeHTTP(authSpokeRec, authSpokeReq)
	if authSpokeRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for spoke with valid token, got %d", authSpokeRec.Code)
	}

	// Prometheus metrics with cluster label
	mRec := httptest.NewRecorder()
	metrics.Handler(reg).ServeHTTP(mRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if mRec.Code != http.StatusOK {
		t.Fatalf("expected 200 metrics, got %d", mRec.Code)
	}
	mBody := mRec.Body.String()
	for _, want := range []string{
		`prepuller_cluster_up{cluster="talos-hub"`,
		`prepuller_cluster_up{cluster="talos-kz"`,
		`prepuller_image_present{cluster="talos-kz"`,
	} {
		if !strings.Contains(mBody, want) {
			t.Fatalf("expected %q in /metrics output", want)
		}
	}
}
func TestSpokeContractV2Endpoints(t *testing.T) {
	p := provider.NewMockProvider("talos-kz")
	spoke := NewSpokeServer(p, "spoke-secret")

	authed := func(method, path string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer spoke-secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		spoke.ServeHTTP(rec, req)
		return rec
	}

	// Counters endpoint feeds prepuller_pull_total for remote clusters.
	rec := authed(http.MethodGet, "/internal/v1/counters", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "api-gateway") {
		t.Fatalf("counters = %d: %s", rec.Code, rec.Body.String())
	}

	// Pull returns the created task instead of the v1 {"ok":true}.
	rec = authed(http.MethodPost, "/internal/v1/pull", `{"apiVersion":"v2","image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01","actor":"admin","trigger":"manual"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("pull = %d: %s", rec.Code, rec.Body.String())
	}
	var task struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.ID == "" || task.State != "running" {
		t.Fatalf("pull returned %+v, want running task with id", task)
	}

	// Cancel the task.
	rec = authed(http.MethodPost, "/internal/v1/pulls/"+task.ID+"/cancel", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}

	// Unknown apiVersion is rejected during the rollout window.
	rec = authed(http.MethodPost, "/internal/v1/pull", `{"apiVersion":"v9","image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported apiVersion = %d, want 400", rec.Code)
	}

	// Previous contract version is still accepted.
	rec = authed(http.MethodPost, "/internal/v1/reconcile", `{"apiVersion":"v1","actor":"admin"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("v1 reconcile = %d: %s", rec.Code, rec.Body.String())
	}
}

// errProvider fails TriggerPull with a fixed error so the HTTP layer mapping can
// be exercised without a live Kubernetes cluster. The embedded Provider is nil:
// only TriggerPull is called by these tests.
type errProvider struct {
	provider.Provider
	err error
}

func (p errProvider) TriggerPull(context.Context, model.PullRequest) (model.PullTask, error) {
	return model.PullTask{}, p.err
}

func TestPullErrorStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusBadRequest},
		{fmt.Errorf("pull: %w", provider.ErrTaskNotFound), http.StatusNotFound},
		{fmt.Errorf("pull: %w", provider.ErrCancelNotSupported), http.StatusConflict},
		{fmt.Errorf("pull: %w", provider.ErrAgentDiskPaused), http.StatusConflict},
		{fmt.Errorf("pull: %w", provider.ErrAgentUnauthorized), http.StatusBadGateway},
		{fmt.Errorf("pull: %w", provider.ErrAgentUnavailable), http.StatusBadGateway},
		{fmt.Errorf("pull: %w", provider.ErrAgentPullStalled), http.StatusGatewayTimeout},
		{fmt.Errorf("pull: %w", provider.ErrAgentRegistryAuth), http.StatusBadGateway},
		{fmt.Errorf("pull: %w", provider.ErrSpokeUnauthorized), http.StatusBadGateway},
		{fmt.Errorf("pull: %w", provider.ErrSpokeUnavailable), http.StatusBadGateway},
		{provider.ErrNotImplemented, http.StatusNotImplemented},
		{errors.New("image and node are required"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		if got := pullErrorStatus(tc.err); got != tc.want {
			t.Errorf("pullErrorStatus(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

// TestSpokePullErrorStatuses checks the wire statuses the hub/UI see for a node
// whose ImageFS is above the pause threshold (409) and for an unreachable agent
// (502) instead of a blanket 400.
func TestSpokePullErrorStatuses(t *testing.T) {
	body := `{"apiVersion":"v2","image":"ghcr.io/acme/api-gateway:1.14.2","node":"talos-worker-01"}`
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("agent: %w", provider.ErrAgentDiskPaused), http.StatusConflict},
		{fmt.Errorf("agent: %w", provider.ErrAgentUnavailable), http.StatusBadGateway},
		{fmt.Errorf("agent: %w", provider.ErrAgentUnauthorized), http.StatusBadGateway},
		{fmt.Errorf("agent: %w", provider.ErrAgentPullStalled), http.StatusGatewayTimeout},
	}
	for _, tc := range cases {
		mux := NewSpokeServer(errProvider{err: tc.err}, "")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/pull", strings.NewReader(body)))
		if rec.Code != tc.want {
			t.Errorf("pull with %v = %d (%s), want %d", tc.err, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// zeroSnapshotProvider is a cluster that has nothing to report (e.g. no workload
// carries the scope label). The HTTP layer must still serialise arrays, because
// the UI maps over workloads/nodes and used to crash on null.
type zeroSnapshotProvider struct{ provider.Provider }

func (zeroSnapshotProvider) Snapshot(context.Context) model.Snapshot { return model.Snapshot{} }

func TestSnapshotHandlerEmitsArrays(t *testing.T) {
	mgr, err := auth.NewManager(auth.Config{
		Username:      "admin",
		Password:      "pass123",
		SessionSecret: []byte("unit-test-secret"),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	reg := provider.NewRegistry()
	reg.Register(provider.ClusterEntry{ID: "empty", Name: "empty", Role: "local", Provider: zeroSnapshotProvider{}})
	srv, err := NewServer(reg, mgr)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	loginBody, _ := json.Marshal(map[string]string{"username": "admin", "password": "pass123"})
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(loginBody)))
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login = %d", loginRec.Code)
	}
	cookie := loginRec.Result().Cookies()[0]

	checkArrays := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode snapshot: %v (%s)", err, rec.Body.String())
		}
		for _, field := range []string{"workloads", "nodes", "activity"} {
			raw, ok := body[field]
			if !ok {
				t.Fatalf("%s is missing: %s", field, rec.Body.String())
			}
			if string(raw) == "null" {
				t.Fatalf("%s is null, want an array: %s", field, rec.Body.String())
			}
			if !strings.HasPrefix(string(raw), "[") {
				t.Fatalf("%s = %s, want an array", field, raw)
			}
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?cluster=empty", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot = %d", rec.Code)
	}
	checkArrays(t, rec)

	spokeRec := httptest.NewRecorder()
	NewSpokeServer(zeroSnapshotProvider{}, "").ServeHTTP(spokeRec, httptest.NewRequest(http.MethodGet, "/internal/v1/snapshot", nil))
	if spokeRec.Code != http.StatusOK {
		t.Fatalf("spoke snapshot = %d", spokeRec.Code)
	}
	checkArrays(t, spokeRec)
}

// pullTasksTestProvider is a provider whose Snapshot returns a fixed task list.
// The snapshot fallback and the snapshot->history feed of /api/v1/pull-tasks are
// tested with it, so MockProvider demo data cannot change the expected totals.
type pullTasksTestProvider struct {
	provider.Provider
	tasks []model.PullTask
}

func (p pullTasksTestProvider) Snapshot(context.Context) model.Snapshot {
	snap := model.Snapshot{Tasks: p.tasks}
	snap.Normalize()
	return snap
}

// taskHistoryServer builds a hub with one cluster and the given task history.
func taskHistoryServer(t *testing.T, cluster string, p provider.Provider, history *tasks.Store) (*Server, *http.Cookie) {
	t.Helper()
	mgr, err := auth.NewManager(auth.Config{
		Username:      "admin",
		Password:      "pass123",
		SessionSecret: []byte("unit-test-secret"),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	reg := provider.NewRegistry()
	reg.Register(provider.ClusterEntry{ID: cluster, Name: cluster, Role: "local", Provider: p})
	opts := []Option{}
	if history != nil {
		opts = append(opts, WithTaskHistory(history))
	}
	srv, err := NewServer(reg, mgr, opts...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cookie, _, code := login(t, srv, "admin", "pass123")
	if code != http.StatusOK || cookie == nil {
		t.Fatalf("login = %d", code)
	}
	return srv, cookie
}

type pullTasksResponse struct {
	Cluster string           `json:"cluster"`
	Tasks   []model.PullTask `json:"tasks"`
	Total   int              `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
	Active  int              `json:"active"`
	Source  string           `json:"source"`
}

func decodePullTasks(t *testing.T, rec *httptest.ResponseRecorder) pullTasksResponse {
	t.Helper()
	var body pullTasksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode pull-tasks: %v (%s)", err, rec.Body.String())
	}
	return body
}

func TestPullTasksHistoryPaginationAndStateFilter(t *testing.T) {
	store, err := tasks.Open("", 100)
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	seed := []model.PullTask{
		{ID: "t-0", Image: "ghcr.io/acme/a:1", Node: "n1", State: model.TaskSucceeded, Trigger: model.TriggerManual, CreatedAt: base},
		{ID: "t-1", Image: "ghcr.io/acme/a:1", Node: "n1", State: model.TaskQueued, Trigger: model.TriggerManual, CreatedAt: base.Add(time.Minute)},
		{ID: "t-2", Image: "ghcr.io/acme/a:1", Node: "n1", State: model.TaskRunning, Trigger: model.TriggerManual, CreatedAt: base.Add(2 * time.Minute)},
		{ID: "t-3", Image: "ghcr.io/acme/a:1", Node: "n1", State: model.TaskFailed, Trigger: model.TriggerManual, CreatedAt: base.Add(3 * time.Minute)},
		{ID: "t-4", Image: "ghcr.io/acme/a:1", Node: "n1", State: model.TaskCanceled, Trigger: model.TriggerManual, CreatedAt: base.Add(4 * time.Minute)},
	}
	store.Observe("talos-kz", seed)

	srv, cookie := taskHistoryServer(t, "talos-kz", pullTasksTestProvider{}, store)

	rec := doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=talos-kz&limit=2&offset=1", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("pull-tasks = %d: %s", rec.Code, rec.Body.String())
	}
	body := decodePullTasks(t, rec)
	if body.Source != "history" || body.Cluster != "talos-kz" {
		t.Fatalf("response = %+v, want source=history cluster=talos-kz", body)
	}
	if body.Total != 5 || body.Active != 2 || body.Limit != 2 || body.Offset != 1 {
		t.Fatalf("total/active/limit/offset = %d/%d/%d/%d, want 5/2/2/1", body.Total, body.Active, body.Limit, body.Offset)
	}
	if len(body.Tasks) != 2 || body.Tasks[0].ID != "t-3" || body.Tasks[1].ID != "t-2" {
		t.Fatalf("page = %+v, want newest-first t-3,t-2", body.Tasks)
	}

	// Default limit is 50, an out-of-range limit is clamped to 1..200 and a
	// negative offset means the first page.
	body = decodePullTasks(t, doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=talos-kz", "", cookie))
	if body.Limit != 50 || body.Total != 5 {
		t.Fatalf("default limit = %d (total %d), want 50 and 5", body.Limit, body.Total)
	}
	if body = decodePullTasks(t, doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=talos-kz&limit=999", "", cookie)); body.Limit != 200 {
		t.Fatalf("clamped limit = %d, want 200", body.Limit)
	}
	if body = decodePullTasks(t, doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=talos-kz&limit=0", "", cookie)); body.Limit != 1 || len(body.Tasks) != 1 {
		t.Fatalf("zero limit = %+v, want limit 1 with one task", body)
	}
	if body = decodePullTasks(t, doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=talos-kz&offset=-5", "", cookie)); body.Offset != 0 || len(body.Tasks) != 5 {
		t.Fatalf("negative offset = %+v, want the first page", body)
	}

	// State filter: active maps to queued+running, everything else is an exact
	// TaskState match, and the active counter ignores the filter.
	body = decodePullTasks(t, doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=talos-kz&state=active", "", cookie))
	if body.Total != 2 || body.Active != 2 || len(body.Tasks) != 2 || body.Tasks[0].ID != "t-2" {
		t.Fatalf("state=active = %+v, want 2 active tasks newest first", body)
	}
	body = decodePullTasks(t, doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=talos-kz&state=failed", "", cookie))
	if body.Total != 1 || body.Active != 2 || len(body.Tasks) != 1 || body.Tasks[0].ID != "t-3" {
		t.Fatalf("state=failed = %+v, want only t-3 with active still 2", body)
	}

	// Bad input is a 400, unknown cluster stays a 404.
	for _, path := range []string{
		"/api/v1/pull-tasks?cluster=talos-kz&limit=abc",
		"/api/v1/pull-tasks?cluster=talos-kz&offset=abc",
		"/api/v1/pull-tasks?cluster=talos-kz&state=bogus",
	} {
		if rec := doAuthed(srv, http.MethodGet, path, "", cookie); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=nope", "", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("unknown cluster = %d, want 404", rec.Code)
	}
}

func TestPullTasksFallsBackToSnapshotWithoutHistory(t *testing.T) {
	base := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	p := pullTasksTestProvider{tasks: []model.PullTask{
		{ID: "snap-2", State: model.TaskRunning, CreatedAt: base.Add(2 * time.Minute)},
		{ID: "snap-1", State: model.TaskSucceeded, CreatedAt: base.Add(time.Minute)},
		{ID: "snap-0", State: model.TaskFailed, CreatedAt: base},
	}}
	srv, cookie := taskHistoryServer(t, "talos-kz", p, nil)

	body := decodePullTasks(t, doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=talos-kz&limit=2&offset=1", "", cookie))
	if body.Source != "snapshot" {
		t.Fatalf("source = %q, want snapshot", body.Source)
	}
	if body.Total != 3 || body.Active != 1 || body.Limit != 2 || body.Offset != 1 {
		t.Fatalf("total/active/limit/offset = %d/%d/%d/%d, want 3/1/2/1", body.Total, body.Active, body.Limit, body.Offset)
	}
	if len(body.Tasks) != 2 || body.Tasks[0].ID != "snap-1" || body.Tasks[1].ID != "snap-0" {
		t.Fatalf("page = %+v, want snap-1,snap-0", body.Tasks)
	}
	body = decodePullTasks(t, doAuthed(srv, http.MethodGet, "/api/v1/pull-tasks?cluster=talos-kz&state=active", "", cookie))
	if body.Source != "snapshot" || body.Total != 1 || len(body.Tasks) != 1 || body.Tasks[0].ID != "snap-2" {
		t.Fatalf("state=active fallback = %+v", body)
	}
}

func TestSnapshotFeedsTaskHistory(t *testing.T) {
	store, err := tasks.Open("", 100)
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	p := pullTasksTestProvider{tasks: []model.PullTask{
		{ID: "feed-1", State: model.TaskSucceeded, CreatedAt: base},
	}}
	srv, cookie := taskHistoryServer(t, "talos-kz", p, store)

	if rec := doAuthed(srv, http.MethodGet, "/api/v1/snapshot?cluster=talos-kz", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("snapshot = %d: %s", rec.Code, rec.Body.String())
	}
	got, total, _ := store.Query("talos-kz", "", 0, 10)
	if total != 1 || len(got) != 1 || got[0].ID != "feed-1" {
		t.Fatalf("history after snapshot = %+v (total %d), want feed-1", got, total)
	}
}

// TestWorkloadPullHandlerTriggersEveryNode keeps the POST /api/v1/workloads/pull
// contract honest: the response is {"triggered": n} with n covering every node
// in scope, and Pull All no longer hides offline nodes (MockProvider marks their
// tasks failed, so they are visible in the task list right away).
func TestWorkloadPullHandlerTriggersEveryNode(t *testing.T) {
	mock := provider.NewMockProvider("demo")
	srv, cookie := taskHistoryServer(t, "demo", mock, nil)

	rec := doAuthed(srv, http.MethodPost, "/api/v1/workloads/pull",
		`{"cluster":"demo","namespace":"prod","kind":"Deployment","name":"api-gateway"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("workload pull = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Triggered int `json:"triggered"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode workload pull response: %v (%s)", err, rec.Body.String())
	}
	// prod/api-gateway has 2 images and the mock has 4 nodes, one of them offline.
	if response.Triggered != 8 {
		t.Fatalf("triggered = %d, want 8 (2 images x 4 nodes)", response.Triggered)
	}

	failed := decodePullTasks(t, doAuthed(srv, http.MethodGet,
		"/api/v1/pull-tasks?cluster=demo&state=failed", "", cookie))
	if failed.Total != 2 {
		t.Fatalf("failed tasks = %d, want 2 for the offline node: %+v", failed.Total, failed.Tasks)
	}
	for _, task := range failed.Tasks {
		if task.Node != "talos-worker-04" || task.State != model.TaskFailed {
			t.Fatalf("failed task = %+v, want talos-worker-04 failed", task)
		}
		if !strings.Contains(task.Error, "offline") {
			t.Fatalf("failed task error = %q, want offline/agent-unreachable cause", task.Error)
		}
	}

	// An unknown workload is still a 400 with the regular error envelope.
	rec = doAuthed(srv, http.MethodPost, "/api/v1/workloads/pull",
		`{"cluster":"demo","namespace":"prod","kind":"Deployment","name":"missing"}`, cookie)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("unknown workload = %d: %s, want 400 with error", rec.Code, rec.Body.String())
	}
}
