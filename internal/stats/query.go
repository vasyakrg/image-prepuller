package stats

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// Read-side defaults. The HTTP layer normalises the window and paging already,
// but the store defends itself too: it is called from tests and future callers.
const (
	defaultQueryWindow = 24 * time.Hour
	defaultQueryLimit  = 50
)

// Ping reports whether the pool answers right now. It is the read-side health
// probe behind model.StatsInfo.OK.
func (s *postgresStore) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("stats: ping: %w", err)
	}
	return nil
}

// Overview returns the aggregate header of the stats tab. It runs several small
// sequential queries: the statistics tables are tiny (one row per node sample or
// task) and one statement per aggregate keeps every NULL/COALESCE explicit
// instead of a single unreadable UNION.
//
// A non-positive window falls back to defaultQueryWindow; the effective values
// are echoed in the result so the UI can render the window it actually got.
func (s *postgresStore) Overview(ctx context.Context, window time.Duration) (model.StatsOverview, error) {
	window = normalizeQueryWindow(window)
	now := time.Now()
	from := now.Add(-window)
	out := model.StatsOverview{
		WindowSeconds: int64(window / time.Second),
		From:          from,
		To:            now,
		Clusters:      []string{},
	}

	clusters, err := s.queryStrings(ctx, overviewClustersSQL)
	if err != nil {
		return model.StatsOverview{}, fmt.Errorf("stats: overview: clusters: %w", err)
	}
	out.Clusters = clusters

	var nodes int64
	if err := s.db.QueryRowContext(ctx, overviewNodesSQL).Scan(&nodes); err != nil {
		return model.StatsOverview{}, fmt.Errorf("stats: overview: nodes: %w", err)
	}
	out.Nodes = int(nodes)

	// SUM/percentile/AVG over an empty window return SQL NULL; the SQL already
	// wraps them in COALESCE, and the sql.Null* scans below keep a NULL from any
	// driver or fake from panicking the read path: NULL means 0.
	var (
		openImages int64
		openBytes  sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx, overviewOpenImagesSQL).Scan(&openImages, &openBytes); err != nil {
		return model.StatsOverview{}, fmt.Errorf("stats: overview: open images: %w", err)
	}
	out.OpenImages = int(openImages)
	out.OpenImagesBytes = nullInt64(openBytes)

	var (
		closed int64
		median sql.NullFloat64
		p95    sql.NullFloat64
	)
	if err := s.db.QueryRowContext(ctx, overviewClosedSQL, from).Scan(&closed, &median, &p95); err != nil {
		return model.StatsOverview{}, fmt.Errorf("stats: overview: closed intervals: %w", err)
	}
	out.ClosedIntervals = int(closed)
	out.MedianLifetimeSeconds = nullFloat64(median)
	out.P95LifetimeSeconds = nullFloat64(p95)

	var (
		gcRuns   int64
		gcImages sql.NullInt64
		gcBytes  sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx, overviewGCSQL, from).Scan(&gcRuns, &gcImages, &gcBytes); err != nil {
		return model.StatsOverview{}, fmt.Errorf("stats: overview: gc events: %w", err)
	}
	out.GCRuns = int(gcRuns)
	out.GCRemovedImages = int(nullInt64(gcImages))
	out.GCRemovedBytes = nullInt64(gcBytes)

	var (
		repulls   int64
		repullMed sql.NullFloat64
		repullMax sql.NullFloat64
	)
	if err := s.db.QueryRowContext(ctx, overviewRepullsSQL, from).Scan(&repulls, &repullMed, &repullMax); err != nil {
		return model.StatsOverview{}, fmt.Errorf("stats: overview: repulls: %w", err)
	}
	out.Repulls = int(repulls)
	out.RepullMedianLagSeconds = nullFloat64(repullMed)
	out.RepullMaxLagSeconds = nullFloat64(repullMax)

	var (
		tasks     int64
		succeeded sql.NullInt64
		failed    sql.NullInt64
		active    sql.NullInt64
		avgMs     sql.NullFloat64
	)
	if err := s.db.QueryRowContext(ctx, overviewTasksSQL, from).Scan(&tasks, &succeeded, &failed, &active, &avgMs); err != nil {
		return model.StatsOverview{}, fmt.Errorf("stats: overview: tasks: %w", err)
	}
	out.Tasks = int(tasks)
	out.TasksSucceeded = int(nullInt64(succeeded))
	out.TasksFailed = int(nullInt64(failed))
	out.TasksActive = int(nullInt64(active))
	out.TasksAvgDurationMs = nullFloat64(avgMs)

	lastSample, err := s.queryNullTime(ctx, overviewLastSampleSQL)
	if err != nil {
		return model.StatsOverview{}, fmt.Errorf("stats: overview: last sample: %w", err)
	}
	out.LastSampleAt = lastSample

	lastTask, err := s.queryNullTime(ctx, overviewLastTaskSQL)
	if err != nil {
		return model.StatsOverview{}, fmt.Errorf("stats: overview: last task: %w", err)
	}
	out.LastTaskAt = lastTask

	return out, nil
}

// Nodes returns the newest sample of every known node: DISTINCT ON keeps the
// first row per (cluster, node) after the descending sampled_at sort.
func (s *postgresStore) Nodes(ctx context.Context) ([]model.StatsNodeSample, error) {
	rows, err := s.db.QueryContext(ctx, nodesSQL)
	if err != nil {
		return nil, fmt.Errorf("stats: nodes: %w", err)
	}
	defer rows.Close()

	out := []model.StatsNodeSample{}
	for rows.Next() {
		var (
			item      model.StatsNodeSample
			sampledAt time.Time
		)
		if err := rows.Scan(
			&item.Cluster,
			&item.Node,
			&sampledAt,
			&item.Ready,
			&item.AgentUp,
			&item.DiskPaused,
			&item.ImageFSUsedBytes,
			&item.ImageFSCapacityBytes,
			&item.ImageFSCapacityEstimated,
			&item.CachedImageCount,
		); err != nil {
			return nil, fmt.Errorf("stats: nodes: scan: %w", err)
		}
		item.SampledAt = sampledAt
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stats: nodes: %w", err)
	}
	return out, nil
}

// Images returns one page of image presence intervals. State selects open
// intervals, intervals closed inside the window, or both; Search is a substring
// of the image ref. Every user string travels as a bind argument.
func (s *postgresStore) Images(ctx context.Context, q model.StatsImageQuery) (model.StatsImagePage, error) {
	from := time.Now().Add(-normalizeQueryWindow(q.Window))
	limit, offset := normalizeQueryPage(q.Limit, q.Offset)
	page := model.StatsImagePage{
		Limit:  limit,
		Offset: offset,
		Rows:   []model.StatsImage{},
	}
	where, args := imageWhereSQL(q, from)

	var total int64
	if err := s.db.QueryRowContext(ctx, imageCountSQL+where, args...).Scan(&total); err != nil {
		return model.StatsImagePage{}, fmt.Errorf("stats: images: count: %w", err)
	}
	page.Total = int(total)

	pageArgs := make([]any, 0, len(args)+2)
	pageArgs = append(pageArgs, args...)
	pageArgs = append(pageArgs, limit, offset)
	query := imagePageSQL + where +
		fmt.Sprintf(" ORDER BY COALESCE(removed_at, last_seen_at) DESC, cluster, node, image LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)

	rows, err := s.db.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return model.StatsImagePage{}, fmt.Errorf("stats: images: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			item      model.StatsImage
			firstSeen time.Time
			lastSeen  time.Time
			removed   sql.NullTime
		)
		if err := rows.Scan(
			&item.Cluster,
			&item.Node,
			&item.Image,
			&firstSeen,
			&lastSeen,
			&removed,
			&item.SizeBytes,
			&item.LifetimeSeconds,
		); err != nil {
			return model.StatsImagePage{}, fmt.Errorf("stats: images: scan: %w", err)
		}
		item.FirstSeenAt = firstSeen
		item.LastSeenAt = lastSeen
		if removed.Valid {
			at := removed.Time
			item.RemovedAt = &at
		}
		page.Rows = append(page.Rows, item)
	}
	if err := rows.Err(); err != nil {
		return model.StatsImagePage{}, fmt.Errorf("stats: images: %w", err)
	}
	return page, nil
}

// GCEvents returns one page of image-collector events inside the window.
func (s *postgresStore) GCEvents(ctx context.Context, q model.StatsEventQuery) (model.StatsGCPage, error) {
	from := time.Now().Add(-normalizeQueryWindow(q.Window))
	limit, offset := normalizeQueryPage(q.Limit, q.Offset)
	page := model.StatsGCPage{
		Limit:  limit,
		Offset: offset,
		Rows:   []model.StatsGCEvent{},
	}
	where, args := eventWhereSQL("occurred_at", q, from)

	var total int64
	if err := s.db.QueryRowContext(ctx, gcEventCountSQL+where, args...).Scan(&total); err != nil {
		return model.StatsGCPage{}, fmt.Errorf("stats: gc events: count: %w", err)
	}
	page.Total = int(total)

	pageArgs := make([]any, 0, len(args)+2)
	pageArgs = append(pageArgs, args...)
	pageArgs = append(pageArgs, limit, offset)
	query := gcEventPageSQL + where +
		fmt.Sprintf(" ORDER BY occurred_at DESC, cluster, node LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)

	rows, err := s.db.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return model.StatsGCPage{}, fmt.Errorf("stats: gc events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			item       model.StatsGCEvent
			occurredAt time.Time
		)
		if err := rows.Scan(
			&item.Cluster,
			&item.Node,
			&occurredAt,
			&item.RemovedCount,
			&item.RemovedBytes,
			&item.WindowSeconds,
		); err != nil {
			return model.StatsGCPage{}, fmt.Errorf("stats: gc events: scan: %w", err)
		}
		item.OccurredAt = occurredAt
		page.Rows = append(page.Rows, item)
	}
	if err := rows.Err(); err != nil {
		return model.StatsGCPage{}, fmt.Errorf("stats: gc events: %w", err)
	}
	return page, nil
}

// Repulls returns one page of re-pulls that followed a collector removal.
func (s *postgresStore) Repulls(ctx context.Context, q model.StatsEventQuery) (model.StatsRepullPage, error) {
	from := time.Now().Add(-normalizeQueryWindow(q.Window))
	limit, offset := normalizeQueryPage(q.Limit, q.Offset)
	page := model.StatsRepullPage{
		Limit:  limit,
		Offset: offset,
		Rows:   []model.StatsRepull{},
	}
	where, args := eventWhereSQL("finished_at", q, from)

	var total int64
	if err := s.db.QueryRowContext(ctx, repullCountSQL+where, args...).Scan(&total); err != nil {
		return model.StatsRepullPage{}, fmt.Errorf("stats: repulls: count: %w", err)
	}
	page.Total = int(total)

	pageArgs := make([]any, 0, len(args)+2)
	pageArgs = append(pageArgs, args...)
	pageArgs = append(pageArgs, limit, offset)
	query := repullPageSQL + where +
		fmt.Sprintf(" ORDER BY finished_at DESC, cluster, task_id LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)

	rows, err := s.db.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return model.StatsRepullPage{}, fmt.Errorf("stats: repulls: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			item       model.StatsRepull
			removedAt  time.Time
			finishedAt time.Time
		)
		if err := rows.Scan(
			&item.Cluster,
			&item.TaskID,
			&item.Image,
			&item.Node,
			&removedAt,
			&finishedAt,
			&item.LagSeconds,
		); err != nil {
			return model.StatsRepullPage{}, fmt.Errorf("stats: repulls: scan: %w", err)
		}
		item.RemovedAt = removedAt
		item.FinishedAt = finishedAt
		page.Rows = append(page.Rows, item)
	}
	if err := rows.Err(); err != nil {
		return model.StatsRepullPage{}, fmt.Errorf("stats: repulls: %w", err)
	}
	return page, nil
}

// Tasks returns one page of the pull-task log. State "active" means queued or
// running; any other non-empty state is matched exactly. Trigger is matched
// exactly and Search is an image substring.
func (s *postgresStore) Tasks(ctx context.Context, q model.StatsTaskQuery) (model.StatsTaskPage, error) {
	from := time.Now().Add(-normalizeQueryWindow(q.Window))
	limit, offset := normalizeQueryPage(q.Limit, q.Offset)
	page := model.StatsTaskPage{
		Limit:  limit,
		Offset: offset,
		Rows:   []model.StatsTask{},
	}
	where, args := taskWhereSQL(q, from)

	var total int64
	if err := s.db.QueryRowContext(ctx, taskCountSQL+where, args...).Scan(&total); err != nil {
		return model.StatsTaskPage{}, fmt.Errorf("stats: tasks: count: %w", err)
	}
	page.Total = int(total)

	pageArgs := make([]any, 0, len(args)+2)
	pageArgs = append(pageArgs, args...)
	pageArgs = append(pageArgs, limit, offset)
	query := taskPageSQL + where +
		fmt.Sprintf(" ORDER BY observed_at DESC, cluster, task_id LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)

	rows, err := s.db.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return model.StatsTaskPage{}, fmt.Errorf("stats: tasks: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			item       model.StatsTask
			createdAt  sql.NullTime
			startedAt  sql.NullTime
			finishedAt sql.NullTime
			observedAt time.Time
			changedAt  time.Time
		)
		if err := rows.Scan(
			&item.Cluster,
			&item.TaskID,
			&item.Image,
			&item.Node,
			&item.State,
			&item.Attempt,
			&item.Trigger,
			&item.Actor,
			&item.Error,
			&createdAt,
			&startedAt,
			&finishedAt,
			&item.DurationMs,
			&observedAt,
			&changedAt,
		); err != nil {
			return model.StatsTaskPage{}, fmt.Errorf("stats: tasks: scan: %w", err)
		}
		item.CreatedAt = nullTimePtr(createdAt)
		item.StartedAt = nullTimePtr(startedAt)
		item.FinishedAt = nullTimePtr(finishedAt)
		item.ObservedAt = observedAt
		item.StateChangedAt = changedAt
		page.Rows = append(page.Rows, item)
	}
	if err := rows.Err(); err != nil {
		return model.StatsTaskPage{}, fmt.Errorf("stats: tasks: %w", err)
	}
	return page, nil
}

// normalizeQueryWindow clamps a zero or negative window to the default. The HTTP
// layer already normalises, but a direct caller must not get an empty window.
func normalizeQueryWindow(window time.Duration) time.Duration {
	if window <= 0 {
		return defaultQueryWindow
	}
	return window
}

// normalizeQueryPage clamps page size and offset so a hostile query string can
// never produce LIMIT 0 / negative OFFSET or an unbounded scan.
func normalizeQueryPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// queryStrings runs a single-column query and always returns a non-nil slice:
// empty results must serialise as [] in the UI, never null.
func (s *postgresStore) queryStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// queryNullTime runs a single-aggregate query that may return SQL NULL.
func (s *postgresStore) queryNullTime(ctx context.Context, query string, args ...any) (*time.Time, error) {
	var value sql.NullTime
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&value); err != nil {
		return nil, err
	}
	return nullTimePtr(value), nil
}

// nullInt64 and nullFloat64 collapse SQL NULL to zero for aggregate columns.
func nullInt64(value sql.NullInt64) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func nullFloat64(value sql.NullFloat64) float64 {
	if !value.Valid {
		return 0
	}
	return value.Float64
}

// nullTimePtr converts a nullable timestamp into the optional pointer the model
// contract uses. A nil pointer means "no row yet", not the zero time.
func nullTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time
	return &at
}

// imageWhereSQL builds the shared predicate of the count and page queries. Only
// operators and column names are interpolated; every user value is a bind
// argument. State is a closed set: "open", "removed", or "" for both.
//
// The open branch has no time predicate, so it must not take the window
// argument either: bind arguments are numbered by len(args), and an unused $1
// would make pgx reject the statement with a bind-count mismatch.
func imageWhereSQL(q model.StatsImageQuery, from time.Time) (string, []any) {
	args := []any{}
	conditions := []string{}
	switch q.State {
	case "open":
		conditions = append(conditions, "removed_at IS NULL")
	case "removed":
		args = append(args, from)
		conditions = append(conditions, "removed_at IS NOT NULL AND removed_at >= $1")
	default:
		args = append(args, from)
		conditions = append(conditions, "(removed_at IS NULL OR removed_at >= $1)")
	}
	if q.Cluster != "" {
		args = append(args, q.Cluster)
		conditions = append(conditions, fmt.Sprintf("cluster = $%d", len(args)))
	}
	if q.Search != "" {
		args = append(args, q.Search)
		conditions = append(conditions, fmt.Sprintf("image ILIKE '%%' || $%d || '%%'", len(args)))
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// eventWhereSQL builds the window+cluster predicate of GC events and re-pulls.
// column is an internal constant, never user input.
func eventWhereSQL(column string, q model.StatsEventQuery, from time.Time) (string, []any) {
	args := []any{from}
	conditions := []string{column + " >= $1"}
	if q.Cluster != "" {
		args = append(args, q.Cluster)
		conditions = append(conditions, fmt.Sprintf("cluster = $%d", len(args)))
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// taskWhereSQL builds the pull-task predicate. State goes through a closed
// switch: "active" is an internal constant list, everything else is an exact
// bind value.
func taskWhereSQL(q model.StatsTaskQuery, from time.Time) (string, []any) {
	args := []any{from}
	conditions := []string{"observed_at >= $1"}
	if q.Cluster != "" {
		args = append(args, q.Cluster)
		conditions = append(conditions, fmt.Sprintf("cluster = $%d", len(args)))
	}
	switch q.State {
	case "":
	case "active":
		conditions = append(conditions, "state IN ('queued', 'running')")
	default:
		args = append(args, q.State)
		conditions = append(conditions, fmt.Sprintf("state = $%d", len(args)))
	}
	if q.Trigger != "" {
		args = append(args, q.Trigger)
		conditions = append(conditions, fmt.Sprintf(`"trigger" = $%d`, len(args)))
	}
	if q.Search != "" {
		args = append(args, q.Search)
		conditions = append(conditions, fmt.Sprintf("image ILIKE '%%' || $%d || '%%'", len(args)))
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// Read-side SQL. Only aggregate columns are cast explicitly (::bigint /
// ::double precision) so the driver returns plain int64/float64 values that
// database/sql can scan without pgx-specific types.
const (
	overviewClustersSQL = `SELECT cluster FROM (
	SELECT cluster FROM prepuller_tasks
	UNION
	SELECT cluster FROM prepuller_image_presence
	UNION
	SELECT cluster FROM prepuller_gc_events
	UNION
	SELECT cluster FROM prepuller_repulls
	UNION
	SELECT cluster FROM prepuller_node_samples
) AS all_clusters ORDER BY cluster`

	overviewNodesSQL = `SELECT COUNT(*)::bigint FROM (
	SELECT DISTINCT cluster, node FROM prepuller_node_samples
) AS node_pairs`

	overviewOpenImagesSQL = `SELECT COUNT(*)::bigint, COALESCE(SUM(size_bytes), 0)::bigint
FROM prepuller_image_presence
WHERE removed_at IS NULL`

	overviewClosedSQL = `SELECT
	COUNT(*)::bigint,
	COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(epoch FROM removed_at - first_seen_at)), 0)::double precision,
	COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(epoch FROM removed_at - first_seen_at)), 0)::double precision
FROM prepuller_image_presence
WHERE removed_at IS NOT NULL AND removed_at >= $1`

	overviewGCSQL = `SELECT
	COUNT(*)::bigint,
	COALESCE(SUM(removed_count), 0)::bigint,
	COALESCE(SUM(removed_bytes), 0)::bigint
FROM prepuller_gc_events
WHERE occurred_at >= $1`

	overviewRepullsSQL = `SELECT
	COUNT(*)::bigint,
	COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY lag_seconds), 0)::double precision,
	COALESCE(MAX(lag_seconds), 0)::double precision
FROM prepuller_repulls
WHERE finished_at >= $1`

	overviewTasksSQL = `SELECT
	COUNT(*)::bigint,
	COALESCE(SUM(CASE WHEN state = 'succeeded' THEN 1 ELSE 0 END), 0)::bigint,
	COALESCE(SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END), 0)::bigint,
	COALESCE(SUM(CASE WHEN state IN ('queued', 'running') THEN 1 ELSE 0 END), 0)::bigint,
	COALESCE(AVG(duration_ms) FILTER (WHERE state IN ('succeeded', 'failed', 'canceled')), 0)::double precision
FROM prepuller_tasks
WHERE observed_at >= $1`

	overviewLastSampleSQL = `SELECT MAX(sampled_at) FROM prepuller_node_samples`
	overviewLastTaskSQL   = `SELECT MAX(observed_at) FROM prepuller_tasks`

	nodesSQL = `SELECT DISTINCT ON (cluster, node)
	cluster, node, sampled_at, ready, agent_up, disk_paused,
	image_fs_used_bytes, image_fs_capacity_bytes, image_fs_capacity_estimated,
	cached_image_count
FROM prepuller_node_samples
ORDER BY cluster, node, sampled_at DESC`

	imageCountSQL = `SELECT COUNT(*)::bigint FROM prepuller_image_presence`
	imagePageSQL  = `SELECT
	cluster, node, image, first_seen_at, last_seen_at, removed_at, size_bytes,
	EXTRACT(epoch FROM COALESCE(removed_at, last_seen_at) - first_seen_at)::double precision
FROM prepuller_image_presence`

	gcEventCountSQL = `SELECT COUNT(*)::bigint FROM prepuller_gc_events`
	gcEventPageSQL  = `SELECT
	cluster, node, occurred_at, removed_count, removed_bytes, window_seconds
FROM prepuller_gc_events`

	repullCountSQL = `SELECT COUNT(*)::bigint FROM prepuller_repulls`
	repullPageSQL  = `SELECT
	cluster, task_id, image, node, removed_at, finished_at, lag_seconds
FROM prepuller_repulls`

	taskCountSQL = `SELECT COUNT(*)::bigint FROM prepuller_tasks`
	taskPageSQL  = `SELECT
	cluster, task_id, image, node, state, attempt, "trigger", actor, error,
	created_at, started_at, finished_at, duration_ms, observed_at, state_changed_at
FROM prepuller_tasks`
)
