package stats

import (
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// presenceKey identifies one image on one node inside a cluster.
type presenceKey struct {
	node  string
	image string
}

// presenceEntry is the collector's committed view of one image presence interval.
type presenceEntry struct {
	open        bool
	firstSeen   time.Time
	lastSeen    time.Time
	sizeBytes   int64
	lastRemoved time.Time
}

// clusterState is the committed state of one cluster between Step and Commit.
type clusterState struct {
	presence map[presenceKey]*presenceEntry
	lastStep map[string]time.Time
}

// Collector computes presence diffs and commits them only after a successful
// Store.Apply. It is safe for concurrent use.
type Collector struct {
	mu       sync.Mutex
	clusters map[string]*clusterState
}

// NewCollector returns an empty collector.
func NewCollector() *Collector {
	return &Collector{clusters: make(map[string]*clusterState)}
}

func (c *Collector) state(cluster string) *clusterState {
	st := c.clusters[cluster]
	if st == nil {
		st = &clusterState{
			presence: make(map[presenceKey]*presenceEntry),
			lastStep: make(map[string]time.Time),
		}
		c.clusters[cluster] = st
	}
	return st
}

// Step builds the diff for one snapshot without changing the committed state.
// Call Commit only after Store.Apply succeeded; otherwise the next Step produces
// the same diff again.
func (c *Collector) Step(cluster string, snap model.Snapshot, at time.Time) Batch {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.state(cluster)
	b := Batch{Cluster: cluster, At: at}

	for _, task := range snap.Tasks {
		task.Cluster = cluster
		b.Tasks = append(b.Tasks, TaskUpsert{Task: task, Observed: at})
	}

	// overrides is the prospective presence view for this step: it lets the
	// repull check see appearances/removals without mutating committed state.
	overrides := make(map[presenceKey]*presenceEntry)

	for _, node := range snap.Nodes {
		b.NodeSamples = append(b.NodeSamples, NodeSample{
			Node:                     node.Name,
			SampledAt:                at,
			Ready:                    node.Ready,
			AgentUp:                  node.AgentUp,
			ImageFSUsedBytes:         node.ImageFSUsedBytes,
			ImageFSCapacityBytes:     node.ImageFSCapacityBytes,
			ImageFSCapacityEstimated: node.ImageFSCapacityEstimated,
			CachedImageCount:         node.CachedImageCount,
			DiskPaused:               node.DiskPaused,
		})
		if !node.AgentUp {
			// Without an agent the cached-image list is unknown, not empty:
			// never interpret a dead agent as "the collector removed everything".
			continue
		}

		current := make(map[string]int64, len(node.CachedImages))
		for _, img := range node.CachedImages {
			if img.Image == "" {
				continue
			}
			current[img.Image] = img.SizeBytes
		}

		for image, size := range current {
			key := presenceKey{node: node.Name, image: image}
			entry := &presenceEntry{
				open:      true,
				firstSeen: at,
				lastSeen:  at,
				sizeBytes: size,
			}
			if prev := st.presence[key]; prev != nil {
				entry.lastRemoved = prev.lastRemoved
				if prev.open {
					entry.firstSeen = prev.firstSeen
				}
			}
			overrides[key] = entry
			b.PresenceSeen = append(b.PresenceSeen, PresenceSeen{
				Node:        node.Name,
				Image:       image,
				FirstSeenAt: entry.firstSeen,
				LastSeenAt:  at,
				SizeBytes:   size,
			})
		}

		var removedCount int
		var removedBytes int64
		for key, prev := range st.presence {
			if key.node != node.Name || !prev.open {
				continue
			}
			if _, ok := current[key.image]; ok {
				continue
			}
			overrides[key] = &presenceEntry{
				open:        false,
				firstSeen:   prev.firstSeen,
				lastSeen:    prev.lastSeen,
				sizeBytes:   prev.sizeBytes,
				lastRemoved: at,
			}
			b.PresenceRemoved = append(b.PresenceRemoved, PresenceRemoved{
				Node:      node.Name,
				Image:     key.image,
				RemovedAt: at,
				SizeBytes: prev.sizeBytes,
			})
			removedCount++
			removedBytes += prev.sizeBytes
		}

		if removedCount > 0 {
			window := 0.0
			if prev, ok := st.lastStep[node.Name]; ok && !prev.IsZero() {
				if d := at.Sub(prev).Seconds(); d > 0 {
					window = d
				}
			}
			b.GCEvents = append(b.GCEvents, GCEvent{
				Node:          node.Name,
				At:            at,
				RemovedCount:  removedCount,
				RemovedBytes:  removedBytes,
				WindowSeconds: window,
			})
		}
	}

	for _, upsert := range b.Tasks {
		task := upsert.Task
		if task.State != model.TaskSucceeded || task.FinishedAt == nil {
			continue
		}
		start := task.CreatedAt
		if task.StartedAt != nil && !task.StartedAt.IsZero() {
			start = *task.StartedAt
		}
		if start.IsZero() {
			continue
		}
		key := presenceKey{node: task.Node, image: task.Image}
		entry := overrides[key]
		if entry == nil {
			entry = st.presence[key]
		}
		if entry == nil || entry.lastRemoved.IsZero() {
			continue
		}
		// The image must have been absent when the task started: the latest
		// removal happened at or before the task start and the task finished
		// after that removal.
		if entry.lastRemoved.After(start) || entry.lastRemoved.After(*task.FinishedAt) {
			continue
		}
		// A presence interval that opened after the task start means the image
		// was absent at start and came back during the pull.
		if entry.open && !entry.firstSeen.After(start) {
			continue
		}
		lag := task.FinishedAt.Sub(entry.lastRemoved).Seconds()
		if lag < 0 {
			continue
		}
		b.Repulls = append(b.Repulls, Repull{
			TaskID:     task.ID,
			Image:      task.Image,
			Node:       task.Node,
			RemovedAt:  entry.lastRemoved,
			FinishedAt: *task.FinishedAt,
			LagSeconds: lag,
		})
	}

	return b
}

// Commit applies a successfully persisted batch to the collector's committed
// state. It must only be called after Store.Apply returned nil.
func (c *Collector) Commit(cluster string, b Batch) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.state(cluster)
	for _, sample := range b.NodeSamples {
		if !sample.AgentUp || sample.SampledAt.IsZero() {
			continue
		}
		st.lastStep[sample.Node] = sample.SampledAt
	}
	for _, seen := range b.PresenceSeen {
		key := presenceKey{node: seen.Node, image: seen.Image}
		entry := st.presence[key]
		if entry == nil {
			entry = &presenceEntry{}
			st.presence[key] = entry
		}
		if !entry.open {
			entry.firstSeen = seen.FirstSeenAt
		}
		entry.open = true
		entry.lastSeen = seen.LastSeenAt
		entry.sizeBytes = seen.SizeBytes
	}
	for _, removed := range b.PresenceRemoved {
		key := presenceKey{node: removed.Node, image: removed.Image}
		entry := st.presence[key]
		if entry == nil {
			entry = &presenceEntry{}
			st.presence[key] = entry
		}
		entry.open = false
		entry.lastRemoved = removed.RemovedAt
		entry.sizeBytes = removed.SizeBytes
	}
}
