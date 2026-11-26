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
	pullPolicy    string
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
				{image: "ghcr.io/acme/api-gateway:1.14.2", containerName: "gateway", pullPolicy: "IfNotPresent", sizeBytes: 142 * mb},
				{image: "ghcr.io/acme/db-migrator:1.14.2", containerName: "migrate", isInit: true, pullPolicy: "IfNotPresent", sizeBytes: 68 * mb},
			},
		},
		{
			namespace:        "prod",
			kind:             "StatefulSet",
			name:             "payments-worker",
			replicas:         1,
			imagePullSecrets: []string{"harbor-internal"},
			images: []mockImageDef{
				{image: "registry.internal/fin/payments:2.3.0", containerName: "worker", pullPolicy: "IfNotPresent", sizeBytes: 312 * mb},
			},
		},
		{
			namespace:        "prod",
			kind:             "Deployment",
			name:             "legacy-cron",
			replicas:         1,
			imagePullSecrets: []string{},
			images: []mockImageDef{
				{image: "nginx:latest", containerName: "cron-proxy", pullPolicy: "Always", sizeBytes: 187 * mb},
			},
		},
		{
			namespace:        "staging",
			kind:             "Deployment",
			name:             "ml-scorer",
			replicas:         1,
			imagePullSecrets: []string{"harbor-ml-creds"},
			images: []mockImageDef{
				{image: "registry.internal/ml/scorer:0.9.1", containerName: "scorer", pullPolicy: "IfNotPresent", sizeBytes: 1840 * mb},
			},
		},
		{
			namespace:        "infra",
			kind:             "DaemonSet",
			name:             "log-collector",
			replicas:         4,
			imagePullSecrets: []string{},
			images: []mockImageDef{
				{image: "fluent/fluent-bit:3.1.8", containerName: "fluent-bit", pullPolicy: "IfNotPresent", sizeBytes: 94 * mb},
			},
		},
	}

	m := &MockProvider{
		clusterName: clusterName,
		nodes:       nodes,
		workloads:   workloads,
		cells:       make(map[string]map[string]*mockCell),
		pullCounts:  make(map[string]uint64),
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
			Trigger:    "auto",
			Result:     "success",
			DurationMs: 2410,
		},
		{
			ID:         "evt-2",
			Timestamp:  now.Add(-8 * time.Minute),
			Node:       "talos-worker-02",
			Image:      "registry.internal/ml/scorer:0.9.1",
			Trigger:    "auto",
			Result:     "error",
			DurationMs: 420,
			Error:      "401 Unauthorized (harbor-ml-creds)",
		},
		{
			ID:         "evt-3",
			Timestamp:  now.Add(-3 * time.Minute),
			Node:       "talos-worker-01",
			Image:      "registry.internal/fin/payments:2.3.0",
			Trigger:    "auto",
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

func EvaluatePolicyViolations(image, pullPolicy string) []model.PolicyViolation {
	var out []model.PolicyViolation
	if strings.EqualFold(pullPolicy, "Always") {
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
	for _, n := range m.nodes {
		summary.TotalWorkers++
		if n.ready {
			summary.ReadyWorkers++
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

	activity := make([]model.PullEvent, len(m.activity))
	copy(activity, m.activity)
	sort.Slice(activity, func(i, j int) bool {
		return activity[i].Timestamp.After(activity[j].Timestamp)
	})

	return model.Snapshot{
		ClusterName: m.clusterName,
		MockMode:    true,
		UpdatedAt:   time.Now(),
		Summary:     summary,
		Workloads:   workloads,
		Nodes:       nodes,
		Activity:    activity,
	}
}

func (m *MockProvider) TriggerPull(_ context.Context, image, nodeName, actor string) error {
	m.mu.Lock()
	nodeMap, ok := m.cells[image]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown image %q", image)
	}
	cell, ok := nodeMap[nodeName]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown node %q", nodeName)
	}
	var targetNode *mockNode
	for _, n := range m.nodes {
		if n.name == nodeName {
			targetNode = n
			break
		}
	}
	if targetNode == nil {
		m.mu.Unlock()
		return fmt.Errorf("node %q not found", nodeName)
	}
	if !targetNode.ready || !targetNode.agentUp {
		m.mu.Unlock()
		return fmt.Errorf("node %q is offline / agent unreachable", nodeName)
	}
	if cell.state == model.StatePulling {
		m.mu.Unlock()
		return nil
	}

	var imgSize int64 = 128 * 1024 * 1024
	for _, w := range m.workloads {
		for _, img := range w.images {
			if img.image == image {
				imgSize = img.sizeBytes
				break
			}
		}
	}

	cell.state = model.StatePulling
	cell.errMsg = ""
	targetNode.activePulls++

	m.eventSeq++
	evtID := fmt.Sprintf("evt-%d", m.eventSeq)
	trigger := "auto"
	if actor != "" {
		trigger = fmt.Sprintf("manual (%s)", actor)
	}
	m.activity = append(m.activity, model.PullEvent{
		ID:        evtID,
		Timestamp: time.Now(),
		Node:      nodeName,
		Image:     image,
		Trigger:   trigger,
		Result:    "pulling",
	})
	if len(m.activity) > 100 {
		m.activity = m.activity[len(m.activity)-100:]
	}
	m.mu.Unlock()

	// Simulate realistic async image pull in background
	go func() {
		delay := time.Duration(1200+rand.Intn(1300)) * time.Millisecond
		time.Sleep(delay)

		m.mu.Lock()
		defer m.mu.Unlock()

		if targetNode.activePulls > 0 {
			targetNode.activePulls--
		}
		targetNode.lastSeen = time.Now()
		cell.state = model.StatePresent
		cell.sizeBytes = imgSize
		cell.lastChecked = time.Now()
		cell.errMsg = ""
		m.pullCounts[fmt.Sprintf("%s|%s|success", image, nodeName)]++

		for i := range m.activity {
			if m.activity[i].ID == evtID {
				m.activity[i].Result = "success"
				m.activity[i].DurationMs = delay.Milliseconds()
				break
			}
		}
	}()

	return nil
}

func (m *MockProvider) TriggerWorkloadPull(ctx context.Context, namespace, kind, name, actor string) (int, error) {
	m.mu.RLock()
	var images []string
	var onlineNodes []string
	found := false
	for _, w := range m.workloads {
		if w.namespace == namespace && strings.EqualFold(w.kind, kind) && w.name == name {
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
		return 0, fmt.Errorf("workload %s/%s/%s not found", namespace, kind, name)
	}

	triggered := 0
	for _, img := range images {
		for _, node := range onlineNodes {
			if err := m.TriggerPull(ctx, img, node, actor); err == nil {
				triggered++
			}
		}
	}
	return triggered, nil
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
	return nil
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
