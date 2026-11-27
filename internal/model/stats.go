package model

import "time"

// Statistics view types for the optional PostgreSQL pipeline (ADR-002).
//
// These are hub-only: they never cross the hub<->spoke wire, so they are not part
// of contract v2. They live in model because both the collector package
// (internal/stats) and the HTTP layer (internal/web) speak them, and model is the
// only shared place both may import: internal/web is stdlib-only and must not
// pull pgx in through internal/stats (see AGENTS §3).
//
// Like the rest of the API contract the JSON tags are camelCase and every slice
// is rendered as an array, never null (the UI does .map/.length on it).

// StatsInfo describes the statistics pipeline as the hub currently sees it.
// Enabled=false means the operator did not configure a DSN ("admin disabled the
// collection" in the UI); Enabled=true with OK=false means the database cannot
// be reached right now.
type StatsInfo struct {
	Enabled bool   `json:"enabled"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	// Clusters are the cluster IDs the collector samples.
	Clusters []string `json:"clusters"`
	// IntervalSeconds and RetentionSeconds describe the collector configuration.
	IntervalSeconds  int64 `json:"intervalSeconds,omitempty"`
	RetentionSeconds int64 `json:"retentionSeconds,omitempty"`
	// ConnectedAt is when the current database connection was established.
	ConnectedAt *time.Time `json:"connectedAt,omitempty"`
	// CheckedAt is when the hub actually probed the database for this answer.
	CheckedAt time.Time `json:"checkedAt"`
}

// StatsOverviewQuery is the filter of the aggregate header. Window is clamped
// by the store (non-positive means the 24h default); Cluster narrows every
// aggregate to one cluster, the same filter the list endpoints take.
type StatsOverviewQuery struct {
	Window  time.Duration
	Cluster string
}

// StatsOverview is the header of the statistics tab: one window of aggregates
// computed from all five tables, narrowed to one cluster when the query asked
// for it.
type StatsOverview struct {
	// WindowSeconds is the effective window (the API clamps the request).
	WindowSeconds int64     `json:"windowSeconds"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
	// Cluster is the filter the aggregates were computed for; empty means all
	// clusters. Clusters below always lists every known cluster so the UI can
	// still populate its filter select while a cluster is selected.
	Cluster  string   `json:"cluster,omitempty"`
	Clusters []string `json:"clusters"`

	// Nodes is the number of (cluster, node) pairs with at least one sample.
	Nodes int `json:"nodes"`

	// OpenImages now: image presence intervals without removed_at.
	OpenImages      int   `json:"openImages"`
	OpenImagesBytes int64 `json:"openImagesBytes"`

	// ClosedIntervals are intervals closed inside the window; the lifetime
	// percentiles are computed over exactly those rows.
	ClosedIntervals       int     `json:"closedIntervals"`
	MedianLifetimeSeconds float64 `json:"medianLifetimeSeconds"`
	P95LifetimeSeconds    float64 `json:"p95LifetimeSeconds"`

	// Image-collector activity inside the window.
	GCRuns          int   `json:"gcRuns"`
	GCRemovedImages int   `json:"gcRemovedImages"`
	GCRemovedBytes  int64 `json:"gcRemovedBytes"`

	// Re-pulls inside the window: how often a removed image had to be pulled again.
	Repulls                int     `json:"repulls"`
	RepullMedianLagSeconds float64 `json:"repullMedianLagSeconds"`
	RepullMaxLagSeconds    float64 `json:"repullMaxLagSeconds"`

	// Pull-task log inside the window.
	Tasks              int     `json:"tasks"`
	TasksSucceeded     int     `json:"tasksSucceeded"`
	TasksFailed        int     `json:"tasksFailed"`
	TasksActive        int     `json:"tasksActive"`
	TasksAvgDurationMs float64 `json:"tasksAvgDurationMs"`

	// LastSampleAt is the newest node sample in the whole database (not only the
	// window), so a stalled collector is visible; nil when no sample exists yet.
	LastSampleAt *time.Time `json:"lastSampleAt,omitempty"`
	// LastTaskAt is the newest observed pull task, when there is one.
	LastTaskAt *time.Time `json:"lastTaskAt,omitempty"`
}

// StatsNodeSample is the newest inventory sample of one node.
type StatsNodeSample struct {
	Cluster                  string    `json:"cluster"`
	Node                     string    `json:"node"`
	SampledAt                time.Time `json:"sampledAt"`
	Ready                    bool      `json:"ready"`
	AgentUp                  bool      `json:"agentUp"`
	DiskPaused               bool      `json:"diskPaused"`
	ImageFSUsedBytes         int64     `json:"imageFsUsedBytes"`
	ImageFSCapacityBytes     int64     `json:"imageFsCapacityBytes"`
	ImageFSCapacityEstimated bool      `json:"imageFsCapacityEstimated"`
	CachedImageCount         int       `json:"cachedImageCount"`
}

// StatsImage is one image presence interval on one node.
type StatsImage struct {
	Cluster     string     `json:"cluster"`
	Node        string     `json:"node"`
	Image       string     `json:"image"`
	FirstSeenAt time.Time  `json:"firstSeenAt"`
	LastSeenAt  time.Time  `json:"lastSeenAt"`
	RemovedAt   *time.Time `json:"removedAt,omitempty"`
	SizeBytes   int64      `json:"sizeBytes"`
	// LifetimeSeconds is removed_at-first_seen_at for closed intervals and
	// last_seen_at-first_seen_at for the ones still open.
	LifetimeSeconds float64 `json:"lifetimeSeconds"`
}

// StatsGCEvent is one image-collector pass that removed images from a node.
type StatsGCEvent struct {
	Cluster       string    `json:"cluster"`
	Node          string    `json:"node"`
	OccurredAt    time.Time `json:"occurredAt"`
	RemovedCount  int       `json:"removedCount"`
	RemovedBytes  int64     `json:"removedBytes"`
	WindowSeconds float64   `json:"windowSeconds"`
}

// StatsRepull is a pull that happened after the collector had removed the image.
type StatsRepull struct {
	Cluster    string    `json:"cluster"`
	TaskID     string    `json:"taskId"`
	Image      string    `json:"image"`
	Node       string    `json:"node"`
	RemovedAt  time.Time `json:"removedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	LagSeconds float64   `json:"lagSeconds"`
}

// StatsTask is one row of the pull-task log written by the statistics collector
// (not the hub's own pull-task history: this one survives restarts and pruning).
type StatsTask struct {
	Cluster        string     `json:"cluster"`
	TaskID         string     `json:"taskId"`
	Image          string     `json:"image"`
	Node           string     `json:"node"`
	State          string     `json:"state"`
	Attempt        int        `json:"attempt"`
	Trigger        string     `json:"trigger"`
	Actor          string     `json:"actor"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      *time.Time `json:"createdAt,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	DurationMs     int64      `json:"durationMs"`
	ObservedAt     time.Time  `json:"observedAt"`
	StateChangedAt time.Time  `json:"stateChangedAt"`
}

// StatsImagePage is one page of the image presence list.
type StatsImagePage struct {
	Total  int          `json:"total"`
	Offset int          `json:"offset"`
	Limit  int          `json:"limit"`
	Rows   []StatsImage `json:"rows"`
}

// StatsGCPage is one page of the image-collector events.
type StatsGCPage struct {
	Total  int            `json:"total"`
	Offset int            `json:"offset"`
	Limit  int            `json:"limit"`
	Rows   []StatsGCEvent `json:"rows"`
}

// StatsRepullPage is one page of the re-pull log.
type StatsRepullPage struct {
	Total  int           `json:"total"`
	Offset int           `json:"offset"`
	Limit  int           `json:"limit"`
	Rows   []StatsRepull `json:"rows"`
}

// StatsTaskPage is one page of the pull-task log.
type StatsTaskPage struct {
	Total  int         `json:"total"`
	Offset int         `json:"offset"`
	Limit  int         `json:"limit"`
	Rows   []StatsTask `json:"rows"`
}

// StatsImageQuery filters the image presence list. State is "open" (removed_at
// IS NULL), "removed" (removed inside the window) or "" for both.
type StatsImageQuery struct {
	Window  time.Duration
	Cluster string
	State   string
	Search  string
	Limit   int
	Offset  int
}

// StatsTaskQuery filters the pull-task log. State is a task state or "active"
// (queued+running); Trigger filters by pull trigger.
type StatsTaskQuery struct {
	Window  time.Duration
	Cluster string
	State   string
	Trigger string
	Search  string
	Limit   int
	Offset  int
}

// StatsEventQuery filters the collector-event lists (GC events and re-pulls).
type StatsEventQuery struct {
	Window  time.Duration
	Cluster string
	Limit   int
	Offset  int
}
