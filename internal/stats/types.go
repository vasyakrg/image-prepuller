// Package stats collects optional PostgreSQL statistics about image lifetimes,
// containerd image-collector (GC) activity, re-pulls and pull tasks on the hub.
//
// The pipeline is best effort: the hub never blocks on it. A failing database
// only means the next tick retries the same diff (see ADR-002).
package stats

import (
	"context"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// Config controls the background collector loop.
type Config struct {
	// Interval is how often every cluster is snapshotted. Zero means DefaultInterval.
	Interval time.Duration
	// SnapshotTimeout bounds one Cluster.Snapshot call. Zero means DefaultSnapshotTimeout.
	SnapshotTimeout time.Duration
	// Retention is the prune horizon. Zero disables pruning.
	Retention time.Duration
	// PruneInterval is how often Prune runs. Zero means Interval.
	PruneInterval time.Duration
}

// Defaults used when Config leaves a field zero.
const (
	DefaultInterval        = time.Minute
	DefaultSnapshotTimeout = 10 * time.Second
)

// Cluster is one source of snapshots for the collector.
type Cluster struct {
	ID       string
	Snapshot func(context.Context) model.Snapshot
}

// Store is the persistence backend. Apply must be atomic: either the whole
// Batch lands or nothing does. Prune deletes rows older than the horizon.
type Store interface {
	Apply(context.Context, Batch) error
	Prune(context.Context, time.Time) error
	Close() error
}

// Querier is the read side of the statistics database, used by the hub UI
// (internal/web stays stdlib-only and talks to this interface through its own
// StatsSource port). postgresStore implements both Store and Querier; the
// service keeps them apart so the writer path cannot accidentally depend on the
// reader and tests can fake either side.
type Querier interface {
	// Ping reports whether the database answers right now.
	Ping(context.Context) error
	// Overview returns the aggregate header of the statistics tab.
	Overview(context.Context, time.Duration) (model.StatsOverview, error)
	// Nodes returns the newest sample of every known node.
	Nodes(context.Context) ([]model.StatsNodeSample, error)
	// Images returns one page of image presence intervals.
	Images(context.Context, model.StatsImageQuery) (model.StatsImagePage, error)
	// GCEvents returns one page of image-collector events.
	GCEvents(context.Context, model.StatsEventQuery) (model.StatsGCPage, error)
	// Repulls returns one page of re-pulls after a collector removal.
	Repulls(context.Context, model.StatsEventQuery) (model.StatsRepullPage, error)
	// Tasks returns one page of the pull-task log.
	Tasks(context.Context, model.StatsTaskQuery) (model.StatsTaskPage, error)
}

// Batch is one collector step for one cluster. The zero value is a valid empty
// batch (Apply still runs the idempotent DDL).
type Batch struct {
	Cluster string
	At      time.Time

	// Tasks are pull-task upserts keyed by cluster+task ID.
	Tasks []TaskUpsert
	// NodeSamples are node inventory samples.
	NodeSamples []NodeSample
	// PresenceSeen opens (or refreshes) an image presence interval on a node.
	PresenceSeen []PresenceSeen
	// PresenceRemoved closes the open interval for an image that disappeared.
	PresenceRemoved []PresenceRemoved
	// GCEvents aggregates one event per node per step that lost images.
	GCEvents []GCEvent
	// Repulls records succeeded tasks whose image was absent when the task started.
	Repulls []Repull
}

// TaskUpsert is one pull-task upsert. Task.Cluster is filled in by Step.
type TaskUpsert struct {
	Task     model.PullTask
	Observed time.Time
}

// NodeSample is one node inventory sample.
type NodeSample struct {
	Node      string
	SampledAt time.Time
	Ready     bool
	AgentUp   bool
	// ImageFS is the containerd image filesystem view reported by the agent.
	ImageFSUsedBytes         int64
	ImageFSCapacityBytes     int64
	ImageFSCapacityEstimated bool
	CachedImageCount         int
	DiskPaused               bool
}

// PresenceSeen is an image ref observed in NodeInfo.CachedImages. The first
// observation opens a presence interval; later ones refresh last_seen_at.
type PresenceSeen struct {
	Node        string
	Image       string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	SizeBytes   int64
}

// PresenceRemoved closes the open presence interval for (node, image).
type PresenceRemoved struct {
	Node      string
	Image     string
	RemovedAt time.Time
	SizeBytes int64
}

// GCEvent is one image-collector pass observed on a node. WindowSeconds is the
// time since the previous AgentUp observation of that node.
type GCEvent struct {
	Node          string
	At            time.Time
	RemovedCount  int
	RemovedBytes  int64
	WindowSeconds float64
}

// Repull is a pull task whose image had been removed by the collector before
// the task started. LagSeconds is finished_at - removed_at.
type Repull struct {
	TaskID     string
	Image      string
	Node       string
	RemovedAt  time.Time
	FinishedAt time.Time
	LagSeconds float64
}
