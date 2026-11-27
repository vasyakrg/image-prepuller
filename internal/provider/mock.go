package provider

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

const mockAgentVersion = "mock-0.1.0"

type mockNode struct {
	name           string
	ready          bool
	agentUp        bool
	usedBytes      int64
	capacityBytes  int64
	diskPauseRatio float64
	kubeletGCRatio float64
	activePulls    int
	lastSeen       time.Time
}

type mockWorkloadDef struct {
	namespace        string
	kind             string
	name             string
	replicas         int32
	imagePullSecrets []string
	images           []mockImageDef
}

type mockImageDef struct {
	image         string
	containerName string
	isInit        bool
	pullPolicy    model.PullPolicy
	sizeBytes     int64
}

type mockCell struct {
	state       model.PullState
	sizeBytes   int64
	lastChecked time.Time
	errMsg      string
}

type MockProvider struct {
	mu          sync.RWMutex
	clusterName string
	nodes       []*mockNode
	workloads   []mockWorkloadDef
	cells       map[string]map[string]*mockCell // image -> node -> cell
	activity    []model.PullEvent
	pullCounts  map[string]uint64 // "image|node|result" -> count
	eventSeq    int
	tasks       []*model.PullTask
	taskCell    map[string]string          // task id -> "image|node"
	taskPrev    map[string]model.PullState // task id -> state before pulling
	taskSeq     int
	lastSync    time.Time
}

func NewMockProvider(clusterName string) *MockProvider {
	if clusterName == "" {
		clusterName = "talos-1.14-prod"
	}
	now := time.Now()
	const gb = int64(1024 * 1024 * 1024)
	const mb = int64(1024 * 1024)

	nodes := []*mockNode{
		{
			name:           "talos-worker-01",
			ready:          true,
			agentUp:        true,
			usedBytes:      38 * gb,
			capacityBytes:  80 * gb,
			diskPauseRatio: 0.78,
			kubeletGCRatio: 0.85,
			lastSeen:       now.Add(-4 * time.Second),
		},
		{
			name:           "talos-worker-02",
			ready:          true,
			agentUp:        true,
			usedBytes:      43 * gb,
			capacityBytes:  80 * gb,
			diskPauseRatio: 0.78,
			kubeletGCRatio: 0.85,
			lastSeen:       now.Add(-3 * time.Second),
		},
		{
			name:           "talos-worker-03",
			ready:          true,
			agentUp:        true,
			usedBytes:      64 * gb, // 80% -> paused_disk (> 78%)
			capacityBytes:  80 * gb,
			diskPauseRatio: 0.78,
			kubeletGCRatio: 0.85,
			lastSeen:       now.Add(-5 * time.Second),
		},
		{
			name:           "talos-worker-04",
			ready:          false,
			agentUp:        false,
			usedBytes:      31 * gb,
			capacityBytes:  80 * gb,
			diskPauseRatio: 0.78,
			kubeletGCRatio: 0.85,
			lastSeen:       now.Add(-7 * time.Minute),
		},
	}

	workloads := []mockWorkloadDef{
		{
			namespace:        "prod",
			kind:             "Deployment",
			name:             "api-gateway",
			replicas:         2,
			imagePullSecrets: []string{"ghcr-prod-creds"},
			images: []mockImageDef{
				{image: "ghcr.io/acme/api-gateway:1.14.2", containerName: "gateway", pullPolicy: model.PullPolicyIfNotPresent, sizeBytes: 142 * mb},
				{image: "ghcr.io/acme/db-migrator:1.14.2", containerName: "migrate", isInit: true, pullPolicy: model.PullPolicyIfNotPresent, sizeBytes: 68 * mb},
			},
		},
		{
			namespace:        "prod",
			kind:             "StatefulSet",
			name:             "payments-worker",
			replicas:         1,
			imagePullSecrets: []string{"harbor-internal"},
			images: []mockImageDef{
				{image: "registry.internal/fin/payments:2.3.0", containerName: "worker", pullPolicy: model.PullPolicyIfNotPresent, sizeBytes: 312 * mb},
			},
		},
		{
			namespace:        "prod",
			kind:             "Deployment",
			name:             "legacy-cron",
			replicas:         1,
			imagePullSecrets: []string{},
			images: []mockImageDef{
				{image: "nginx:latest", containerName: "cron-proxy", pullPolicy: model.PullPolicyAlways, sizeBytes: 187 * mb},
			},
		},
		{
			namespace:        "staging",
			kind:             "Deployment",
			name:             "ml-scorer",
			replicas:         1,
			imagePullSecrets: []string{"harbor-ml-creds"},
			images: []mockImageDef{
				{image: "registry.internal/ml/scorer:0.9.1", containerName: "scorer", pullPolicy: model.PullPolicyIfNotPresent, sizeBytes: 1840 * mb},
			},
		},
		{
			namespace:        "infra",
			kind:             "DaemonSet",
			name:             "log-collector",
			replicas:         4,
			imagePullSecrets: []string{},
			images: []mockImageDef{
				{image: "fluent/fluent-bit:3.1.8", containerName: "fluent-bit", pullPolicy: model.PullPolicyIfNotPresent, sizeBytes: 94 * mb},
			},
		},
	}

	m := &MockProvider{
		clusterName: clusterName,
		nodes:       nodes,
		workloads:   workloads,
		cells:       make(map[string]map[string]*mockCell),
		pullCounts:  make(map[string]uint64),
		taskCell:    make(map[string]string),
		taskPrev:    make(map[string]model.PullState),
		lastSync:    now,
	}

	for _, w := range workloads {
		for _, img := range w.images {
			if _, ok := m.cells[img.image]; !ok {
				m.cells[img.image] = make(map[string]*mockCell)
			}
			for _, n := range nodes {
				cell := &mockCell{
					state:       model.StatePresent,
					sizeBytes:   img.sizeBytes,
					lastChecked: now.Add(-20 * time.Second),
				}
				if !n.ready || !n.agentUp {
					cell.state = model.StateOffline
					cell.sizeBytes = 0
					cell.errMsg = "node unreachable / agent down"
				} else if float64(n.usedBytes)/float64(n.capacityBytes) >= n.diskPauseRatio &&
					(img.image == "registry.internal/fin/payments:2.3.0" || img.image == "registry.internal/ml/scorer:0.9.1") {
					cell.state = model.StatePausedDisk
					cell.sizeBytes = 0
					cell.errMsg = "auto-prepull paused: ImageFS usage 80.0% >= 78% threshold"
				} else if img.image == "registry.internal/ml/scorer:0.9.1" && n.name == "talos-worker-02" {
					cell.state = model.StateFailed
					cell.sizeBytes = 0
					cell.errMsg = "rpc error: code = Unknown desc = failed to pull image: 401 Unauthorized (harbor-ml-creds)"
				}
				m.cells[img.image][n.name] = cell
			}
		}
	}

	m.activity = []model.PullEvent{
		{
			ID:         "evt-1",
			Timestamp:  now.Add(-9 * time.Minute),
			Node:       "talos-worker-01",
			Image:      "ghcr.io/acme/api-gateway:1.14.2",
			Trigger:    model.TriggerAuto,
			Result:     "success",
			DurationMs: 2410,
		},
		{
			ID:         "evt-2",
			Timestamp:  now.Add(-8 * time.Minute),
			Node:       "talos-worker-02",
			Image:      "registry.internal/ml/scorer:0.9.1",
			Trigger:    model.TriggerAuto,
			Result:     "error",
			DurationMs: 420,
			Error:      "401 Unauthorized (harbor-ml-creds)",
		},
		{
			ID:         "evt-3",
			Timestamp:  now.Add(-3 * time.Minute),
			Node:       "talos-worker-01",
			Image:      "registry.internal/fin/payments:2.3.0",
			Trigger:    model.TriggerAuto,
			Result:     "success",
			DurationMs: 4120,
		},
	}
	m.eventSeq = 3
	m.pullCounts["ghcr.io/acme/api-gateway:1.14.2|talos-worker-01|success"] = 1
	m.pullCounts["registry.internal/ml/scorer:0.9.1|talos-worker-02|error"] = 2
	m.pullCounts["registry.internal/fin/payments:2.3.0|talos-worker-01|success"] = 1

	return m
}

// EvaluatePolicyViolations reports imagePullPolicy/tag problems for one image.
func EvaluatePolicyViolations(image string, pullPolicy model.PullPolicy) []model.PolicyViolation {
	var out []model.PolicyViolation
	if pullPolicy == model.PullPolicyAlways {
		out = append(out, model.PolicyViolation{
			Reason:  "pull_policy_always",
			Message: "imagePullPolicy is Always (blocks offline failover)",
		})
	}
	if isLatestOrUntagged(image) {
		out = append(out, model.PolicyViolation{
			Reason:  "latest_tag",
			Message: "image uses :latest or has no immutable tag/digest",
		})
	}
	return out
}

func isLatestOrUntagged(image string) bool {
	if strings.Contains(image, "@sha256:") {
		return false
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon <= lastSlash {
		return true
	}
	tag := image[lastColon+1:]
	return tag == "" || strings.EqualFold(tag, "latest")
}

func (m *MockProvider) Snapshot(_ context.Context) model.Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var summary model.Summary
	uniqueImages := make(map[string]struct{})
	nodeImages := make(map[string][]model.CachedImage)

	workloads := make([]model.Workload, 0, len(m.workloads))
	for _, wd := range m.workloads {
		summary.TotalWorkloads++
		switch wd.kind {
		case "Deployment":
			summary.Deployments++
		case "StatefulSet":
			summary.StatefulSets++
		case "DaemonSet":
			summary.DaemonSets++
		}

		w := model.Workload{
			Namespace:        wd.namespace,
			Kind:             wd.kind,
			Name:             wd.name,
			Replicas:         wd.replicas,
			ImagePullSecrets: append([]string(nil), wd.imagePullSecrets...),
			Images:           make([]model.WorkloadImage, 0, len(wd.images)),
		}

		for _, imgDef := range wd.images {
			uniqueImages[imgDef.image] = struct{}{}
			violations := EvaluatePolicyViolations(imgDef.image, imgDef.pullPolicy)
			if len(violations) > 0 {
				summary.PolicyWarnings++
			}

			wi := model.WorkloadImage{
				Image:            imgDef.image,
				Ref:              model.ParseImageRef(imgDef.image),
				ContainerName:    imgDef.containerName,
				IsInit:           imgDef.isInit,
				PullPolicy:       imgDef.pullPolicy,
				PolicyViolations: violations,
				Nodes:            make([]model.ImageNodeStatus, 0, len(m.nodes)),
			}

			for _, n := range m.nodes {
				c := m.cells[imgDef.image][n.name]
				wi.Nodes = append(wi.Nodes, model.ImageNodeStatus{
					Node:        n.name,
					State:       c.state,
					SizeBytes:   c.sizeBytes,
					LastChecked: c.lastChecked,
					Error:       c.errMsg,
				})
				summary.TotalSlots++
				if c.state == model.StatePresent {
					summary.PresentSlots++
					nodeImages[n.name] = append(nodeImages[n.name], model.CachedImage{
						Image:     imgDef.image,
						SizeBytes: c.sizeBytes,
					})
				}
			}
			w.Images = append(w.Images, wi)
		}
		workloads = append(workloads, w)
	}

	nodes := make([]model.NodeInfo, 0, len(m.nodes))
	readyWorkers := 0
	for _, n := range m.nodes {
		summary.TotalWorkers++
		if n.ready {
			summary.ReadyWorkers++
			readyWorkers++
		}
		paused := n.ready && float64(n.usedBytes)/float64(n.capacityBytes) >= n.diskPauseRatio
		if paused {
			summary.DiskPausedNodes++
		}
		summary.ActivePulls += n.activePulls
		cached := nodeImages[n.name]
		nodes = append(nodes, model.NodeInfo{
			Name:                 n.name,
			Ready:                n.ready,
			AgentUp:              n.agentUp,
			AgentVersion:         mockAgentVersion,
			Conditions:           mockConditions(n),
			DiskPaused:           paused,
			DiskPauseThreshold:   n.diskPauseRatio,
			KubeletGCThreshold:   n.kubeletGCRatio,
			ImageFSUsedBytes:     n.usedBytes,
			ImageFSCapacityBytes: n.capacityBytes,
			CachedImageCount:     len(cached),
			ActivePulls:          n.activePulls,
			CachedImages:         cached,
			LastSeen:             n.lastSeen,
		})
	}

	summary.UniqueImages = len(uniqueImages)
	if summary.TotalSlots > 0 {
		summary.CoveragePercent = float64(summary.PresentSlots) * 100.0 / float64(summary.TotalSlots)
	}

	tasks := make([]model.PullTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, *t)
		if t.State == model.TaskQueued {
			summary.QueuedTasks++
		}
		if t.State == model.TaskRunning {
			summary.RunningTasks++
		}
		if t.State == model.TaskFailed {
			summary.FailedTasks++
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })

	activity := make([]model.PullEvent, len(m.activity))
	copy(activity, m.activity)
	sort.Slice(activity, func(i, j int) bool {
		return activity[i].Timestamp.After(activity[j].Timestamp)
	})

	syncStatus := model.SyncOK
	if readyWorkers < len(m.nodes) {
		syncStatus = model.SyncDegraded
	}

	snap := model.Snapshot{
		ClusterName: m.clusterName,
		APIVersion:  model.APIVersion,
		MockMode:    true,
		UpdatedAt:   time.Now(),
		LastSyncAt:  m.lastSync,
		SyncStatus:  syncStatus,
		Partial:     false,
		Summary:     summary,
		Workloads:   workloads,
		Nodes:       nodes,
		Activity:    activity,
		Tasks:       tasks,
	}
	snap.Normalize()
	return snap
}

// mockConditions returns the kubelet condition subset the hub renders. The
// healthy value differs per type: Ready=True is good, while DiskPressure=False is
// the good state - the demo data shows both, so the UI colouring is visible in
// mock mode. worker-03 is the 80% disk node, hence DiskPressure=True there.
func mockConditions(n *mockNode) []model.NodeCondition {
	now := time.Now()
	if !n.ready {
		return []model.NodeCondition{{
			Type:           "Ready",
			Status:         "False",
			Reason:         "KubeletNotReady",
			Message:        "node is NotReady (simulated)",
			LastTransition: now.Add(-7 * time.Minute),
		}}
	}
	diskPressure := n.capacityBytes > 0 && float64(n.usedBytes)/float64(n.capacityBytes) >= n.diskPauseRatio
	disk := model.NodeCondition{
		Type:           "DiskPressure",
		Status:         "False",
		Reason:         "KubeletHasNoDiskPressure",
		Message:        "kubelet has no disk pressure",
		LastTransition: now.Add(-26 * time.Hour),
	}
	conditions := []model.NodeCondition{{
		Type:           "Ready",
		Status:         "True",
		Reason:         "KubeletReady",
		Message:        "kubelet is posting ready status",
		LastTransition: now.Add(-26 * time.Hour),
	}}
	if diskPressure {
		disk.Status = "True"
		disk.Reason = "KubeletHasDiskPressure"
		disk.Message = "kubelet has disk pressure (simulated: above the pause threshold)"
	} else {
		conditions = append(conditions, model.NodeCondition{
			Type:           "MemoryPressure",
			Status:         "False",
			Reason:         "KubeletHasSufficientMemory",
			Message:        "kubelet has sufficient memory available",
			LastTransition: now.Add(-26 * time.Hour),
		})
	}
	return append(conditions, disk)
}

func (m *MockProvider) TriggerPull(_ context.Context, req model.PullRequest) (model.PullTask, error) {
	m.mu.Lock()
	nodeMap, ok := m.cells[req.Image]
	if !ok {
		m.mu.Unlock()
		return model.PullTask{}, fmt.Errorf("unknown image %q", req.Image)
	}
	cell, ok := nodeMap[req.Node]
	if !ok {
		m.mu.Unlock()
		return model.PullTask{}, fmt.Errorf("unknown node %q", req.Node)
	}
	targetNode := m.nodeLocked(req.Node)
	if targetNode == nil {
		m.mu.Unlock()
		return model.PullTask{}, fmt.Errorf("node %q not found", req.Node)
	}
	if !targetNode.ready || !targetNode.agentUp {
		m.mu.Unlock()
		return model.PullTask{}, fmt.Errorf("node %q is offline / agent unreachable", req.Node)
	}
	// Idempotency: an active pull for the same image+node is returned as-is.
	if existing := m.activeTaskLocked(req.Image, req.Node); existing != nil {
		out := *existing
		m.mu.Unlock()
		return out, nil
	}

	imgSize := m.imageSizeLocked(req.Image)
	now := time.Now()
	started := now
	m.taskSeq++
	task := &model.PullTask{
		ID:        fmt.Sprintf("task-%d", m.taskSeq),
		Cluster:   m.clusterName,
		Image:     req.Image,
		Node:      req.Node,
		State:     model.TaskRunning,
		Attempt:   1,
		Trigger:   triggerOrDefault(req.Trigger),
		Actor:     req.Actor,
		CreatedAt: now,
		StartedAt: &started,
	}
	m.tasks = append(m.tasks, task)
	m.taskCell[task.ID] = req.Image + "|" + req.Node
	m.taskPrev[task.ID] = cell.state

	cell.state = model.StatePulling
	cell.errMsg = ""
	targetNode.activePulls++

	m.eventSeq++
	evtID := fmt.Sprintf("evt-%d", m.eventSeq)
	m.activity = append(m.activity, model.PullEvent{
		ID:        evtID,
		Timestamp: now,
		Node:      req.Node,
		Image:     req.Image,
		Trigger:   task.Trigger,
		Actor:     req.Actor,
		Result:    "pulling",
	})
	m.trimLocked()
	out := *task
	m.mu.Unlock()

	// Simulate a realistic asynchronous image pull in the background.
	go func(taskID string) {
		delay := time.Duration(1200+rand.Intn(1300)) * time.Millisecond
		time.Sleep(delay)

		m.mu.Lock()
		defer m.mu.Unlock()

		t := m.findTaskLocked(taskID)
		if t == nil || t.State != model.TaskRunning {
			return
		}
		if targetNode.activePulls > 0 {
			targetNode.activePulls--
		}
		finished := time.Now()
		targetNode.lastSeen = finished
		cell.state = model.StatePresent
		cell.sizeBytes = imgSize
		cell.lastChecked = finished
		cell.errMsg = ""
		m.pullCounts[fmt.Sprintf("%s|%s|success", req.Image, req.Node)]++
		t.State = model.TaskSucceeded
		t.FinishedAt = &finished
		t.DurationMs = delay.Milliseconds()
		m.lastSync = finished

		for i := range m.activity {
			if m.activity[i].ID == evtID {
				m.activity[i].Result = "success"
				m.activity[i].DurationMs = delay.Milliseconds()
				break
			}
		}
	}(task.ID)

	return out, nil
}

func (m *MockProvider) TriggerWorkloadPull(ctx context.Context, req model.WorkloadPullRequest) (int, error) {
	m.mu.RLock()
	var images []string
	var onlineNodes []string
	found := false
	for _, w := range m.workloads {
		if w.namespace == req.Namespace && strings.EqualFold(w.kind, req.Kind) && w.name == req.Name {
			found = true
			for _, img := range w.images {
				images = append(images, img.image)
			}
			break
		}
	}
	for _, n := range m.nodes {
		if n.ready && n.agentUp {
			onlineNodes = append(onlineNodes, n.name)
		}
	}
	m.mu.RUnlock()

	if !found {
		return 0, fmt.Errorf("workload %s/%s/%s not found", req.Namespace, req.Kind, req.Name)
	}

	triggered := 0
	for _, img := range images {
		for _, node := range onlineNodes {
			if _, err := m.TriggerPull(ctx, model.PullRequest{
				Image:   img,
				Node:    node,
				Actor:   req.Actor,
				Trigger: triggerOrDefault(req.Trigger),
			}); err == nil {
				triggered++
			}
		}
	}
	return triggered, nil
}

func (m *MockProvider) CancelPull(_ context.Context, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	t := m.findTaskLocked(taskID)
	if t == nil {
		return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
	}
	if !t.State.Active() {
		return fmt.Errorf("pull task %s is already finished (%s)", taskID, t.State)
	}

	now := time.Now()
	t.State = model.TaskCanceled
	t.FinishedAt = &now
	if cellKey, ok := m.taskCell[taskID]; ok {
		parts := strings.SplitN(cellKey, "|", 2)
		if len(parts) == 2 {
			if n := m.nodeLocked(parts[1]); n != nil && n.activePulls > 0 {
				n.activePulls--
			}
			if cell := m.cellLocked(parts[0], parts[1]); cell != nil {
				cell.state = m.taskPrev[taskID]
				if cell.state == model.StatePresent {
					cell.sizeBytes = m.imageSizeLocked(parts[0])
				}
				cell.lastChecked = now
			}
		}
	}
	m.activity = append(m.activity, model.PullEvent{
		ID:        fmt.Sprintf("evt-%d", m.nextEventLocked()),
		Timestamp: now,
		Node:      t.Node,
		Image:     t.Image,
		Trigger:   t.Trigger,
		Actor:     t.Actor,
		Result:    "canceled",
	})
	m.trimLocked()
	return nil
}

func (m *MockProvider) SyncNow(_ context.Context, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, n := range m.nodes {
		if n.ready && n.agentUp {
			n.lastSeen = now
		}
	}
	for _, nodeMap := range m.cells {
		for _, cell := range nodeMap {
			if cell.state != model.StateOffline {
				cell.lastChecked = now
			}
		}
	}
	m.lastSync = now
	return nil
}

func (m *MockProvider) Health(_ context.Context) model.Health {
	return model.Health{
		Online:    true,
		Version:   mockAgentVersion,
		CheckedAt: time.Now(),
	}
}

func (m *MockProvider) PullCounters() []model.PullMetricSample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]model.PullMetricSample, 0, len(m.pullCounts))
	for k, v := range m.pullCounts {
		parts := strings.Split(k, "|")
		if len(parts) != 3 {
			continue
		}
		out = append(out, model.PullMetricSample{
			Image:  parts[0],
			Node:   parts[1],
			Result: parts[2],
			Count:  v,
		})
	}
	return out
}

func (m *MockProvider) nodeLocked(name string) *mockNode {
	for _, n := range m.nodes {
		if n.name == name {
			return n
		}
	}
	return nil
}

func (m *MockProvider) cellLocked(image, node string) *mockCell {
	nodeMap, ok := m.cells[image]
	if !ok {
		return nil
	}
	return nodeMap[node]
}

func (m *MockProvider) imageSizeLocked(image string) int64 {
	for _, w := range m.workloads {
		for _, img := range w.images {
			if img.image == image {
				return img.sizeBytes
			}
		}
	}
	return 128 * 1024 * 1024
}

func (m *MockProvider) findTaskLocked(taskID string) *model.PullTask {
	for _, t := range m.tasks {
		if t.ID == taskID {
			return t
		}
	}
	return nil
}

func (m *MockProvider) activeTaskLocked(image, node string) *model.PullTask {
	for _, t := range m.tasks {
		if t.Image == image && t.Node == node && t.State.Active() {
			return t
		}
	}
	return nil
}

func (m *MockProvider) nextEventLocked() int {
	m.eventSeq++
	return m.eventSeq
}

func (m *MockProvider) trimLocked() {
	if len(m.activity) > 100 {
		m.activity = m.activity[len(m.activity)-100:]
	}
	if len(m.tasks) > 200 {
		m.tasks = m.tasks[len(m.tasks)-200:]
	}
}

func triggerOrDefault(t model.PullTrigger) model.PullTrigger {
	if t == "" {
		return model.TriggerAuto
	}
	return t
}
