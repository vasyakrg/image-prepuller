package stats

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// Column sets of the read queries. They mirror the SELECT lists in query.go.
var (
	clusterColumn     = []string{"cluster"}
	countColumn       = []string{"count"}
	openImagesColumns = []string{"open_images", "open_bytes"}
	closedColumns     = []string{"closed", "median", "p95"}
	gcOverviewColumns = []string{"runs", "images", "bytes"}
	repullOverColumns = []string{"repulls", "median", "max"}
	taskOverviewCols  = []string{"tasks", "succeeded", "failed", "active", "avg"}
	timeColumn        = []string{"last"}

	nodeColumns = []string{
		"cluster", "node", "sampled_at", "ready", "agent_up", "disk_paused",
		"used", "capacity", "estimated", "cached",
	}
	imageColumns = []string{
		"cluster", "node", "image", "first_seen_at", "last_seen_at",
		"removed_at", "size_bytes", "lifetime",
	}
	gcEventColumns = []string{
		"cluster", "node", "occurred_at", "removed_count", "removed_bytes", "window_seconds",
	}
	repullColumns = []string{
		"cluster", "task_id", "image", "node", "removed_at", "finished_at", "lag_seconds",
	}
	taskColumns = []string{
		"cluster", "task_id", "image", "node", "state", "attempt", "trigger", "actor",
		"error", "created_at", "started_at", "finished_at", "duration_ms", "observed_at",
		"state_changed_at",
	}
)

// openFakeQuerier opens a postgresStore on a fresh fake driver and clears the
// schema writes, so db.snapshot()/db.querySnapshot() contain only read traffic.
func openFakeQuerier(t *testing.T) (*postgresStore, *fakeDB) {
	t.Helper()
	db := &fakeDB{}
	store, ok := openFakeStore(t, db).(*postgresStore)
	if !ok {
		t.Fatal("OpenPostgres did not return *postgresStore")
	}
	db.reset()
	return store, db
}

func queryAt(t *testing.T, db *fakeDB, index int) fakeExec {
	t.Helper()
	queries := db.querySnapshot()
	if index < 0 || index >= len(queries) {
		t.Fatalf("query %d missing, recorded %d: %+v", index, len(queries), queries)
	}
	return queries[index]
}

func wantSQL(t *testing.T, exec fakeExec, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(exec.query, fragment) {
			t.Fatalf("query %q does not contain %q", exec.query, fragment)
		}
	}
}

func wantArgs(t *testing.T, exec fakeExec, want ...any) {
	t.Helper()
	if len(exec.args) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(exec.args, want) {
		t.Fatalf("args = %#v, want %#v", exec.args, want)
	}
}

// argTime returns the bind argument at index expected to be a time.Time.
func argTime(t *testing.T, exec fakeExec, index int) time.Time {
	t.Helper()
	if index >= len(exec.args) {
		t.Fatalf("query %q has %d arguments, want index %d", exec.query, len(exec.args), index)
	}
	value, ok := exec.args[index].(time.Time)
	if !ok {
		t.Fatalf("argument %d = %#v, want time.Time", index, exec.args[index])
	}
	return value
}

// wantWindowStart checks that a query receives time.Now()-window as its first
// bind argument and returns it, so later assertions can reuse the exact value.
func wantWindowStart(t *testing.T, exec fakeExec, window time.Duration) time.Time {
	t.Helper()
	from := argTime(t, exec, 0)
	if diff := from.Sub(time.Now().Add(-window)); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("window start = %v, want about %v ago (diff %v)", from, window, diff)
	}
	return from
}

// placeholderRE matches a positional bind placeholder such as $3.
var placeholderRE = regexp.MustCompile(`\$(\d+)`)

// wantPlaceholdersMatchArgs guards the invariant the fake driver cannot see: a
// real PostgreSQL/pgx statement is rejected when the number of bind arguments
// does not match the placeholders. Every placeholder 1..max must exist and max
// must equal len(args).
func wantPlaceholdersMatchArgs(t *testing.T, exec fakeExec) {
	t.Helper()
	seen := map[int]bool{}
	for _, match := range placeholderRE.FindAllStringSubmatch(exec.query, -1) {
		n, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("unparsable placeholder %q in %q", match[0], exec.query)
		}
		seen[n] = true
	}
	if len(seen) != len(exec.args) {
		t.Fatalf("query has %d distinct placeholders but %d bind arguments: %q args=%#v",
			len(seen), len(exec.args), exec.query, exec.args)
	}
	for i := 1; i <= len(seen); i++ {
		if !seen[i] {
			t.Fatalf("placeholder $%d is missing in %q", i, exec.query)
		}
	}
}

func wantWindowArg(t *testing.T, exec fakeExec, want time.Time) {
	t.Helper()
	if len(exec.args) == 0 {
		t.Fatalf("query %q has no bind arguments", exec.query)
	}
	from, ok := exec.args[0].(time.Time)
	if !ok {
		t.Fatalf("first argument = %#v, want time.Time", exec.args[0])
	}
	if !from.Equal(want) {
		t.Fatalf("window start = %v, want %v", from, want)
	}
}

// overviewFixture is one canned answer of the nine Overview queries.
type overviewFixture struct {
	clusters   []string
	nodes      int64
	openImages int64
	openBytes  any
	closed     int64
	median     any
	p95        any
	gcRuns     int64
	gcImages   any
	gcBytes    any
	repulls    int64
	repullMed  any
	repullMax  any
	tasks      int64
	succeeded  any
	failed     any
	active     any
	avgMs      any
	lastSample any
	lastTask   any
}

func (f overviewFixture) enqueue(db *fakeDB) {
	clusterRows := make([][]driver.Value, 0, len(f.clusters))
	for _, cluster := range f.clusters {
		clusterRows = append(clusterRows, []driver.Value{cluster})
	}
	db.enqueue(clusterColumn, clusterRows...).
		enqueue(countColumn, []driver.Value{f.nodes}).
		enqueue(openImagesColumns, []driver.Value{f.openImages, f.openBytes}).
		enqueue(closedColumns, []driver.Value{f.closed, f.median, f.p95}).
		enqueue(gcOverviewColumns, []driver.Value{f.gcRuns, f.gcImages, f.gcBytes}).
		enqueue(repullOverColumns, []driver.Value{f.repulls, f.repullMed, f.repullMax}).
		enqueue(taskOverviewCols, []driver.Value{f.tasks, f.succeeded, f.failed, f.active, f.avgMs}).
		enqueue(timeColumn, []driver.Value{f.lastSample}).
		enqueue(timeColumn, []driver.Value{f.lastTask})
}

func TestQuerierPing(t *testing.T) {
	store, db := openFakeQuerier(t)
	if err := store.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	db.pingErr = errors.New("connection refused")
	err := store.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded with a failing connection")
	}
	if !strings.HasPrefix(err.Error(), "stats: ping:") {
		t.Fatalf("Ping error = %q, want the stats: ping: prefix", err)
	}
}

func TestOverviewAssemblesWindowAggregates(t *testing.T) {
	store, db := openFakeQuerier(t)
	sampleAt := baseTime
	taskAt := baseTime.Add(time.Minute)
	overviewFixture{
		clusters:   []string{"c1", "c2"},
		nodes:      2,
		openImages: 3, openBytes: int64(1024),
		closed: 4, median: float64(120.5), p95: float64(900),
		gcRuns: 5, gcImages: int64(6), gcBytes: int64(7),
		repulls: 8, repullMed: float64(30), repullMax: float64(300),
		tasks: 9, succeeded: int64(5), failed: int64(2), active: int64(2), avgMs: float64(1234.5),
		lastSample: sampleAt, lastTask: taskAt,
	}.enqueue(db)

	window := 6 * time.Hour
	got, err := store.Overview(context.Background(), window)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}

	if got.WindowSeconds != int64(window/time.Second) {
		t.Fatalf("WindowSeconds = %d, want %d", got.WindowSeconds, int64(window/time.Second))
	}
	if got.From.IsZero() || got.To.IsZero() {
		t.Fatalf("window bounds not filled: %+v", got)
	}
	if got.To.Sub(got.From) != window {
		t.Fatalf("window = %v, want %v", got.To.Sub(got.From), window)
	}
	if !reflect.DeepEqual(got.Clusters, []string{"c1", "c2"}) {
		t.Fatalf("Clusters = %#v", got.Clusters)
	}
	if got.Nodes != 2 {
		t.Fatalf("Nodes = %d, want 2", got.Nodes)
	}
	if got.OpenImages != 3 || got.OpenImagesBytes != 1024 {
		t.Fatalf("open images = %d/%d, want 3/1024", got.OpenImages, got.OpenImagesBytes)
	}
	if got.ClosedIntervals != 4 || got.MedianLifetimeSeconds != 120.5 || got.P95LifetimeSeconds != 900 {
		t.Fatalf("closed intervals = %+v", got)
	}
	if got.GCRuns != 5 || got.GCRemovedImages != 6 || got.GCRemovedBytes != 7 {
		t.Fatalf("gc = %+v", got)
	}
	if got.Repulls != 8 || got.RepullMedianLagSeconds != 30 || got.RepullMaxLagSeconds != 300 {
		t.Fatalf("repulls = %+v", got)
	}
	if got.Tasks != 9 || got.TasksSucceeded != 5 || got.TasksFailed != 2 || got.TasksActive != 2 || got.TasksAvgDurationMs != 1234.5 {
		t.Fatalf("tasks = %+v", got)
	}
	if got.LastSampleAt == nil || !got.LastSampleAt.Equal(sampleAt) {
		t.Fatalf("LastSampleAt = %v, want %v", got.LastSampleAt, sampleAt)
	}
	if got.LastTaskAt == nil || !got.LastTaskAt.Equal(taskAt) {
		t.Fatalf("LastTaskAt = %v, want %v", got.LastTaskAt, taskAt)
	}

	if len(db.querySnapshot()) != 9 {
		t.Fatalf("Overview ran %d queries, want 9", len(db.querySnapshot()))
	}
	wantSQL(t, queryAt(t, db, 0), "UNION", "prepuller_image_presence", "prepuller_node_samples", "ORDER BY cluster")
	wantSQL(t, queryAt(t, db, 1), "DISTINCT cluster, node", "prepuller_node_samples")
	wantSQL(t, queryAt(t, db, 2), "removed_at IS NULL", "SUM(size_bytes)")
	wantSQL(t, queryAt(t, db, 3), "percentile_cont(0.5)", "percentile_cont(0.95)", "removed_at >= $1")
	wantWindowArg(t, queryAt(t, db, 3), got.From)
	wantSQL(t, queryAt(t, db, 4), "prepuller_gc_events", "occurred_at >= $1")
	wantWindowArg(t, queryAt(t, db, 4), got.From)
	wantSQL(t, queryAt(t, db, 5), "percentile_cont(0.5)", "MAX(lag_seconds)", "finished_at >= $1")
	wantWindowArg(t, queryAt(t, db, 5), got.From)
	wantSQL(t, queryAt(t, db, 6), "state IN ('queued', 'running')", "AVG(duration_ms)", "observed_at >= $1")
	wantWindowArg(t, queryAt(t, db, 6), got.From)
	wantSQL(t, queryAt(t, db, 7), "MAX(sampled_at)")
	wantSQL(t, queryAt(t, db, 8), "MAX(observed_at)")
}

func TestOverviewNullsBecomeZeroAndNil(t *testing.T) {
	store, db := openFakeQuerier(t)
	overviewFixture{
		clusters: nil,
		// An empty statistics database: SUM/percentile/AVG are NULL without rows,
		// and the last sample/task aggregates have no row value either.
		openBytes: nil, median: nil, p95: nil, gcImages: nil, gcBytes: nil,
		repullMed: nil, repullMax: nil, succeeded: nil, failed: nil, active: nil, avgMs: nil,
		lastSample: nil, lastTask: nil,
	}.enqueue(db)

	got, err := store.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if got.Clusters == nil || len(got.Clusters) != 0 {
		t.Fatalf("Clusters = %#v, want an empty non-nil slice", got.Clusters)
	}
	if got.OpenImagesBytes != 0 || got.MedianLifetimeSeconds != 0 || got.P95LifetimeSeconds != 0 {
		t.Fatalf("nullable aggregates not zeroed: %+v", got)
	}
	if got.GCRemovedImages != 0 || got.GCRemovedBytes != 0 || got.RepullMedianLagSeconds != 0 || got.RepullMaxLagSeconds != 0 {
		t.Fatalf("nullable gc/repull aggregates not zeroed: %+v", got)
	}
	if got.TasksSucceeded != 0 || got.TasksFailed != 0 || got.TasksActive != 0 || got.TasksAvgDurationMs != 0 {
		t.Fatalf("nullable task aggregates not zeroed: %+v", got)
	}
	if got.LastSampleAt != nil || got.LastTaskAt != nil {
		t.Fatalf("last sample/task = %v/%v, want nil/nil", got.LastSampleAt, got.LastTaskAt)
	}
}

func TestOverviewClampsNonPositiveWindow(t *testing.T) {
	for _, window := range []time.Duration{0, -time.Hour} {
		store, db := openFakeQuerier(t)
		overviewFixture{}.enqueue(db)

		got, err := store.Overview(context.Background(), window)
		if err != nil {
			t.Fatalf("Overview(%v): %v", window, err)
		}
		if got.WindowSeconds != int64(defaultQueryWindow/time.Second) {
			t.Fatalf("Overivew(%v).WindowSeconds = %d, want the 24h default %d",
				window, got.WindowSeconds, int64(defaultQueryWindow/time.Second))
		}
		if got.To.Sub(got.From) != defaultQueryWindow {
			t.Fatalf("Overview(%v) window = %v, want %v", window, got.To.Sub(got.From), defaultQueryWindow)
		}
		wantWindowArg(t, queryAt(t, db, 3), got.From)
	}
}

func TestNodesReturnsLatestSamplePerNode(t *testing.T) {
	store, db := openFakeQuerier(t)
	later := baseTime.Add(time.Minute)
	db.enqueue(nodeColumns,
		[]driver.Value{"c1", "n1", later, true, true, false, int64(100), int64(200), false, int64(3)},
		[]driver.Value{"c1", "n2", baseTime, false, false, true, int64(0), int64(0), true, int64(0)},
	)

	nodes, err := store.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("Nodes = %d, want 2", len(nodes))
	}
	first := nodes[0]
	if first.Cluster != "c1" || first.Node != "n1" || !first.SampledAt.Equal(later) {
		t.Fatalf("unexpected first node: %+v", first)
	}
	if !first.Ready || !first.AgentUp || first.DiskPaused {
		t.Fatalf("unexpected first node flags: %+v", first)
	}
	if first.ImageFSUsedBytes != 100 || first.ImageFSCapacityBytes != 200 || first.ImageFSCapacityEstimated || first.CachedImageCount != 3 {
		t.Fatalf("unexpected first node inventory: %+v", first)
	}
	if nodes[1].Node != "n2" || nodes[1].Ready || nodes[1].AgentUp || !nodes[1].DiskPaused || !nodes[1].ImageFSCapacityEstimated {
		t.Fatalf("unexpected second node: %+v", nodes[1])
	}

	query := queryAt(t, db, 0)
	wantSQL(t, query, "DISTINCT ON (cluster, node)", "ORDER BY cluster, node, sampled_at DESC")
	wantArgs(t, query)
}

func TestNodesEmptyResultIsEmptySlice(t *testing.T) {
	store, db := openFakeQuerier(t)
	db.enqueue(nodeColumns)

	nodes, err := store.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if nodes == nil {
		t.Fatal("Nodes returned nil, want an empty non-nil slice")
	}
	if len(nodes) != 0 {
		t.Fatalf("Nodes = %+v, want empty", nodes)
	}
}

func TestImagesStateFilters(t *testing.T) {
	cases := []struct {
		state    string
		fragment string
		absent   string
	}{
		{state: "open", fragment: "removed_at IS NULL", absent: "removed_at IS NOT NULL"},
		{state: "removed", fragment: "removed_at IS NOT NULL AND removed_at >= $1", absent: "removed_at IS NULL OR"},
		{state: "", fragment: "(removed_at IS NULL OR removed_at >= $1)", absent: ""},
	}
	for _, tc := range cases {
		t.Run("state="+tc.state, func(t *testing.T) {
			store, db := openFakeQuerier(t)
			db.enqueue(countColumn, []driver.Value{int64(2)}).
				enqueue(imageColumns)

			page, err := store.Images(context.Background(), model.StatsImageQuery{State: tc.state})
			if err != nil {
				t.Fatalf("Images: %v", err)
			}
			if page.Total != 2 {
				t.Fatalf("Total = %d, want 2", page.Total)
			}
			if page.Limit != defaultQueryLimit {
				t.Fatalf("Limit = %d, want the default %d", page.Limit, defaultQueryLimit)
			}
			if page.Rows == nil || len(page.Rows) != 0 {
				t.Fatalf("Rows = %#v, want an empty non-nil slice", page.Rows)
			}

			count := queryAt(t, db, 0)
			pageQuery := queryAt(t, db, 1)
			wantSQL(t, count, "FROM prepuller_image_presence", tc.fragment)
			wantSQL(t, pageQuery, "FROM prepuller_image_presence", tc.fragment)
			if tc.absent != "" {
				for _, exec := range []fakeExec{count, pageQuery} {
					if strings.Contains(exec.query, tc.absent) {
						t.Fatalf("query %q unexpectedly contains %q", exec.query, tc.absent)
					}
				}
			}
			// The open branch has no time predicate, so it must not take the
			// window argument; the other two start their placeholders at $1=from.
			if tc.state == "open" {
				wantArgs(t, count)
				wantArgs(t, pageQuery, int64(defaultQueryLimit), int64(0))
			} else {
				from := wantWindowStart(t, count, defaultQueryWindow)
				wantArgs(t, count, from)
				wantArgs(t, pageQuery, from, int64(defaultQueryLimit), int64(0))
			}
			wantPlaceholdersMatchArgs(t, count)
			wantPlaceholdersMatchArgs(t, pageQuery)
		})
	}
}

// TestImagesOpenStateHasNoWindowArgument is the regression test for a bind
// mismatch: removed_at IS NULL needs no time value, so passing the window anyway
// would leave pgx with an unused $1 and fail the real statement.
func TestImagesOpenStateHasNoWindowArgument(t *testing.T) {
	store, db := openFakeQuerier(t)
	db.enqueue(countColumn, []driver.Value{int64(1)}).
		enqueue(imageColumns)

	if _, err := store.Images(context.Background(), model.StatsImageQuery{
		State:   "open",
		Cluster: "c1",
		Search:  "nginx",
		Limit:   10,
		Offset:  2,
	}); err != nil {
		t.Fatalf("Images: %v", err)
	}

	count := queryAt(t, db, 0)
	pageQuery := queryAt(t, db, 1)
	if strings.Contains(count.query, "removed_at >=") {
		t.Fatalf("open state carries a time predicate: %q", count.query)
	}
	// No time.Time among the arguments: the window is not used by this branch.
	wantArgs(t, count, "c1", "nginx")
	wantArgs(t, pageQuery, "c1", "nginx", int64(10), int64(2))
	wantPlaceholdersMatchArgs(t, count)
	wantPlaceholdersMatchArgs(t, pageQuery)
}

func TestImagesSearchAndClusterUseBindArguments(t *testing.T) {
	store, db := openFakeQuerier(t)
	search := "nginx; DROP TABLE prepuller_tasks"
	removedAt := baseTime
	db.enqueue(countColumn, []driver.Value{int64(42)}).
		enqueue(imageColumns,
			[]driver.Value{"c1", "n1", "nginx:1", baseTime.Add(-time.Hour), baseTime, nil, int64(100), float64(3600)},
			[]driver.Value{"c1", "n2", "redis:7", baseTime.Add(-2 * time.Hour), baseTime.Add(-time.Hour), removedAt, int64(200), float64(3600)},
		)

	page, err := store.Images(context.Background(), model.StatsImageQuery{
		Window:  2 * time.Hour,
		Cluster: "c1",
		Search:  search,
		Limit:   10,
		Offset:  5,
	})
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if page.Total != 42 || page.Limit != 10 || page.Offset != 5 {
		t.Fatalf("page metadata = %+v", page)
	}
	if len(page.Rows) != 2 {
		t.Fatalf("Rows = %d, want 2", len(page.Rows))
	}

	count := queryAt(t, db, 0)
	pageQuery := queryAt(t, db, 1)
	for _, exec := range []fakeExec{count, pageQuery} {
		// The search string must travel as a bind argument, never be spliced in.
		if strings.Contains(exec.query, "DROP") || strings.Contains(exec.query, "nginx") {
			t.Fatalf("user search string interpolated into SQL: %q", exec.query)
		}
		wantSQL(t, exec, "(removed_at IS NULL OR removed_at >= $1)", "cluster = $2", "image ILIKE '%' || $3 || '%'")
	}
	wantSQL(t, pageQuery,
		"ORDER BY COALESCE(removed_at, last_seen_at) DESC, cluster, node, image",
		"LIMIT $4 OFFSET $5",
		"EXTRACT(epoch FROM COALESCE(removed_at, last_seen_at) - first_seen_at)",
	)
	from := wantWindowStart(t, count, 2*time.Hour)
	wantArgs(t, count, from, "c1", search)
	wantArgs(t, pageQuery, from, "c1", search, int64(10), int64(5))
	wantPlaceholdersMatchArgs(t, count)
	wantPlaceholdersMatchArgs(t, pageQuery)

	open := page.Rows[0]
	if open.RemovedAt != nil {
		t.Fatalf("open interval has RemovedAt = %v, want nil", open.RemovedAt)
	}
	if open.Cluster != "c1" || open.Node != "n1" || open.Image != "nginx:1" || open.SizeBytes != 100 || open.LifetimeSeconds != 3600 {
		t.Fatalf("unexpected open row: %+v", open)
	}
	if !open.FirstSeenAt.Equal(baseTime.Add(-time.Hour)) || !open.LastSeenAt.Equal(baseTime) {
		t.Fatalf("unexpected open row timestamps: %+v", open)
	}
	closed := page.Rows[1]
	if closed.RemovedAt == nil || !closed.RemovedAt.Equal(removedAt) {
		t.Fatalf("closed interval RemovedAt = %v, want %v", closed.RemovedAt, removedAt)
	}
}

func TestImagesEmptyPageClampsPaging(t *testing.T) {
	store, db := openFakeQuerier(t)
	db.enqueue(countColumn, []driver.Value{int64(0)}).
		enqueue(imageColumns)

	page, err := store.Images(context.Background(), model.StatsImageQuery{Limit: 0, Offset: -5})
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if page.Limit != defaultQueryLimit || page.Offset != 0 || page.Total != 0 {
		t.Fatalf("clamped page = %+v, want Limit=%d Offset=0 Total=0", page, defaultQueryLimit)
	}
	if page.Rows == nil || len(page.Rows) != 0 {
		t.Fatalf("Rows = %#v, want an empty non-nil slice", page.Rows)
	}
	pageQuery := queryAt(t, db, 1)
	wantSQL(t, pageQuery, "LIMIT $2 OFFSET $3")
	wantArgs(t, pageQuery, wantWindowStart(t, pageQuery, defaultQueryWindow), int64(defaultQueryLimit), int64(0))
	wantPlaceholdersMatchArgs(t, queryAt(t, db, 0))
	wantPlaceholdersMatchArgs(t, pageQuery)
}

func TestGCEventsWindowClusterAndPagination(t *testing.T) {
	store, db := openFakeQuerier(t)
	at := baseTime
	db.enqueue(countColumn, []driver.Value{int64(9)}).
		enqueue(gcEventColumns, []driver.Value{"c2", "n1", at, int64(4), int64(4096), float64(60)})

	page, err := store.GCEvents(context.Background(), model.StatsEventQuery{
		Window:  6 * time.Hour,
		Cluster: "c2",
		Limit:   3,
		Offset:  7,
	})
	if err != nil {
		t.Fatalf("GCEvents: %v", err)
	}
	if page.Total != 9 || page.Limit != 3 || page.Offset != 7 {
		t.Fatalf("page metadata = %+v", page)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("Rows = %d, want 1", len(page.Rows))
	}
	row := page.Rows[0]
	if row.Cluster != "c2" || row.Node != "n1" || !row.OccurredAt.Equal(at) {
		t.Fatalf("unexpected gc row: %+v", row)
	}
	if row.RemovedCount != 4 || row.RemovedBytes != 4096 || row.WindowSeconds != 60 {
		t.Fatalf("unexpected gc row values: %+v", row)
	}

	count := queryAt(t, db, 0)
	pageQuery := queryAt(t, db, 1)
	for _, exec := range []fakeExec{count, pageQuery} {
		wantSQL(t, exec, "FROM prepuller_gc_events", "occurred_at >= $1", "cluster = $2")
	}
	wantSQL(t, pageQuery, "ORDER BY occurred_at DESC, cluster, node", "LIMIT $3 OFFSET $4")
	from := wantWindowStart(t, count, 6*time.Hour)
	wantArgs(t, pageQuery, from, "c2", int64(3), int64(7))
	wantPlaceholdersMatchArgs(t, count)
	wantPlaceholdersMatchArgs(t, pageQuery)
}

func TestGCEventsEmptyResultIsEmptySlice(t *testing.T) {
	store, db := openFakeQuerier(t)
	db.enqueue(countColumn, []driver.Value{int64(0)}).enqueue(gcEventColumns)

	page, err := store.GCEvents(context.Background(), model.StatsEventQuery{})
	if err != nil {
		t.Fatalf("GCEvents: %v", err)
	}
	if page.Rows == nil || len(page.Rows) != 0 {
		t.Fatalf("Rows = %#v, want an empty non-nil slice", page.Rows)
	}
}

func TestRepullsWindowAndPagination(t *testing.T) {
	store, db := openFakeQuerier(t)
	removedAt := baseTime.Add(-time.Hour)
	finishedAt := baseTime
	db.enqueue(countColumn, []driver.Value{int64(4)}).
		enqueue(repullColumns, []driver.Value{"c1", "task-1", "nginx:1", "n1", removedAt, finishedAt, float64(3600)})

	page, err := store.Repulls(context.Background(), model.StatsEventQuery{
		Window:  90 * time.Minute,
		Cluster: "c1",
		Limit:   2,
		Offset:  1,
	})
	if err != nil {
		t.Fatalf("Repulls: %v", err)
	}
	if page.Total != 4 || page.Limit != 2 || page.Offset != 1 {
		t.Fatalf("page metadata = %+v", page)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("Rows = %d, want 1", len(page.Rows))
	}
	row := page.Rows[0]
	if row.Cluster != "c1" || row.TaskID != "task-1" || row.Image != "nginx:1" || row.Node != "n1" {
		t.Fatalf("unexpected repull row: %+v", row)
	}
	if !row.RemovedAt.Equal(removedAt) || !row.FinishedAt.Equal(finishedAt) || row.LagSeconds != 3600 {
		t.Fatalf("unexpected repull row values: %+v", row)
	}

	count := queryAt(t, db, 0)
	pageQuery := queryAt(t, db, 1)
	for _, exec := range []fakeExec{count, pageQuery} {
		wantSQL(t, exec, "FROM prepuller_repulls", "finished_at >= $1", "cluster = $2")
	}
	wantSQL(t, pageQuery, "ORDER BY finished_at DESC, cluster, task_id", "LIMIT $3 OFFSET $4")
	wantArgs(t, pageQuery, count.args[0], "c1", int64(2), int64(1))
	wantPlaceholdersMatchArgs(t, count)
	wantPlaceholdersMatchArgs(t, pageQuery)
}

func TestRepullsEmptyResultIsEmptySlice(t *testing.T) {
	store, db := openFakeQuerier(t)
	db.enqueue(countColumn, []driver.Value{int64(0)}).enqueue(repullColumns)

	page, err := store.Repulls(context.Background(), model.StatsEventQuery{})
	if err != nil {
		t.Fatalf("Repulls: %v", err)
	}
	if page.Rows == nil || len(page.Rows) != 0 {
		t.Fatalf("Rows = %#v, want an empty non-nil slice", page.Rows)
	}
}

func TestTasksActiveStateTriggerAndSearchUseBindArguments(t *testing.T) {
	store, db := openFakeQuerier(t)
	db.enqueue(countColumn, []driver.Value{int64(2)}).
		enqueue(taskColumns)

	page, err := store.Tasks(context.Background(), model.StatsTaskQuery{
		Window:  time.Hour,
		Cluster: "c1",
		State:   "active",
		Trigger: "auto",
		Search:  "nginx:1",
		Limit:   5,
		Offset:  1,
	})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if page.Total != 2 || page.Limit != 5 || page.Offset != 1 {
		t.Fatalf("page metadata = %+v", page)
	}
	if page.Rows == nil || len(page.Rows) != 0 {
		t.Fatalf("Rows = %#v, want an empty non-nil slice", page.Rows)
	}

	count := queryAt(t, db, 0)
	pageQuery := queryAt(t, db, 1)
	for _, exec := range []fakeExec{count, pageQuery} {
		if strings.Contains(exec.query, "auto") || strings.Contains(exec.query, "nginx") {
			t.Fatalf("user filter interpolated into SQL: %q", exec.query)
		}
		wantSQL(t, exec,
			"FROM prepuller_tasks",
			"observed_at >= $1",
			"cluster = $2",
			"state IN ('queued', 'running')",
			`"trigger" = $3`,
			"image ILIKE '%' || $4 || '%'",
		)
	}
	wantSQL(t, pageQuery,
		`"trigger"`,
		"ORDER BY observed_at DESC, cluster, task_id",
		"LIMIT $5 OFFSET $6",
	)
	from := wantWindowStart(t, count, time.Hour)
	wantArgs(t, count, from, "c1", "auto", "nginx:1")
	wantArgs(t, pageQuery, from, "c1", "auto", "nginx:1", int64(5), int64(1))
	wantPlaceholdersMatchArgs(t, count)
	wantPlaceholdersMatchArgs(t, pageQuery)
}

func TestTasksExactStateFilter(t *testing.T) {
	store, db := openFakeQuerier(t)
	db.enqueue(countColumn, []driver.Value{int64(0)}).enqueue(taskColumns)

	if _, err := store.Tasks(context.Background(), model.StatsTaskQuery{State: "failed"}); err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	count := queryAt(t, db, 0)
	wantSQL(t, count, "state = $2")
	if strings.Contains(count.query, "state IN") {
		t.Fatalf("exact state filter used the active-state list: %q", count.query)
	}
	if len(count.args) != 2 || count.args[1] != "failed" {
		t.Fatalf("count args = %#v, want [window failed]", count.args)
	}
	wantPlaceholdersMatchArgs(t, count)
	wantPlaceholdersMatchArgs(t, queryAt(t, db, 1))
}

func TestTasksRowsScanOptionalTimestamps(t *testing.T) {
	store, db := openFakeQuerier(t)
	created := baseTime
	started := baseTime.Add(10 * time.Second)
	finished := baseTime.Add(time.Minute)
	db.enqueue(countColumn, []driver.Value{int64(2)}).
		enqueue(taskColumns,
			[]driver.Value{"c1", "task-1", "nginx:1", "n1", "queued", int64(0), "auto", "operator", "", nil, nil, nil, int64(0), baseTime, baseTime},
			[]driver.Value{"c1", "task-2", "redis:7", "n2", "failed", int64(1), "manual", "operator", "401 unauthorized", created, started, finished, int64(5000), finished, finished},
		)

	page, err := store.Tasks(context.Background(), model.StatsTaskQuery{})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(page.Rows) != 2 {
		t.Fatalf("Rows = %d, want 2", len(page.Rows))
	}

	active := page.Rows[0]
	if active.CreatedAt != nil || active.StartedAt != nil || active.FinishedAt != nil {
		t.Fatalf("active task timestamps = %+v, want nil/nil/nil", active)
	}
	if active.State != "queued" || active.Trigger != "auto" || active.Actor != "operator" || active.DurationMs != 0 {
		t.Fatalf("unexpected active task: %+v", active)
	}

	done := page.Rows[1]
	if done.CreatedAt == nil || !done.CreatedAt.Equal(created) ||
		done.StartedAt == nil || !done.StartedAt.Equal(started) ||
		done.FinishedAt == nil || !done.FinishedAt.Equal(finished) {
		t.Fatalf("finished task timestamps = %+v", done)
	}
	if done.Error != "401 unauthorized" || done.Attempt != 1 || done.DurationMs != 5000 {
		t.Fatalf("unexpected finished task: %+v", done)
	}
	if !done.ObservedAt.Equal(finished) || !done.StateChangedAt.Equal(finished) {
		t.Fatalf("observed/state-changed = %v/%v", done.ObservedAt, done.StateChangedAt)
	}
}

func TestQueryErrorsAreWrapped(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		call   func(context.Context, *postgresStore) error
	}{
		{"overview", "stats: overview:", func(ctx context.Context, store *postgresStore) error {
			_, err := store.Overview(ctx, time.Hour)
			return err
		}},
		{"nodes", "stats: nodes:", func(ctx context.Context, store *postgresStore) error {
			_, err := store.Nodes(ctx)
			return err
		}},
		{"images", "stats: images:", func(ctx context.Context, store *postgresStore) error {
			_, err := store.Images(ctx, model.StatsImageQuery{})
			return err
		}},
		{"gc events", "stats: gc events:", func(ctx context.Context, store *postgresStore) error {
			_, err := store.GCEvents(ctx, model.StatsEventQuery{})
			return err
		}},
		{"repulls", "stats: repulls:", func(ctx context.Context, store *postgresStore) error {
			_, err := store.Repulls(ctx, model.StatsEventQuery{})
			return err
		}},
		{"tasks", "stats: tasks:", func(ctx context.Context, store *postgresStore) error {
			_, err := store.Tasks(ctx, model.StatsTaskQuery{})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := openFakeQuerier(t)
			// No canned result is queued, so the very first query fails.
			err := tc.call(context.Background(), store)
			if err == nil {
				t.Fatal("query succeeded without a canned result")
			}
			if !strings.HasPrefix(err.Error(), tc.prefix) {
				t.Fatalf("error = %q, want the %q prefix", err, tc.prefix)
			}
		})
	}
}

func TestQuerierPassesRequestContext(t *testing.T) {
	store, _ := openFakeQuerier(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.Nodes(ctx)
	if err == nil {
		t.Fatal("Nodes succeeded with a canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled in the chain", err)
	}
	if !strings.HasPrefix(err.Error(), "stats: nodes:") {
		t.Fatalf("error = %q, want the stats: nodes: prefix", err)
	}
}
