package provider

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/local/image-prepuller/internal/model"
)

// --- helpers ---------------------------------------------------------------

func testNode(name string, ready bool, extra ...corev1.NodeCondition) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	node.Status.Conditions = append([]corev1.NodeCondition{{
		Type:               corev1.NodeReady,
		Status:             status,
		Reason:             "KubeletReady",
		Message:            "kubelet is posting ready status",
		LastTransitionTime: metav1.NewTime(time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)),
	}}, extra...)
	return node
}

func testPodSpec(containers, initContainers []corev1.Container, secrets ...string) corev1.PodSpec {
	spec := corev1.PodSpec{Containers: containers, InitContainers: initContainers}
	for _, name := range secrets {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
	}
	return spec
}

func testContainer(name, image string, policy corev1.PullPolicy) corev1.Container {
	return corev1.Container{Name: name, Image: image, ImagePullPolicy: policy}
}

func testDeployment(ns, name string, replicas int32, containers, init []corev1.Container, secrets ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: testPodSpec(containers, init, secrets...)},
		},
	}
}

func testStatefulSet(ns, name string, replicas int32, containers []corev1.Container) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: testPodSpec(containers, nil)},
		},
	}
}

func testDaemonSet(ns, name string, containers []corev1.Container) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{Spec: testPodSpec(containers, nil)},
		},
	}
}

func newTestProvider(cs *kubefake.Clientset, inventory map[string]NodeInventory, opts ...K8sOption) *K8sProvider {
	allOpts := []K8sOption{WithNodeInventorySource(NewStaticInventorySource(inventory))}
	allOpts = append(allOpts, opts...)
	return NewK8sProviderWithClientset("test-cluster", cs, allOpts...)
}

func findWorkload(t *testing.T, snap model.Snapshot, ns, kind, name string) model.Workload {
	t.Helper()
	for _, w := range snap.Workloads {
		if w.Namespace == ns && w.Kind == kind && w.Name == name {
			return w
		}
	}
	t.Fatalf("workload %s/%s/%s not found in snapshot", ns, kind, name)
	return model.Workload{}
}

func findImage(t *testing.T, workload model.Workload, containerName string) model.WorkloadImage {
	t.Helper()
	for _, image := range workload.Images {
		if image.ContainerName == containerName {
			return image
		}
	}
	t.Fatalf("image for container %q not found in workload %s/%s", containerName, workload.Namespace, workload.Name)
	return model.WorkloadImage{}
}

func findSlot(t *testing.T, image model.WorkloadImage, node string) model.ImageNodeStatus {
	t.Helper()
	for _, slot := range image.Nodes {
		if slot.Node == node {
			return slot
		}
	}
	t.Fatalf("slot for node %q not found in image %q", node, image.Image)
	return model.ImageNodeStatus{}
}

func policyReasons(violations []model.PolicyViolation) []string {
	reasons := make([]string, 0, len(violations))
	for _, v := range violations {
		reasons = append(reasons, v.Reason)
	}
	return reasons
}

// --- workload listing ------------------------------------------------------

func TestK8sProviderListsWorkloads(t *testing.T) {
	cs := kubefake.NewSimpleClientset(
		testNode("worker-01", true),
		testNode("worker-02", true),
		testDeployment("prod", "api-gateway", 2,
			[]corev1.Container{testContainer("gateway", "ghcr.io/acme/api-gateway:1.14.2", corev1.PullIfNotPresent)},
			[]corev1.Container{testContainer("migrate", "ghcr.io/acme/db-migrator:1.14.2", corev1.PullIfNotPresent)},
			"ghcr-prod-creds"),
		testDeployment("prod", "legacy-cron", 1,
			[]corev1.Container{testContainer("cron-proxy", "nginx:latest", "")}, nil),
		testStatefulSet("prod", "payments-worker", 1,
			[]corev1.Container{testContainer("worker", "registry.internal/fin/payments:2.3.0", corev1.PullIfNotPresent)}),
		testDaemonSet("infra", "log-collector",
			[]corev1.Container{testContainer("fluent-bit", "fluent/fluent-bit:3.1.8", corev1.PullIfNotPresent)}),
	)
	p := newTestProvider(cs, map[string]NodeInventory{})

	snap := p.Snapshot(context.Background())

	if snap.MockMode {
		t.Fatalf("live provider must not report MockMode")
	}
	if snap.APIVersion != model.APIVersion {
		t.Fatalf("apiVersion = %q, want %q", snap.APIVersion, model.APIVersion)
	}
	if snap.ClusterName != "test-cluster" {
		t.Fatalf("clusterName = %q", snap.ClusterName)
	}
	if len(snap.Workloads) != 4 {
		t.Fatalf("workloads = %d, want 4", len(snap.Workloads))
	}
	if snap.Summary.TotalWorkloads != 4 || snap.Summary.Deployments != 2 ||
		snap.Summary.StatefulSets != 1 || snap.Summary.DaemonSets != 1 {
		t.Fatalf("summary counts wrong: %+v", snap.Summary)
	}

	apiGateway := findWorkload(t, snap, "prod", "Deployment", "api-gateway")
	if apiGateway.Replicas != 2 {
		t.Fatalf("api-gateway replicas = %d, want 2", apiGateway.Replicas)
	}
	if len(apiGateway.ImagePullSecrets) != 1 || apiGateway.ImagePullSecrets[0] != "ghcr-prod-creds" {
		t.Fatalf("imagePullSecrets = %v", apiGateway.ImagePullSecrets)
	}
	if len(apiGateway.Images) != 2 {
		t.Fatalf("api-gateway images = %d, want 2 (init + main)", len(apiGateway.Images))
	}
	initImage := findImage(t, apiGateway, "migrate")
	if !initImage.IsInit {
		t.Fatalf("migrate image must be marked IsInit")
	}
	mainImage := findImage(t, apiGateway, "gateway")
	if mainImage.Image != "ghcr.io/acme/api-gateway:1.14.2" || mainImage.Ref.Tag != "1.14.2" {
		t.Fatalf("unexpected image ref: %+v", mainImage)
	}
	if len(initImage.PolicyViolations) != 0 || len(mainImage.PolicyViolations) != 0 {
		t.Fatalf("tagged images with IfNotPresent must not warn: %+v", apiGateway.Images)
	}

	legacy := findWorkload(t, snap, "prod", "Deployment", "legacy-cron")
	legacyImage := findImage(t, legacy, "cron-proxy")
	if legacyImage.PullPolicy != model.PullPolicyAlways {
		t.Fatalf("omitted imagePullPolicy for :latest = %q, want Always", legacyImage.PullPolicy)
	}
	reasons := policyReasons(legacyImage.PolicyViolations)
	if len(reasons) != 2 || reasons[0] != "pull_policy_always" || reasons[1] != "latest_tag" {
		t.Fatalf("policy violations = %v", reasons)
	}
	if snap.Summary.PolicyWarnings != 1 {
		t.Fatalf("policyWarnings = %d, want 1", snap.Summary.PolicyWarnings)
	}

	daemonSet := findWorkload(t, snap, "infra", "DaemonSet", "log-collector")
	if daemonSet.Replicas != 2 {
		t.Fatalf("daemonset replicas = %d, want node count 2", daemonSet.Replicas)
	}
	if snap.Summary.UniqueImages != 5 {
		t.Fatalf("uniqueImages = %d, want 5", snap.Summary.UniqueImages)
	}

	// Workloads are sorted by namespace, kind, name so the UI does not flicker.
	for i := 1; i < len(snap.Workloads); i++ {
		prev, cur := snap.Workloads[i-1], snap.Workloads[i]
		if prev.Namespace > cur.Namespace {
			t.Fatalf("workloads not sorted by namespace: %s > %s", prev.Namespace, cur.Namespace)
		}
	}
}

func TestK8sProviderNamespaceAndLabelSelector(t *testing.T) {
	prodDeployment := testDeployment("prod", "api", 1,
		[]corev1.Container{testContainer("app", "acme/api:1.0.0", corev1.PullIfNotPresent)}, nil)
	prodDeployment.Labels = map[string]string{"app.kubernetes.io/part-of": "prepuller"}
	stagingDeployment := testDeployment("staging", "api", 1,
		[]corev1.Container{testContainer("app", "acme/api:0.9.0", corev1.PullIfNotPresent)}, nil)
	stagingDeployment.Labels = map[string]string{"app.kubernetes.io/part-of": "other"}

	cs := kubefake.NewSimpleClientset(
		testNode("worker-01", true),
		prodDeployment,
		stagingDeployment,
	)
	var selectors []string
	cs.PrependReactor("list", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		selectors = append(selectors, action.(clienttesting.ListAction).GetListRestrictions().Labels.String())
		return false, nil, nil
	})

	p := newTestProvider(cs, map[string]NodeInventory{},
		WithNamespaces("prod"),
		WithLabelSelector("app.kubernetes.io/part-of=prepuller"))

	snap := p.Snapshot(context.Background())
	if len(snap.Workloads) != 1 {
		t.Fatalf("workloads = %d, want 1 (prod only)", len(snap.Workloads))
	}
	if snap.Workloads[0].Namespace != "prod" {
		t.Fatalf("listed namespace %q, want prod", snap.Workloads[0].Namespace)
	}
	found := false
	for _, selector := range selectors {
		if selector == "app.kubernetes.io/part-of=prepuller" {
			found = true
		}
	}
	if !found {
		t.Fatalf("label selector was not passed to the API: %v", selectors)
	}
}

// --- nodes, conditions, inventory ------------------------------------------

func TestK8sProviderNodesAndConditions(t *testing.T) {
	transition := metav1.NewTime(time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC))
	cs := kubefake.NewSimpleClientset(
		testNode("worker-01", true,
			corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, LastTransitionTime: transition},
			corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, LastTransitionTime: transition},
			corev1.NodeCondition{Type: "NetworkUnavailable", Status: corev1.ConditionTrue, LastTransitionTime: transition},
		),
		testNode("worker-02", false),
	)
	const gb = int64(1024 * 1024 * 1024)
	lastSeen := time.Date(2026, 11, 26, 10, 5, 0, 0, time.UTC)
	inventory := map[string]NodeInventory{
		"worker-01": {
			Images:               []model.CachedImage{{Image: "nginx:latest", SizeBytes: 100}},
			ImageFSUsedBytes:     63 * gb,
			ImageFSCapacityBytes: 80 * gb,
			AgentVersion:         "agent-0.2.0",
			ActivePulls:          1,
			LastSeen:             lastSeen,
		},
	}
	p := newTestProvider(cs, inventory)

	snap := p.Snapshot(context.Background())
	if len(snap.Nodes) != 2 || snap.Nodes[0].Name != "worker-01" || snap.Nodes[1].Name != "worker-02" {
		t.Fatalf("nodes not listed/sorted: %+v", snap.Nodes)
	}

	healthy := snap.Nodes[0]
	if !healthy.Ready || !healthy.AgentUp {
		t.Fatalf("worker-01 must be ready with agent up: %+v", healthy)
	}
	if healthy.AgentVersion != "agent-0.2.0" {
		t.Fatalf("agentVersion = %q", healthy.AgentVersion)
	}
	if !healthy.DiskPaused {
		t.Fatalf("worker-01 ImageFS at 78.75%% must be disk-paused")
	}
	if healthy.DiskPauseThreshold != DefaultDiskPauseRatio || healthy.KubeletGCThreshold != DefaultKubeletGCRatio {
		t.Fatalf("thresholds = %v/%v", healthy.DiskPauseThreshold, healthy.KubeletGCThreshold)
	}
	if healthy.CachedImageCount != 1 || len(healthy.CachedImages) != 1 {
		t.Fatalf("cached images = %+v", healthy.CachedImages)
	}
	if healthy.ActivePulls != 1 {
		t.Fatalf("activePulls = %d", healthy.ActivePulls)
	}
	if !healthy.LastSeen.Equal(lastSeen) {
		t.Fatalf("lastSeen = %v, want %v", healthy.LastSeen, lastSeen)
	}
	if len(healthy.Conditions) != 3 {
		t.Fatalf("conditions = %+v, want Ready+MemoryPressure+DiskPressure", healthy.Conditions)
	}
	for _, condition := range healthy.Conditions {
		if condition.Type == "NetworkUnavailable" {
			t.Fatalf("unexpected condition type in API subset: %+v", healthy.Conditions)
		}
	}

	notReady := snap.Nodes[1]
	if notReady.Ready || notReady.AgentUp || notReady.DiskPaused {
		t.Fatalf("worker-02 state wrong: %+v", notReady)
	}
	if len(notReady.Conditions) != 1 || notReady.Conditions[0].Type != "Ready" || notReady.Conditions[0].Status != "False" {
		t.Fatalf("worker-02 conditions = %+v", notReady.Conditions)
	}

	if snap.Summary.ReadyWorkers != 1 || snap.Summary.TotalWorkers != 2 {
		t.Fatalf("worker counts = %+v", snap.Summary)
	}
	if snap.Summary.DiskPausedNodes != 1 || snap.Summary.ActivePulls != 1 {
		t.Fatalf("disk/active pulls = %+v", snap.Summary)
	}
	if snap.SyncStatus != model.SyncDegraded || !snap.Partial {
		t.Fatalf("syncStatus/partial = %q/%v, want degraded/true", snap.SyncStatus, snap.Partial)
	}
}

// --- slots -----------------------------------------------------------------

func TestK8sProviderSlots(t *testing.T) {
	const mb = int64(1024 * 1024)
	cs := kubefake.NewSimpleClientset(
		testNode("worker-01", true),
		testNode("worker-02", true),
		testNode("worker-03", true),
		testNode("worker-04", false),
		testDeployment("prod", "app", 1,
			[]corev1.Container{testContainer("app", "ghcr.io/acme/app:1.0.0", corev1.PullIfNotPresent)},
			[]corev1.Container{testContainer("init", "busybox:1.36", corev1.PullIfNotPresent)}),
	)
	inventory := map[string]NodeInventory{
		"worker-01": {
			// The runtime resolves the short image name to the Docker Hub form.
			Images:               []model.CachedImage{{Image: "docker.io/library/busybox:1.36", SizeBytes: 5 * mb}},
			ImageFSUsedBytes:     10 * mb,
			ImageFSCapacityBytes: 100 * mb,
		},
		"worker-02": {
			ImageFSUsedBytes:     10 * mb,
			ImageFSCapacityBytes: 100 * mb,
		},
		"worker-03": {
			ImageFSUsedBytes:     80 * mb, // 80% >= 78% threshold
			ImageFSCapacityBytes: 100 * mb,
		},
		// worker-04 has no inventory: agent unavailable.
	}
	p := newTestProvider(cs, inventory)

	snap := p.Snapshot(context.Background())
	if len(snap.Workloads) != 1 {
		t.Fatalf("workloads = %d", len(snap.Workloads))
	}
	workload := snap.Workloads[0]
	appImage := findImage(t, workload, "app")
	initImage := findImage(t, workload, "init")

	if got := findSlot(t, appImage, "worker-01"); got.State != model.StateMissing {
		t.Fatalf("worker-01 app state = %q, want missing", got.State)
	}
	present := findSlot(t, initImage, "worker-01")
	if present.State != model.StatePresent || present.SizeBytes != 5*mb {
		t.Fatalf("worker-01 busybox slot = %+v, want present 5MB", present)
	}
	if got := findSlot(t, appImage, "worker-02"); got.State != model.StateMissing {
		t.Fatalf("worker-02 app state = %q, want missing", got.State)
	}
	paused := findSlot(t, appImage, "worker-03")
	if paused.State != model.StatePausedDisk {
		t.Fatalf("worker-03 app state = %q, want paused_disk", paused.State)
	}
	if !strings.Contains(paused.Error, "78%") {
		t.Fatalf("paused slot error = %q", paused.Error)
	}
	offline := findSlot(t, appImage, "worker-04")
	if offline.State != model.StateOffline {
		t.Fatalf("NotReady node state = %q, want offline", offline.State)
	}

	// 2 images x 4 nodes.
	if snap.Summary.TotalSlots != 8 {
		t.Fatalf("totalSlots = %d, want 8", snap.Summary.TotalSlots)
	}
	if snap.Summary.PresentSlots != 1 {
		t.Fatalf("presentSlots = %d, want 1", snap.Summary.PresentSlots)
	}
	wantCoverage := 100.0 / 8.0
	if diff := snap.Summary.CoveragePercent - wantCoverage; diff > 0.01 || diff < -0.01 {
		t.Fatalf("coverage = %v, want %v", snap.Summary.CoveragePercent, wantCoverage)
	}
}

func TestFindCachedImage(t *testing.T) {
	inventory := NodeInventory{Images: []model.CachedImage{
		{Image: "nginx:1.27", SizeBytes: 10},
		{Image: "registry.internal/fin/payments:2.3.0@sha256:abc123", SizeBytes: 20},
		{Image: "docker.io/library/busybox:1.36", SizeBytes: 30},
	}}
	tests := []struct {
		name   string
		image  string
		want   int64
		wantOK bool
	}{
		{name: "exact", image: "nginx:1.27", want: 10, wantOK: true},
		{name: "digest wins", image: "registry.internal/fin/payments@sha256:abc123", want: 20, wantOK: true},
		{name: "normalized repo", image: "busybox:1.36", want: 30, wantOK: true},
		{name: "wrong tag", image: "nginx:1.26"},
		{name: "wrong digest", image: "registry.internal/fin/payments@sha256:def456"},
		{name: "unknown image", image: "ghcr.io/acme/other:1.0.0"},
		{name: "empty", image: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := findCachedImage(inventory, tt.image)
			if ok != tt.wantOK {
				t.Fatalf("findCachedImage(%q) ok = %v, want %v", tt.image, ok, tt.wantOK)
			}
			if ok && got.SizeBytes != tt.want {
				t.Fatalf("findCachedImage(%q) size = %d, want %d", tt.image, got.SizeBytes, tt.want)
			}
		})
	}
}

// --- snapshot cache --------------------------------------------------------

func TestK8sProviderSnapshotCache(t *testing.T) {
	cs := kubefake.NewSimpleClientset(
		testNode("worker-01", true),
		testDeployment("prod", "app", 1, []corev1.Container{testContainer("app", "acme/app:1.0.0", corev1.PullIfNotPresent)}, nil),
	)
	var listCalls atomic.Int64
	cs.PrependReactor("list", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		listCalls.Add(1)
		return false, nil, nil
	})

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	p := newTestProvider(cs, map[string]NodeInventory{
		"worker-01": {ImageFSUsedBytes: 1, ImageFSCapacityBytes: 100},
	}, withClock(func() time.Time { return now }))

	first := p.Snapshot(context.Background())
	callsAfterFirst := listCalls.Load()
	if callsAfterFirst != 4 { // deployments + statefulsets + daemonsets + nodes
		t.Fatalf("list calls after first snapshot = %d, want 4", callsAfterFirst)
	}

	// Second scrape inside the TTL is served from the cache.
	second := p.Snapshot(context.Background())
	if got := listCalls.Load(); got != callsAfterFirst {
		t.Fatalf("cached snapshot issued %d extra list calls", got-callsAfterFirst)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("cached snapshot updatedAt changed: %v != %v", second.UpdatedAt, first.UpdatedAt)
	}

	// Still inside the TTL.
	now = now.Add(time.Second)
	p.Snapshot(context.Background())
	if got := listCalls.Load(); got != callsAfterFirst {
		t.Fatalf("snapshot before TTL expiry re-listed: %d != %d", got, callsAfterFirst)
	}

	// SyncNow invalidates immediately.
	if err := p.SyncNow(context.Background(), "operator"); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	third := p.Snapshot(context.Background())
	if got := listCalls.Load(); got != 2*callsAfterFirst {
		t.Fatalf("list calls after SyncNow = %d, want %d", got, 2*callsAfterFirst)
	}
	if !third.UpdatedAt.Equal(now) {
		t.Fatalf("fresh snapshot updatedAt = %v, want %v", third.UpdatedAt, now)
	}

	// TTL expiry re-lists without SyncNow.
	now = now.Add(DefaultSnapshotTTL)
	p.Snapshot(context.Background())
	if got := listCalls.Load(); got != 3*callsAfterFirst {
		t.Fatalf("list calls after TTL expiry = %d, want %d", got, 3*callsAfterFirst)
	}

	// TTL <= 0 disables caching for tests and one-shot callers.
	noCache := newTestProvider(cs, map[string]NodeInventory{}, WithSnapshotTTL(0))
	before := listCalls.Load()
	noCache.Snapshot(context.Background())
	noCache.Snapshot(context.Background())
	if got := listCalls.Load(); got-before != 2*callsAfterFirst {
		t.Fatalf("uncached provider list calls = %d, want %d", got-before, 2*callsAfterFirst)
	}
}

// --- health ----------------------------------------------------------------

func TestK8sProviderHealth(t *testing.T) {
	cs := kubefake.NewSimpleClientset()
	discovery, ok := cs.Discovery().(*discoveryfake.FakeDiscovery)
	if !ok {
		t.Fatalf("fake clientset discovery type = %T", cs.Discovery())
	}
	discovery.FakedServerVersion = &version.Info{GitVersion: "v1.33.0"}

	p := NewK8sProviderWithClientset("test-cluster", cs)
	health := p.Health(context.Background())
	if !health.Online {
		t.Fatalf("health offline: %+v", health)
	}
	if health.Version != "v1.33.0" {
		t.Fatalf("health version = %q, want v1.33.0", health.Version)
	}
	if health.CheckedAt.IsZero() {
		t.Fatalf("health checkedAt is zero")
	}

	failing := NewK8sProviderWithClientset("test-cluster", cs,
		withServerVersion(func(context.Context) (string, error) {
			return "", errors.New("connection refused")
		}))
	bad := failing.Health(context.Background())
	if bad.Online || !strings.Contains(bad.Error, "connection refused") {
		t.Fatalf("failing health probe = %+v", bad)
	}

	// The probe must respect the timeout even though discovery is not ctx-aware.
	timedOut := NewK8sProviderWithClientset("test-cluster", cs,
		WithHealthTimeout(5*time.Millisecond),
		withServerVersion(func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}))
	timeoutHealth := timedOut.Health(context.Background())
	if timeoutHealth.Online || !strings.Contains(timeoutHealth.Error, "timed out") {
		t.Fatalf("timed out health probe = %+v", timeoutHealth)
	}
}

// --- error handling and unsupported pull operations -----------------------

func TestK8sProviderListErrorsDegrade(t *testing.T) {
	cs := kubefake.NewSimpleClientset(testNode("worker-01", true))
	cs.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	p := newTestProvider(cs, map[string]NodeInventory{})

	snap := p.Snapshot(context.Background())
	if snap.SyncStatus != model.SyncOffline {
		t.Fatalf("syncStatus = %q, want offline", snap.SyncStatus)
	}
	if !snap.Partial {
		t.Fatalf("snapshot must be partial on list errors")
	}
	if len(snap.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1 (node listing still succeeds)", len(snap.Nodes))
	}
	if snap.LastSyncAt.IsZero() == false {
		t.Fatalf("lastSyncAt must stay zero when no sync ever succeeded")
	}
}

func TestNewK8sProviderValidation(t *testing.T) {
	if _, err := NewK8sProvider("test-cluster", nil); err == nil {
		t.Fatalf("nil rest config must be rejected")
	}
	p, err := NewK8sProvider("test-cluster", &rest.Config{Host: "https://127.0.0.1:6443"})
	if err != nil {
		t.Fatalf("NewK8sProvider: %v", err)
	}
	if p.Clientset() == nil {
		t.Fatalf("clientset is nil")
	}
	if p.clusterID != "test-cluster" {
		t.Fatalf("clusterID = %q", p.clusterID)
	}
	if _, err := NewK8sProvider("", &rest.Config{Host: "https://127.0.0.1:6443"}); err != nil {
		t.Fatalf("empty cluster id must fall back to a default: %v", err)
	}
}
