package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/local/image-prepuller/internal/model"
)

const agentTestGB = int64(1024 * 1024 * 1024)

// --- fake node agent (HTTP/JSON v1) ----------------------------------------

// fakeAgent mimics the WS-A agent wire contract: snapshot, pull, auth and error
// statuses. It is safe for concurrent use.
type fakeAgent struct {
	mu sync.Mutex

	token        string
	images       []agentImage
	imageFS      agentImageFS
	agentVersion string
	generatedAt  time.Time
	nodeName     string

	// snapshot overrides for failure cases
	snapshotStatus int
	snapshotBody   string
	apiVersion     string

	snapshotCalls int

	pullStatus int
	pullError  string
	pullCalls  int
	pullImages []string
	pullStart  chan struct{} // signalled once when the first pull starts
	pullWait   chan struct{} // handler blocks until this channel is closed
	pullHook   func(image string)
}

func startFakeAgent(t *testing.T, agent *fakeAgent) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(agent.handler())
	t.Cleanup(server.Close)
	return server
}

func (f *fakeAgent) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.token != "" {
			if r.Header.Get("Authorization") != "Bearer "+f.token {
				writeAgentTestJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == agentSnapshotPath:
			f.serveSnapshot(w)
		case r.Method == http.MethodPost && r.URL.Path == agentPullsPath:
			f.servePull(w, r)
		default:
			writeAgentTestJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		}
	})
}

func (f *fakeAgent) serveSnapshot(w http.ResponseWriter) {
	f.mu.Lock()
	f.snapshotCalls++
	status := f.snapshotStatus
	raw := f.snapshotBody
	apiVersion := f.apiVersion
	nodeName := f.nodeName
	if nodeName == "" {
		nodeName = "worker-01"
	}
	generatedAt := f.generatedAt
	if generatedAt.IsZero() {
		generatedAt = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	}
	snapshot := agentSnapshot{
		APIVersion:   apiVersion,
		NodeName:     nodeName,
		AgentVersion: f.agentVersion,
		GeneratedAt:  generatedAt,
		ImageFS:      f.imageFS,
		Images:       append([]agentImage(nil), f.images...),
	}
	f.mu.Unlock()

	if raw != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusOrOK(status))
		_, _ = w.Write([]byte(raw))
		return
	}
	if status != 0 && status != http.StatusOK {
		writeAgentTestJSON(w, status, map[string]string{"error": "snapshot failed"})
		return
	}
	writeAgentTestJSON(w, http.StatusOK, snapshot)
}

func (f *fakeAgent) servePull(w http.ResponseWriter, r *http.Request) {
	var req agentPullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAgentTestJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}

	f.mu.Lock()
	f.pullCalls++
	f.pullImages = append(f.pullImages, req.Image)
	status := f.pullStatus
	errMessage := f.pullError
	wait := f.pullWait
	hook := f.pullHook
	started := f.pullStart
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	f.mu.Unlock()

	if wait != nil {
		select {
		case <-wait:
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 && status != http.StatusOK {
		writeAgentTestJSON(w, status, map[string]string{"error": errMessage})
		return
	}
	if hook != nil {
		hook(req.Image)
	}
	writeAgentTestJSON(w, http.StatusOK, agentPullResult{
		APIVersion: agentAPIVersion,
		Image:      req.Image,
		ImageRef:   req.Image + "@sha256:deadbeef",
		PulledAt:   time.Now().UTC(),
	})
}

// stats and pulledImages are the mutex-guarded accessors used by tests; the
// handler goroutine writes those fields concurrently.
func (f *fakeAgent) stats() (snapshotCalls, pullCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotCalls, f.pullCalls
}

func (f *fakeAgent) pulledImages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pullImages...)
}

func statusOrOK(status int) int {
	if status == 0 {
		return http.StatusOK
	}
	return status
}

func writeAgentTestJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// --- helpers ---------------------------------------------------------------

func testNodeWithIP(name, ip string, ready bool) *corev1.Node {
	node := testNode(name, ready)
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}
	return node
}

func agentPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse httptest URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse httptest port: %v", err)
	}
	return port
}

// newAgentProvider builds a live provider over a fake clientset with the agent
// transport pointing at the httptest server (127.0.0.1:<port>).
func newAgentProvider(t *testing.T, server *httptest.Server, objects []runtime.Object, opts ...K8sOption) *K8sProvider {
	t.Helper()
	allOpts := []K8sOption{
		WithAgentPort(agentPort(t, server)),
		WithAgentScheme("http"),
	}
	allOpts = append(allOpts, opts...)
	return NewK8sProviderWithClientset("test-cluster", kubefake.NewSimpleClientset(objects...), allOpts...)
}

func findTask(t *testing.T, tasks []model.PullTask, taskID string) model.PullTask {
	t.Helper()
	for _, task := range tasks {
		if task.ID == taskID {
			return task
		}
	}
	t.Fatalf("task %q not found in %+v", taskID, tasks)
	return model.PullTask{}
}

// --- inventory mapping -----------------------------------------------------

func TestAgentInventoryPopulatesSnapshot(t *testing.T) {
	agent := &fakeAgent{
		agentVersion: "agent-test-1",
		generatedAt:  time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC),
		imageFS:      agentImageFS{UsedBytes: 10 * agentTestGB, CapacityBytes: 100 * agentTestGB},
		images: []agentImage{
			{
				ID:        "sha256:aaa",
				Name:      "ghcr.io/acme/app:1.0.0",
				Tags:      []string{"ghcr.io/acme/app:1.0.0"},
				Digests:   []string{"sha256:aaa"},
				SizeBytes: 1000,
			},
			{
				ID:        "sha256:bbb",
				Name:      "docker.io/library/busybox:1.36",
				Tags:      []string{"busybox:1.36", "docker.io/library/busybox:1.36"},
				SizeBytes: 500,
			},
		},
	}
	server := startFakeAgent(t, agent)

	objects := []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
		testDeployment("prod", "app", 1,
			[]corev1.Container{testContainer("app", "ghcr.io/acme/app:1.0.0", corev1.PullIfNotPresent)},
			[]corev1.Container{testContainer("init", "busybox:1.36", corev1.PullIfNotPresent)}),
	}
	provider := newAgentProvider(t, server, objects)

	snapshot := provider.Snapshot(context.Background())
	if len(snapshot.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(snapshot.Nodes))
	}
	node := snapshot.Nodes[0]
	if !node.AgentUp || !node.Ready {
		t.Fatalf("node = %+v, want ready and agent up", node)
	}
	if node.AgentVersion != "agent-test-1" {
		t.Fatalf("agentVersion = %q", node.AgentVersion)
	}
	if node.ImageFSUsedBytes != 10*agentTestGB || node.ImageFSCapacityBytes != 100*agentTestGB {
		t.Fatalf("imageFS = %d/%d", node.ImageFSUsedBytes, node.ImageFSCapacityBytes)
	}
	if !node.LastSeen.Equal(agent.generatedAt) {
		t.Fatalf("lastSeen = %v, want generatedAt %v", node.LastSeen, agent.generatedAt)
	}
	// app tag + digest, busybox short + full reference.
	if node.CachedImageCount != 4 {
		t.Fatalf("cachedImageCount = %d, want 4 refs: %+v", node.CachedImageCount, node.CachedImages)
	}
	if node.DiskPaused {
		t.Fatalf("10%% ImageFS must not pause the node")
	}

	workload := findWorkload(t, snapshot, "prod", "Deployment", "app")
	appSlot := findSlot(t, findImage(t, workload, "app"), "worker-01")
	if appSlot.State != model.StatePresent || appSlot.SizeBytes != 1000 {
		t.Fatalf("app slot = %+v, want present 1000", appSlot)
	}
	initSlot := findSlot(t, findImage(t, workload, "init"), "worker-01")
	if initSlot.State != model.StatePresent || initSlot.SizeBytes != 500 {
		t.Fatalf("busybox slot = %+v, want present 500", initSlot)
	}
	if snapshot.Summary.PresentSlots != 2 || snapshot.Summary.TotalSlots != 2 {
		t.Fatalf("summary slots = %+v", snapshot.Summary)
	}
	if snapshot.Summary.CoveragePercent != 100 {
		t.Fatalf("coverage = %v, want 100", snapshot.Summary.CoveragePercent)
	}
	if snapshot.SyncStatus != model.SyncOK || snapshot.Partial {
		t.Fatalf("syncStatus/partial = %q/%v, want ok/false", snapshot.SyncStatus, snapshot.Partial)
	}

	// Snapshot TTL: a second scrape reuses the cached agent inventory.
	provider.Snapshot(context.Background())
	snapshotCalls, _ := agent.stats()
	if snapshotCalls != 1 {
		t.Fatalf("agent snapshot calls = %d, want 1 (TTL cache)", snapshotCalls)
	}
	if err := provider.SyncNow(context.Background(), "operator"); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	provider.Snapshot(context.Background())
	if snapshotCalls, _ := agent.stats(); snapshotCalls != 2 {
		t.Fatalf("agent snapshot calls after SyncNow = %d, want 2", snapshotCalls)
	}
}

func TestCachedImagesFromAgent(t *testing.T) {
	images := cachedImagesFromAgent([]agentImage{
		{ID: "id-1", Name: "app:1", Tags: []string{"app:1", "app:latest"}, Digests: []string{"sha256:1"}, SizeBytes: 10},
		{ID: "id-2", SizeBytes: 20},
		{ID: "", Name: "", Tags: []string{}, Digests: []string{"sha256:2"}, SizeBytes: 30},
	})
	want := []model.CachedImage{
		{Image: "app:1", SizeBytes: 10},
		{Image: "app:latest", SizeBytes: 10},
		{Image: "sha256:1", SizeBytes: 10},
		{Image: "id-2", SizeBytes: 20},
		{Image: "sha256:2", SizeBytes: 30},
	}
	if len(images) != len(want) {
		t.Fatalf("images = %+v, want %+v", images, want)
	}
	for i := range want {
		if images[i] != want[i] {
			t.Fatalf("image[%d] = %+v, want %+v", i, images[i], want[i])
		}
	}
}

// --- inventory failures ----------------------------------------------------

func TestAgentInventoryAuth(t *testing.T) {
	agent := &fakeAgent{token: "s3cret", agentVersion: "agent-test", images: []agentImage{{Name: "nginx:1.27", SizeBytes: 1}}}
	server := startFakeAgent(t, agent)
	objects := []runtime.Object{testNodeWithIP("worker-01", "127.0.0.1", true)}

	authorized := newAgentProvider(t, server, objects, WithAgentToken("s3cret"))
	inventory, err := authorized.agent.Inventory(context.Background(), "worker-01")
	if err != nil {
		t.Fatalf("authorized Inventory: %v", err)
	}
	if len(inventory.Images) != 1 || inventory.AgentVersion != "agent-test" {
		t.Fatalf("inventory = %+v", inventory)
	}

	wrongToken := newAgentProvider(t, server, objects, WithAgentToken("nope"))
	if _, err := wrongToken.agent.Inventory(context.Background(), "worker-01"); !errors.Is(err, ErrAgentUnauthorized) {
		t.Fatalf("wrong token error = %v, want ErrAgentUnauthorized", err)
	} else if !errors.Is(err, ErrNodeInventoryUnavailable) {
		t.Fatalf("inventory error must also wrap ErrNodeInventoryUnavailable: %v", err)
	}

	noToken := newAgentProvider(t, server, objects)
	if _, err := noToken.agent.Inventory(context.Background(), "worker-01"); !errors.Is(err, ErrAgentUnauthorized) {
		t.Fatalf("missing token error = %v, want ErrAgentUnauthorized", err)
	}

	// A snapshot with an unauthorized node degrades to offline slots, no crash.
	snapshot := noToken.Snapshot(context.Background())
	if snapshot.Nodes[0].AgentUp {
		t.Fatalf("node with unauthorized agent must be agent-down")
	}
	if !snapshot.Partial || snapshot.SyncStatus != model.SyncDegraded {
		t.Fatalf("syncStatus/partial = %q/%v", snapshot.SyncStatus, snapshot.Partial)
	}
}

func TestAgentInventoryErrors(t *testing.T) {
	nodeReady := func() *corev1.Node { return testNodeWithIP("worker-01", "127.0.0.1", true) }

	tests := []struct {
		name       string
		agent      *fakeAgent
		objects    []runtime.Object
		node       string
		wantErr    error
		wantSubstr string
	}{
		{
			name:       "server error",
			agent:      &fakeAgent{snapshotStatus: http.StatusInternalServerError},
			objects:    []runtime.Object{nodeReady()},
			node:       "worker-01",
			wantErr:    ErrAgentUnavailable,
			wantSubstr: "snapshot failed",
		},
		{
			name:       "not found",
			agent:      &fakeAgent{snapshotStatus: http.StatusNotFound},
			objects:    []runtime.Object{nodeReady()},
			node:       "worker-01",
			wantErr:    ErrAgentUnavailable,
			wantSubstr: "404",
		},
		{
			name:       "malformed json",
			agent:      &fakeAgent{snapshotBody: "{not json"},
			objects:    []runtime.Object{nodeReady()},
			node:       "worker-01",
			wantErr:    ErrAgentUnavailable,
			wantSubstr: "decode",
		},
		{
			name:       "unsupported api version",
			agent:      &fakeAgent{apiVersion: "v9"},
			objects:    []runtime.Object{nodeReady()},
			node:       "worker-01",
			wantErr:    ErrAgentUnavailable,
			wantSubstr: "apiVersion",
		},
		{
			name:       "agent answers for another node",
			agent:      &fakeAgent{nodeName: "worker-99"},
			objects:    []runtime.Object{nodeReady()},
			node:       "worker-01",
			wantErr:    ErrAgentUnavailable,
			wantSubstr: "answered for node",
		},
		{
			name:       "node without InternalIP",
			agent:      &fakeAgent{},
			objects:    []runtime.Object{testNode("worker-01", true)},
			node:       "worker-01",
			wantErr:    ErrAgentUnavailable,
			wantSubstr: "InternalIP",
		},
		{
			name:       "unknown node",
			agent:      &fakeAgent{},
			objects:    nil,
			node:       "worker-42",
			wantErr:    ErrAgentUnavailable,
			wantSubstr: "resolve node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := startFakeAgent(t, tt.agent)
			provider := newAgentProvider(t, server, tt.objects)
			_, err := provider.agent.Inventory(context.Background(), tt.node)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Inventory error = %v, want %v", err, tt.wantErr)
			}
			if !errors.Is(err, ErrNodeInventoryUnavailable) {
				t.Fatalf("Inventory error must wrap ErrNodeInventoryUnavailable: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("error = %q, want substring %q", err, tt.wantSubstr)
			}
		})
	}
}

func TestAgentInventoryTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)

	provider := newAgentProvider(t, slow, []runtime.Object{testNodeWithIP("worker-01", "127.0.0.1", true)},
		WithAgentTimeout(20*time.Millisecond))

	started := time.Now()
	_, err := provider.agent.Inventory(context.Background(), "worker-01")
	if !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("timeout error = %v, want ErrAgentUnavailable", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("inventory took %v, timeout not applied", elapsed)
	}

	// Snapshot degrades instead of failing.
	snapshot := provider.Snapshot(context.Background())
	if len(snapshot.Nodes) != 1 || snapshot.Nodes[0].AgentUp {
		t.Fatalf("snapshot node = %+v, want agent-down", snapshot.Nodes)
	}
	if !snapshot.Partial {
		t.Fatalf("timed-out agent must make snapshot partial")
	}
}

// --- pulls -----------------------------------------------------------------

func TestAgentTriggerPullSuccess(t *testing.T) {
	agent := &fakeAgent{
		agentVersion: "agent-test",
		imageFS:      agentImageFS{UsedBytes: agentTestGB, CapacityBytes: 100 * agentTestGB},
	}
	// A successful pull adds the image to the node inventory.
	agent.pullHook = func(image string) {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		agent.images = append(agent.images, agentImage{Name: image, Tags: []string{image}, SizeBytes: 2048})
	}
	server := startFakeAgent(t, agent)
	provider := newAgentProvider(t, server, []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
		testDeployment("prod", "app", 1,
			[]corev1.Container{testContainer("app", "ghcr.io/acme/app:1.0.0", corev1.PullIfNotPresent)}, nil),
	})

	task, err := provider.TriggerPull(context.Background(), model.PullRequest{
		Image:   "ghcr.io/acme/app:1.0.0",
		Node:    "worker-01",
		Actor:   "alice",
		Trigger: model.TriggerManual,
	})
	if err != nil {
		t.Fatalf("TriggerPull: %v", err)
	}
	if task.State != model.TaskSucceeded {
		t.Fatalf("task state = %q, want succeeded (%+v)", task.State, task)
	}
	if task.ID == "" || task.Cluster != "test-cluster" || task.Image != "ghcr.io/acme/app:1.0.0" || task.Node != "worker-01" {
		t.Fatalf("task identity = %+v", task)
	}
	if task.Actor != "alice" || task.Trigger != model.TriggerManual {
		t.Fatalf("task actor/trigger = %+v", task)
	}
	if task.StartedAt == nil || task.FinishedAt == nil || task.DurationMs < 0 {
		t.Fatalf("task timestamps = %+v", task)
	}
	if _, pullCalls := agent.stats(); pullCalls != 1 {
		t.Fatalf("agent pull calls = %d, want 1", pullCalls)
	}
	if pulled := agent.pulledImages(); len(pulled) != 1 || pulled[0] != "ghcr.io/acme/app:1.0.0" {
		t.Fatalf("agent pulled images = %v", pulled)
	}

	counters := provider.PullCounters()
	if len(counters) != 1 || counters[0].Image != "ghcr.io/acme/app:1.0.0" ||
		counters[0].Node != "worker-01" || counters[0].Result != "success" || counters[0].Count != 1 {
		t.Fatalf("counters = %+v", counters)
	}

	// The pull invalidated the snapshot cache: the next scrape must show the
	// image as present without waiting for the TTL.
	snapshot := provider.Snapshot(context.Background())
	workload := findWorkload(t, snapshot, "prod", "Deployment", "app")
	slot := findSlot(t, findImage(t, workload, "app"), "worker-01")
	if slot.State != model.StatePresent || slot.SizeBytes != 2048 {
		t.Fatalf("slot after pull = %+v, want present 2048", slot)
	}
}

func TestAgentTriggerPullDiskPaused(t *testing.T) {
	agent := &fakeAgent{
		pullStatus: http.StatusConflict,
		pullError:  "auto-prepull paused: imagefs usage 80.0% >= 78.0% threshold",
	}
	server := startFakeAgent(t, agent)
	provider := newAgentProvider(t, server, []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
		testDeployment("prod", "app", 1,
			[]corev1.Container{testContainer("app", "ghcr.io/acme/app:1.0.0", corev1.PullIfNotPresent)}, nil),
	})

	task, err := provider.TriggerPull(context.Background(), model.PullRequest{
		Image: "ghcr.io/acme/app:1.0.0", Node: "worker-01", Actor: "bob", Trigger: model.TriggerAuto,
	})
	if !errors.Is(err, ErrAgentDiskPaused) {
		t.Fatalf("TriggerPull error = %v, want ErrAgentDiskPaused", err)
	}
	if !strings.Contains(err.Error(), "paused") {
		t.Fatalf("error = %q, want the agent disk-pause reason", err)
	}
	if task.State != model.TaskFailed || !strings.Contains(task.Error, "paused") {
		t.Fatalf("failed task = %+v, want state failed with paused cause", task)
	}
	if _, pullCalls := agent.stats(); pullCalls != 1 {
		t.Fatalf("pull calls = %d, want 1", pullCalls)
	}

	// The image is not on the node and the failed task is visible in the snapshot.
	snapshot := provider.Snapshot(context.Background())
	workload := findWorkload(t, snapshot, "prod", "Deployment", "app")
	if slot := findSlot(t, findImage(t, workload, "app"), "worker-01"); slot.State != model.StateMissing {
		t.Fatalf("slot after 409 = %+v, want missing", slot)
	}
	if snapshot.Summary.FailedTasks != 1 {
		t.Fatalf("failed tasks = %d, want 1", snapshot.Summary.FailedTasks)
	}
	stored := findTask(t, snapshot.Tasks, task.ID)
	if stored.State != model.TaskFailed || !strings.Contains(stored.Error, "paused") {
		t.Fatalf("snapshot task = %+v", stored)
	}

	counters := provider.PullCounters()
	if len(counters) != 1 || counters[0].Result != "error" || counters[0].Count != 1 {
		t.Fatalf("counters = %+v", counters)
	}
}

func TestAgentPullTimeoutIsConfigurable(t *testing.T) {
	// The agent never answers: only the provider-side pull deadline can end it.
	agent := &fakeAgent{pullWait: make(chan struct{})}
	server := startFakeAgent(t, agent)
	provider := newAgentProvider(t, server, []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
	}, WithAgentPullTimeout(20*time.Millisecond))

	started := time.Now()
	_, err := provider.TriggerPull(context.Background(), model.PullRequest{Image: "acme/app:1.0.0", Node: "worker-01"})
	if !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("TriggerPull error = %v, want ErrAgentUnavailable (deadline exceeded)", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("pull waited %s, want it bounded by the configured pull timeout", elapsed)
	}
}

func TestAgentTriggerPullFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		errorBody  string
		wantErr    error
		wantSubstr string
	}{
		{name: "cri failure", status: http.StatusBadGateway, errorBody: "cri pull failed: not found", wantErr: ErrAgentUnavailable, wantSubstr: "cri pull failed"},
		{name: "unauthorized", status: http.StatusUnauthorized, errorBody: "unauthorized", wantErr: ErrAgentUnauthorized, wantSubstr: "401"},
		{name: "server error", status: http.StatusInternalServerError, errorBody: "boom", wantErr: ErrAgentUnavailable, wantSubstr: "500"},
		{name: "stalled pull", status: http.StatusGatewayTimeout, errorBody: "pull stalled: image filesystem stopped growing", wantErr: ErrAgentPullStalled, wantSubstr: "stalled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &fakeAgent{pullStatus: tt.status, pullError: tt.errorBody}
			server := startFakeAgent(t, agent)
			provider := newAgentProvider(t, server, []runtime.Object{
				testNodeWithIP("worker-01", "127.0.0.1", true),
			})
			task, err := provider.TriggerPull(context.Background(), model.PullRequest{
				Image: "acme/app:1.0.0", Node: "worker-01",
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("error = %q, want substring %q", err, tt.wantSubstr)
			}
			if task.State != model.TaskFailed {
				t.Fatalf("task = %+v, want failed", task)
			}
		})
	}
}

func TestAgentTriggerPullUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	provider := newAgentProvider(t, server, []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
	})
	server.Close() // connection refused

	_, err := provider.TriggerPull(context.Background(), model.PullRequest{Image: "acme/app:1.0.0", Node: "worker-01"})
	if !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("error = %v, want ErrAgentUnavailable", err)
	}

	// A node without InternalIP cannot be addressed either.
	noAddress := newAgentProvider(t, server, []runtime.Object{testNode("worker-02", true)})
	if _, err := noAddress.TriggerPull(context.Background(), model.PullRequest{Image: "acme/app:1.0.0", Node: "worker-02"}); !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("no-address error = %v, want ErrAgentUnavailable", err)
	}
}

func TestAgentTriggerPullIdempotentAndCancel(t *testing.T) {
	agent := &fakeAgent{
		pullStart: make(chan struct{}, 1),
		pullWait:  make(chan struct{}),
	}
	server := startFakeAgent(t, agent)
	provider := newAgentProvider(t, server, []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
	})

	request := model.PullRequest{Image: "acme/app:1.0.0", Node: "worker-01", Trigger: model.TriggerManual}
	type result struct {
		task model.PullTask
		err  error
	}
	first := make(chan result, 1)
	go func() {
		task, err := provider.TriggerPull(context.Background(), request)
		first <- result{task: task, err: err}
	}()

	select {
	case <-agent.pullStart:
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not receive the pull request")
	}

	// Idempotency: the same image+node while in flight returns the same task.
	second, err := provider.TriggerPull(context.Background(), request)
	if err != nil {
		t.Fatalf("second TriggerPull: %v", err)
	}
	if second.State != model.TaskRunning {
		t.Fatalf("second task state = %q, want running", second.State)
	}
	if _, pullCalls := agent.stats(); pullCalls != 1 {
		t.Fatalf("pull calls = %d, want 1 (idempotent)", pullCalls)
	}

	// A running synchronous pull cannot be canceled: documented sentinel, not a 500.
	if err := provider.CancelPull(context.Background(), second.ID); !errors.Is(err, ErrCancelNotSupported) {
		t.Fatalf("CancelPull(running) = %v, want ErrCancelNotSupported", err)
	}

	close(agent.pullWait)
	select {
	case got := <-first:
		if got.err != nil {
			t.Fatalf("first TriggerPull: %v", got.err)
		}
		if got.task.State != model.TaskSucceeded {
			t.Fatalf("first task state = %q, want succeeded", got.task.State)
		}
		if got.task.ID != second.ID {
			t.Fatalf("task ids differ: %q vs %q", got.task.ID, second.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first TriggerPull did not finish")
	}

	// Finished tasks follow MockProvider semantics; unknown ids are ErrTaskNotFound.
	finishedErr := provider.CancelPull(context.Background(), second.ID)
	if finishedErr == nil {
		t.Fatalf("CancelPull(finished) must return an error")
	}
	if errors.Is(finishedErr, ErrTaskNotFound) || errors.Is(finishedErr, ErrCancelNotSupported) {
		t.Fatalf("CancelPull(finished) = %v, want already-finished semantics", finishedErr)
	}
	if !strings.Contains(finishedErr.Error(), "already finished") {
		t.Fatalf("CancelPull(finished) = %v", finishedErr)
	}
	if err := provider.CancelPull(context.Background(), "task-unknown"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("CancelPull(unknown) = %v, want ErrTaskNotFound", err)
	}
}

func TestAgentTriggerWorkloadPull(t *testing.T) {
	agent := &fakeAgent{imageFS: agentImageFS{UsedBytes: agentTestGB, CapacityBytes: 100 * agentTestGB}}
	server := startFakeAgent(t, agent)
	provider := newAgentProvider(t, server, []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
		testNode("worker-02", true), // no InternalIP -> agent unreachable
		testDeployment("prod", "app", 1,
			[]corev1.Container{
				testContainer("app", "ghcr.io/acme/app:1.0.0", corev1.PullIfNotPresent),
				testContainer("sidecar", "ghcr.io/acme/sidecar:2.0.0", corev1.PullIfNotPresent),
			}, nil),
	})

	triggered, err := provider.TriggerWorkloadPull(context.Background(), model.WorkloadPullRequest{
		Namespace: "prod", Kind: "Deployment", Name: "app", Actor: "carol", Trigger: model.TriggerManual,
	})
	if err != nil {
		t.Fatalf("TriggerWorkloadPull: %v", err)
	}
	// 2 images x 1 node with a live agent.
	if triggered != 2 {
		t.Fatalf("triggered = %d, want 2", triggered)
	}
	if _, pullCalls := agent.stats(); pullCalls != 2 {
		t.Fatalf("agent pull calls = %d, want 2", pullCalls)
	}
	if _, err := provider.TriggerWorkloadPull(context.Background(), model.WorkloadPullRequest{
		Namespace: "prod", Kind: "Deployment", Name: "missing",
	}); err == nil {
		t.Fatalf("unknown workload must return an error")
	}
}

// --- construction from environment ----------------------------------------

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: test
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user:
    token: test-token
`

func TestNewK8sProviderFromEnvKubeconfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	cfg, err := restConfigFromKubeconfig(path)
	if err != nil {
		t.Fatalf("restConfigFromKubeconfig: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Fatalf("host = %q", cfg.Host)
	}
	if cfg.BearerToken != "test-token" {
		t.Fatalf("bearer token = %q", cfg.BearerToken)
	}
	if !cfg.TLSClientConfig.Insecure {
		t.Fatalf("insecure-skip-tls-verify was not applied")
	}

	// Force the out-of-cluster path, then verify the constructor wires a clientset
	// without contacting the apiserver.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", path)

	provider, err := NewK8sProviderFromEnv("env-cluster")
	if err != nil {
		t.Fatalf("NewK8sProviderFromEnv: %v", err)
	}
	if provider.Clientset() == nil {
		t.Fatalf("clientset is nil")
	}
	if provider.clusterID != "env-cluster" {
		t.Fatalf("clusterID = %q", provider.clusterID)
	}
	if provider.agent == nil || provider.inventory != provider.agent {
		t.Fatalf("agent transport must be the default inventory source")
	}
}

func TestNewK8sProviderFromEnvErrors(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	if _, err := NewK8sProviderFromEnv("c"); err == nil {
		t.Fatalf("missing kubeconfig must fail")
	}

	dir := t.TempDir()
	bad := filepath.Join(dir, "config")
	if err := os.WriteFile(bad, []byte("kind: Config\ncurrent-context: nope\n"), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", bad)
	if _, err := NewK8sProviderFromEnv("c"); err == nil {
		t.Fatalf("kubeconfig without the current context must fail")
	}
}
