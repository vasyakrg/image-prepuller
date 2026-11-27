package stats

import (
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

var baseTime = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func snapshotWithImages(at time.Time, nodes ...model.NodeInfo) model.Snapshot {
	return model.Snapshot{UpdatedAt: at, Nodes: nodes}
}

func nodeWithImages(name string, agentUp bool, images ...model.CachedImage) model.NodeInfo {
	return model.NodeInfo{Name: name, AgentUp: agentUp, CachedImages: images}
}

func TestStepOpensAndClosesPresence(t *testing.T) {
	c := NewCollector()
	first := c.Step("c1", snapshotWithImages(baseTime, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 100},
		model.CachedImage{Image: "redis:7", SizeBytes: 250},
	)), baseTime)

	if len(first.PresenceSeen) != 2 {
		t.Fatalf("PresenceSeen = %d, want 2", len(first.PresenceSeen))
	}
	if len(first.PresenceRemoved) != 0 || len(first.GCEvents) != 0 {
		t.Fatalf("unexpected removals on first step: %+v %+v", first.PresenceRemoved, first.GCEvents)
	}
	if first.Cluster != "c1" || !first.At.Equal(baseTime) {
		t.Fatalf("batch metadata = %+v", first)
	}
	c.Commit("c1", first)

	second := c.Step("c1", snapshotWithImages(baseTime.Add(time.Minute), nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 100},
	)), baseTime.Add(time.Minute))

	if len(second.PresenceRemoved) != 1 {
		t.Fatalf("PresenceRemoved = %+v, want one", second.PresenceRemoved)
	}
	removed := second.PresenceRemoved[0]
	if removed.Image != "redis:7" || removed.SizeBytes != 250 || !removed.RemovedAt.Equal(baseTime.Add(time.Minute)) {
		t.Fatalf("unexpected removal: %+v", removed)
	}
	if len(second.GCEvents) != 1 {
		t.Fatalf("GCEvents = %+v, want one", second.GCEvents)
	}
	event := second.GCEvents[0]
	if event.Node != "n1" || event.RemovedCount != 1 || event.RemovedBytes != 250 {
		t.Fatalf("unexpected gc event: %+v", event)
	}
	if event.WindowSeconds != 60 {
		t.Fatalf("WindowSeconds = %v, want 60", event.WindowSeconds)
	}
}

func TestStepRetriesDiffWithoutCommit(t *testing.T) {
	c := NewCollector()
	at := baseTime
	c.Commit("c1", c.Step("c1", snapshotWithImages(at, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 1},
		model.CachedImage{Image: "redis:7", SizeBytes: 2},
	)), at))

	next := at.Add(time.Minute)
	first := c.Step("c1", snapshotWithImages(next, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 1},
	)), next)
	// No Commit: the next Step must produce the same removal again.
	second := c.Step("c1", snapshotWithImages(next, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 1},
	)), next)

	if len(first.PresenceRemoved) != 1 || len(second.PresenceRemoved) != 1 {
		t.Fatalf("removals not retried: %d / %d", len(first.PresenceRemoved), len(second.PresenceRemoved))
	}
	if first.PresenceRemoved[0].Image != second.PresenceRemoved[0].Image {
		t.Fatalf("retry changed removed image: %+v vs %+v", first.PresenceRemoved[0], second.PresenceRemoved[0])
	}
	if len(first.GCEvents) != 1 || len(second.GCEvents) != 1 {
		t.Fatalf("gc events not retried: %d / %d", len(first.GCEvents), len(second.GCEvents))
	}
}

func TestStepIgnoresAgentDown(t *testing.T) {
	c := NewCollector()
	at := baseTime
	c.Commit("c1", c.Step("c1", snapshotWithImages(at, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 1},
	)), at))

	next := at.Add(time.Minute)
	batch := c.Step("c1", snapshotWithImages(next, nodeWithImages("n1", false,
		model.CachedImage{Image: "nginx:1", SizeBytes: 1},
	)), next)

	if len(batch.PresenceSeen) != 0 || len(batch.PresenceRemoved) != 0 || len(batch.GCEvents) != 0 {
		t.Fatalf("agent down produced presence diff: %+v", batch)
	}
	if len(batch.NodeSamples) != 1 || batch.NodeSamples[0].AgentUp {
		t.Fatalf("node sample not recorded for agent down: %+v", batch.NodeSamples)
	}
}

func TestStepRepullAfterRemoval(t *testing.T) {
	c := NewCollector()
	at := baseTime
	c.Commit("c1", c.Step("c1", snapshotWithImages(at, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 10},
	)), at))

	removedAt := at.Add(5 * time.Minute)
	c.Commit("c1", c.Step("c1", snapshotWithImages(removedAt, nodeWithImages("n1", true)), removedAt))

	taskStart := removedAt.Add(time.Minute)
	taskFinished := removedAt.Add(3 * time.Minute)
	snap := snapshotWithImages(taskFinished, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 10},
	))
	snap.Tasks = []model.PullTask{{
		ID:         "task-1",
		Image:      "nginx:1",
		Node:       "n1",
		State:      model.TaskSucceeded,
		CreatedAt:  taskStart,
		StartedAt:  &taskStart,
		FinishedAt: &taskFinished,
	}}

	batch := c.Step("c1", snap, taskFinished)
	if len(batch.Repulls) != 1 {
		t.Fatalf("Repulls = %+v, want one", batch.Repulls)
	}
	repull := batch.Repulls[0]
	if repull.TaskID != "task-1" || repull.Image != "nginx:1" || repull.Node != "n1" {
		t.Fatalf("unexpected repull: %+v", repull)
	}
	if !repull.RemovedAt.Equal(removedAt) {
		t.Fatalf("RemovedAt = %v, want %v", repull.RemovedAt, removedAt)
	}
	if repull.LagSeconds != 180 {
		t.Fatalf("LagSeconds = %v, want 180", repull.LagSeconds)
	}
}

func TestStepNoRepullWhenImagePresentAtStart(t *testing.T) {
	c := NewCollector()
	at := baseTime
	c.Commit("c1", c.Step("c1", snapshotWithImages(at, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 10},
	)), at))

	start := at.Add(time.Minute)
	finished := at.Add(2 * time.Minute)
	snap := snapshotWithImages(finished, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 10},
	))
	snap.Tasks = []model.PullTask{{
		ID:         "task-2",
		Image:      "nginx:1",
		Node:       "n1",
		State:      model.TaskSucceeded,
		CreatedAt:  start,
		StartedAt:  &start,
		FinishedAt: &finished,
	}}
	batch := c.Step("c1", snap, finished)
	if len(batch.Repulls) != 0 {
		t.Fatalf("unexpected repull for image present at start: %+v", batch.Repulls)
	}
}

func TestStepGCEventAggregatesPerNode(t *testing.T) {
	c := NewCollector()
	at := baseTime
	c.Commit("c1", c.Step("c1", snapshotWithImages(at, nodeWithImages("n1", true,
		model.CachedImage{Image: "a:1", SizeBytes: 10},
		model.CachedImage{Image: "b:1", SizeBytes: 20},
		model.CachedImage{Image: "c:1", SizeBytes: 30},
	)), at))

	next := at.Add(30 * time.Second)
	batch := c.Step("c1", snapshotWithImages(next, nodeWithImages("n1", true,
		model.CachedImage{Image: "a:1", SizeBytes: 10},
	)), next)
	if len(batch.GCEvents) != 1 {
		t.Fatalf("GCEvents = %+v, want one aggregate event", batch.GCEvents)
	}
	if batch.GCEvents[0].RemovedCount != 2 || batch.GCEvents[0].RemovedBytes != 50 {
		t.Fatalf("unexpected aggregate: %+v", batch.GCEvents[0])
	}
	if batch.GCEvents[0].WindowSeconds != 30 {
		t.Fatalf("WindowSeconds = %v, want 30", batch.GCEvents[0].WindowSeconds)
	}
}

func TestStepTaskUpsertAndNodeSample(t *testing.T) {
	c := NewCollector()
	at := baseTime
	snap := snapshotWithImages(at, nodeWithImages("n1", true,
		model.CachedImage{Image: "nginx:1", SizeBytes: 10},
	))
	snap.Nodes[0].Ready = true
	snap.Nodes[0].ImageFSUsedBytes = 1234
	snap.Nodes[0].ImageFSCapacityBytes = 4096
	snap.Nodes[0].CachedImageCount = 1
	snap.Tasks = []model.PullTask{{ID: "task-3", Image: "nginx:1", Node: "n1", State: model.TaskQueued, CreatedAt: at}}

	batch := c.Step("c1", snap, at)
	if len(batch.Tasks) != 1 {
		t.Fatalf("Tasks = %d, want 1", len(batch.Tasks))
	}
	if batch.Tasks[0].Task.Cluster != "c1" {
		t.Fatalf("task cluster = %q, want c1", batch.Tasks[0].Task.Cluster)
	}
	if !batch.Tasks[0].Observed.Equal(at) {
		t.Fatalf("observed = %v, want %v", batch.Tasks[0].Observed, at)
	}
	if len(batch.NodeSamples) != 1 {
		t.Fatalf("NodeSamples = %d, want 1", len(batch.NodeSamples))
	}
	sample := batch.NodeSamples[0]
	if sample.Node != "n1" || !sample.Ready || sample.ImageFSUsedBytes != 1234 || sample.ImageFSCapacityBytes != 4096 || sample.CachedImageCount != 1 {
		t.Fatalf("unexpected node sample: %+v", sample)
	}
}
