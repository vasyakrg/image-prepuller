package provider

import (
	"context"
	"strings"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// AggregateCounters builds the SpokeCounters envelope (GET /internal/v1/counters
// on the direct contract, the Counters field of a state push on the outbound
// one). It is the single implementation behind both paths.
func AggregateCounters(ctx context.Context, p Provider) SpokeCounters {
	out := SpokeCounters{Samples: []model.PullMetricSample{}}
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
	out.ImagesCached = countCachedImages(snap)
	out.LastSyncAt = snap.LastSyncAt
	if out.LastSyncAt.IsZero() {
		out.LastSyncAt = snap.UpdatedAt
	}
	if out.LastSyncAt.IsZero() {
		out.LastSyncAt = time.Now()
	}
	return out
}

// countCachedImages counts images present on at least one node.
func countCachedImages(snap model.Snapshot) int {
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
