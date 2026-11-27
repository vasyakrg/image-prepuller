package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// Snapshot returns the cached cluster view or re-lists the cluster. Even though
// metrics.Handler takes exactly one Snapshot per cluster per scrape, the UI, the
// reconciler and the API also poll concurrently; the TTL cache keeps a single
// cluster listing shared between them. SyncNow drops the cache so the next call
// is fresh.
func (p *K8sProvider) Snapshot(ctx context.Context) model.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	if p.snapshotTTL > 0 && p.cache.valid && now.Before(p.cache.expiresAt) {
		return p.cache.snapshot
	}

	snap := p.buildSnapshot(ctx, now)
	if p.snapshotTTL > 0 {
		p.cache = snapshotCache{
			snapshot:  snap,
			expiresAt: now.Add(p.snapshotTTL),
			valid:     true,
		}
	}
	return snap
}

// buildSnapshot lists nodes and workloads, pairs every image with every node and
// derives states and summary counters. Setting p.lastSyncAt and mutating the
// snapshot cache is fine because Snapshot holds p.mu.
func (p *K8sProvider) buildSnapshot(ctx context.Context, now time.Time) model.Snapshot {
	snap := model.Snapshot{
		ClusterName: p.clusterID,
		APIVersion:  model.APIVersion,
		MockMode:    false,
		UpdatedAt:   now,
		LastSyncAt:  p.lastSyncAt,
	}

	nodes, nodesErr := p.listNodes(ctx)
	workloads, workloadsErr := p.listWorkloads(ctx, int32(len(nodes)))
	listErr := errors.Join(nodesErr, workloadsErr)

	summary := model.Summary{
		TotalWorkers: len(nodes),
	}
	uniqueImages := make(map[string]struct{})
	nodeInfos := make([]model.NodeInfo, 0, len(nodes))
	anyAgentDown := false
	anyNodeNotReady := false

	for _, node := range nodes {
		info := node.info
		nodeInfos = append(nodeInfos, info)
		if info.Ready {
			summary.ReadyWorkers++
		} else {
			anyNodeNotReady = true
		}
		if !node.inventoryOK {
			anyAgentDown = true
		}
		if info.DiskPaused {
			summary.DiskPausedNodes++
		}
		summary.ActivePulls += info.ActivePulls
	}

	for wi := range workloads {
		workload := &workloads[wi]
		summary.TotalWorkloads++
		switch workload.Kind {
		case kindDeployment:
			summary.Deployments++
		case kindStatefulSet:
			summary.StatefulSets++
		case kindDaemonSet:
			summary.DaemonSets++
		}

		for ii := range workload.Images {
			image := &workload.Images[ii]
			uniqueImages[image.Image] = struct{}{}
			if len(image.PolicyViolations) > 0 {
				summary.PolicyWarnings++
			}
			image.Nodes = make([]model.ImageNodeStatus, 0, len(nodes))
			for _, node := range nodes {
				status := p.slotStatus(node, image.Image, now)
				image.Nodes = append(image.Nodes, status)
				summary.TotalSlots++
				if status.State == model.StatePresent {
					summary.PresentSlots++
				}
			}
		}
	}

	summary.UniqueImages = len(uniqueImages)
	if summary.TotalSlots > 0 {
		summary.CoveragePercent = float64(summary.PresentSlots) * 100.0 / float64(summary.TotalSlots)
	}

	// Tasks created by the agent transport (synchronous pulls). Failed ones
	// carry the agent error, e.g. the 409 disk-paused cause.
	snap.Tasks = p.tasksSnapshot()
	for _, task := range snap.Tasks {
		switch task.State {
		case model.TaskQueued:
			summary.QueuedTasks++
		case model.TaskRunning:
			summary.RunningTasks++
		case model.TaskFailed:
			summary.FailedTasks++
		}
	}
	// The agent does not report its pull queue yet; a running provider task is
	// the best available "active pull" signal for the live cluster.
	summary.ActivePulls += summary.RunningTasks

	snap.Summary = summary
	snap.Workloads = workloads
	snap.Nodes = nodeInfos
	snap.Activity = []model.PullEvent{}

	switch {
	case listErr != nil:
		snap.SyncStatus = model.SyncOffline
		snap.Partial = true
		snap.LastSyncAt = p.lastSyncAt
	case anyAgentDown || anyNodeNotReady:
		snap.SyncStatus = model.SyncDegraded
		snap.Partial = anyAgentDown
	default:
		snap.SyncStatus = model.SyncOK
		p.lastSyncAt = now
		snap.LastSyncAt = now
	}
	snap.Normalize()
	return snap
}

// slotStatus computes the one true state of an (image, node) pair. Order matters:
// offline -> present (the image survives a disk pause) -> paused_disk -> missing.
func (p *K8sProvider) slotStatus(node k8sNode, image string, now time.Time) model.ImageNodeStatus {
	status := model.ImageNodeStatus{Node: node.info.Name, LastChecked: now}

	if !node.info.Ready {
		status.State = model.StateOffline
		status.Error = "node is NotReady"
		return status
	}
	if !node.inventoryOK {
		status.State = model.StateOffline
		status.Error = "node agent unreachable: image inventory unknown"
		return status
	}
	if cached, ok := findCachedImage(node.inventory, image); ok {
		status.State = model.StatePresent
		status.SizeBytes = cached.SizeBytes
		return status
	}
	if node.info.DiskPaused {
		status.State = model.StatePausedDisk
		status.Error = fmt.Sprintf(
			"auto-prepull paused: ImageFS usage %.1f%% >= %.0f%% threshold",
			float64(node.info.ImageFSUsedBytes)*100.0/float64(node.info.ImageFSCapacityBytes),
			p.diskPauseRatio*100,
		)
		return status
	}
	status.State = model.StateMissing
	return status
}
