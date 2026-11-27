package stats

// schemaStatements is the idempotent DDL applied on connect and on the first
// Apply. Every table carries the cluster name so one database can serve several
// hubs/clusters.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS prepuller_tasks (
	cluster text NOT NULL,
	task_id text NOT NULL,
	image text NOT NULL,
	node text NOT NULL,
	state text NOT NULL,
	attempt integer NOT NULL DEFAULT 0,
	"trigger" text NOT NULL DEFAULT '',
	actor text NOT NULL DEFAULT '',
	error text NOT NULL DEFAULT '',
	created_at timestamptz,
	started_at timestamptz,
	finished_at timestamptz,
	duration_ms bigint NOT NULL DEFAULT 0,
	observed_at timestamptz NOT NULL,
	state_changed_at timestamptz NOT NULL,
	PRIMARY KEY (cluster, task_id)
)`,
	`CREATE INDEX IF NOT EXISTS prepuller_tasks_finished_idx ON prepuller_tasks (finished_at)`,
	`CREATE INDEX IF NOT EXISTS prepuller_tasks_image_idx ON prepuller_tasks (cluster, image, node)`,
	`CREATE TABLE IF NOT EXISTS prepuller_image_presence (
	cluster text NOT NULL,
	node text NOT NULL,
	image text NOT NULL,
	first_seen_at timestamptz NOT NULL,
	last_seen_at timestamptz NOT NULL,
	size_bytes bigint NOT NULL DEFAULT 0,
	removed_at timestamptz
)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS prepuller_image_presence_open_uidx ON prepuller_image_presence (cluster, node, image) WHERE removed_at IS NULL`,
	`CREATE INDEX IF NOT EXISTS prepuller_image_presence_removed_idx ON prepuller_image_presence (removed_at)`,
	`CREATE TABLE IF NOT EXISTS prepuller_gc_events (
	cluster text NOT NULL,
	node text NOT NULL,
	occurred_at timestamptz NOT NULL,
	removed_count integer NOT NULL,
	removed_bytes bigint NOT NULL,
	window_seconds double precision NOT NULL,
	PRIMARY KEY (cluster, node, occurred_at)
)`,
	`CREATE INDEX IF NOT EXISTS prepuller_gc_events_occurred_idx ON prepuller_gc_events (occurred_at)`,
	`CREATE TABLE IF NOT EXISTS prepuller_repulls (
	cluster text NOT NULL,
	task_id text NOT NULL,
	node text NOT NULL,
	image text NOT NULL,
	removed_at timestamptz NOT NULL,
	finished_at timestamptz NOT NULL,
	lag_seconds double precision NOT NULL,
	PRIMARY KEY (cluster, task_id)
)`,
	`CREATE INDEX IF NOT EXISTS prepuller_repulls_finished_idx ON prepuller_repulls (finished_at)`,
	`CREATE TABLE IF NOT EXISTS prepuller_node_samples (
	cluster text NOT NULL,
	node text NOT NULL,
	sampled_at timestamptz NOT NULL,
	ready boolean NOT NULL,
	agent_up boolean NOT NULL,
	disk_paused boolean NOT NULL,
	image_fs_used_bytes bigint NOT NULL,
	image_fs_capacity_bytes bigint NOT NULL,
	image_fs_capacity_estimated boolean NOT NULL,
	cached_image_count integer NOT NULL,
	PRIMARY KEY (cluster, node, sampled_at)
)`,
	`CREATE INDEX IF NOT EXISTS prepuller_node_samples_sampled_idx ON prepuller_node_samples (sampled_at)`,
}

const upsertTaskSQL = `INSERT INTO prepuller_tasks (
	cluster, task_id, image, node, state, attempt, "trigger", actor, error,
	created_at, started_at, finished_at, duration_ms, observed_at, state_changed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT (cluster, task_id) DO UPDATE SET
	image = EXCLUDED.image,
	node = EXCLUDED.node,
	state = EXCLUDED.state,
	attempt = EXCLUDED.attempt,
	"trigger" = EXCLUDED."trigger",
	actor = EXCLUDED.actor,
	error = EXCLUDED.error,
	started_at = EXCLUDED.started_at,
	finished_at = EXCLUDED.finished_at,
	duration_ms = EXCLUDED.duration_ms,
	observed_at = EXCLUDED.observed_at,
	state_changed_at = CASE
		WHEN prepuller_tasks.state IS DISTINCT FROM EXCLUDED.state THEN EXCLUDED.observed_at
		ELSE prepuller_tasks.state_changed_at
	END`

const upsertPresenceSeenSQL = `INSERT INTO prepuller_image_presence (
	cluster, node, image, first_seen_at, last_seen_at, size_bytes
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (cluster, node, image) WHERE removed_at IS NULL DO UPDATE SET
	last_seen_at = EXCLUDED.last_seen_at,
	size_bytes = EXCLUDED.size_bytes`

const closePresenceSQL = `UPDATE prepuller_image_presence
SET removed_at = $4
WHERE cluster = $1 AND node = $2 AND image = $3 AND removed_at IS NULL`

const insertGCEventSQL = `INSERT INTO prepuller_gc_events (
	cluster, node, occurred_at, removed_count, removed_bytes, window_seconds
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (cluster, node, occurred_at) DO NOTHING`

const insertRepullSQL = `INSERT INTO prepuller_repulls (
	cluster, task_id, node, image, removed_at, finished_at, lag_seconds
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (cluster, task_id) DO NOTHING`

const upsertNodeSampleSQL = `INSERT INTO prepuller_node_samples (
	cluster, node, sampled_at, ready, agent_up, disk_paused,
	image_fs_used_bytes, image_fs_capacity_bytes, image_fs_capacity_estimated,
	cached_image_count
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (cluster, node, sampled_at) DO UPDATE SET
	ready = EXCLUDED.ready,
	agent_up = EXCLUDED.agent_up,
	disk_paused = EXCLUDED.disk_paused,
	image_fs_used_bytes = EXCLUDED.image_fs_used_bytes,
	image_fs_capacity_bytes = EXCLUDED.image_fs_capacity_bytes,
	image_fs_capacity_estimated = EXCLUDED.image_fs_capacity_estimated,
	cached_image_count = EXCLUDED.cached_image_count`

// pruneStatements delete rows older than the retention horizon ($1).
var pruneStatements = []string{
	`DELETE FROM prepuller_tasks WHERE COALESCE(finished_at, created_at) < $1 AND state NOT IN ('queued', 'running')`,
	`DELETE FROM prepuller_image_presence WHERE removed_at IS NOT NULL AND removed_at < $1`,
	`DELETE FROM prepuller_gc_events WHERE occurred_at < $1`,
	`DELETE FROM prepuller_repulls WHERE finished_at < $1`,
	`DELETE FROM prepuller_node_samples WHERE sampled_at < $1`,
}
