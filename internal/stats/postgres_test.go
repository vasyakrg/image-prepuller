package stats

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// fakeDB records every statement executed through the fake driver.
type fakeDB struct {
	mu        sync.Mutex
	execs     []fakeExec
	begins    int
	commits   int
	rollbacks int
	pingErr   error
	failOn    string
}

type fakeExec struct {
	query string
	args  []any
}

func (f *fakeDB) record(query string, args []driver.NamedValue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	f.execs = append(f.execs, fakeExec{query: query, args: values})
	if f.failOn != "" && strings.Contains(query, f.failOn) {
		return errors.New("fake database failure")
	}
	return nil
}

func (f *fakeDB) snapshot() []fakeExec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeExec, len(f.execs))
	copy(out, f.execs)
	return out
}

func (f *fakeDB) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = nil
}

func (f *fakeDB) counts() (begins, commits, rollbacks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begins, f.commits, f.rollbacks
}

// fakeDriver/fakeConn/fakeTx implement just enough of database/sql/driver.
type fakeDriver struct{ db *fakeDB }

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{db: d.db}, nil }

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.db.mu.Lock()
	c.db.begins++
	c.db.mu.Unlock()
	return &fakeTx{db: c.db}, nil
}
func (c *fakeConn) Ping(context.Context) error {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return c.db.pingErr
}
func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.db.record(query, args); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

type fakeTx struct{ db *fakeDB }

func (t *fakeTx) Commit() error {
	t.db.mu.Lock()
	t.db.commits++
	t.db.mu.Unlock()
	return nil
}
func (t *fakeTx) Rollback() error {
	t.db.mu.Lock()
	t.db.rollbacks++
	t.db.mu.Unlock()
	return nil
}
func (t *fakeTx) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := t.db.record(query, args); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

var fakeDriverSeq int64

// openFakeStore registers a fresh fake driver and opens a Store on it.
func openFakeStore(t *testing.T, db *fakeDB) Store {
	t.Helper()
	name := fmt.Sprintf("stats-fake-%d", atomic.AddInt64(&fakeDriverSeq, 1))
	sql.Register(name, &fakeDriver{db: db})
	previous := driverName
	driverName = name
	t.Cleanup(func() { driverName = previous })

	store, err := OpenPostgres(context.Background(), "postgres://fake")
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func findExec(execs []fakeExec, prefix string) *fakeExec {
	for i := range execs {
		if strings.HasPrefix(execs[i].query, prefix) {
			return &execs[i]
		}
	}
	return nil
}

func TestOpenPostgresAppliesSchema(t *testing.T) {
	db := &fakeDB{}
	openFakeStore(t, db)

	execs := db.snapshot()
	if len(execs) != len(schemaStatements) {
		t.Fatalf("schema statements = %d, want %d", len(execs), len(schemaStatements))
	}
	for _, stmt := range execs {
		if !strings.Contains(stmt.query, "CREATE TABLE IF NOT EXISTS") &&
			!strings.Contains(stmt.query, "CREATE INDEX IF NOT EXISTS") &&
			!strings.Contains(stmt.query, "CREATE UNIQUE INDEX IF NOT EXISTS") {
			t.Fatalf("unexpected schema statement: %s", stmt.query)
		}
	}
	presence := findExec(execs, "CREATE TABLE IF NOT EXISTS prepuller_image_presence")
	if presence == nil {
		t.Fatal("missing prepuller_image_presence DDL")
	}
	unique := findExec(execs, "CREATE UNIQUE INDEX IF NOT EXISTS prepuller_image_presence_open_uidx")
	if unique == nil || !strings.Contains(unique.query, "WHERE removed_at IS NULL") {
		t.Fatalf("missing open-interval unique index: %+v", unique)
	}
}

func TestOpenPostgresPingError(t *testing.T) {
	db := &fakeDB{pingErr: errors.New("cannot connect to postgres://secret-dsn: connection refused")}
	name := fmt.Sprintf("stats-fake-%d", atomic.AddInt64(&fakeDriverSeq, 1))
	sql.Register(name, &fakeDriver{db: db})
	previous := driverName
	driverName = name
	t.Cleanup(func() { driverName = previous })

	store, err := OpenPostgres(context.Background(), "postgres://secret-dsn")
	if err == nil {
		t.Fatal("OpenPostgres succeeded with failing ping")
	}
	if store != nil {
		t.Fatal("store returned on ping failure")
	}
	if strings.Contains(err.Error(), "secret-dsn") {
		t.Fatalf("error leaks the DSN: %v", err)
	}
}

func TestApplyWritesBatch(t *testing.T) {
	db := &fakeDB{}
	store := openFakeStore(t, db)
	db.reset()

	at := baseTime
	finished := at.Add(time.Minute)
	batch := Batch{
		Cluster: "c1",
		At:      at,
		Tasks: []TaskUpsert{{
			Task: model.PullTask{
				ID:         "task-1",
				Image:      "nginx:1",
				Node:       "n1",
				State:      model.TaskSucceeded,
				Attempt:    2,
				Trigger:    model.TriggerAuto,
				CreatedAt:  at,
				FinishedAt: &finished,
				DurationMs: 1000,
			},
			Observed: at,
		}},
		NodeSamples: []NodeSample{{
			Node: "n1", SampledAt: at, Ready: true, AgentUp: true,
			ImageFSUsedBytes: 10, ImageFSCapacityBytes: 20, CachedImageCount: 1,
		}},
		PresenceSeen: []PresenceSeen{{
			Node: "n1", Image: "nginx:1", FirstSeenAt: at, LastSeenAt: at, SizeBytes: 10,
		}},
		PresenceRemoved: []PresenceRemoved{{
			Node: "n1", Image: "redis:7", RemovedAt: at, SizeBytes: 20,
		}},
		GCEvents: []GCEvent{{
			Node: "n1", At: at, RemovedCount: 1, RemovedBytes: 20, WindowSeconds: 60,
		}},
		Repulls: []Repull{{
			TaskID: "task-1", Image: "nginx:1", Node: "n1", RemovedAt: at.Add(-time.Minute), FinishedAt: finished, LagSeconds: 120,
		}},
	}
	if err := store.Apply(context.Background(), batch); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	execs := db.snapshot()
	if len(execs) != 6 {
		t.Fatalf("executed %d statements, want 6: %+v", len(execs), execs)
	}
	task := findExec(execs, "INSERT INTO prepuller_tasks")
	if task == nil {
		t.Fatal("missing task upsert")
	}
	if !strings.Contains(task.query, "ON CONFLICT (cluster, task_id) DO UPDATE") {
		t.Fatalf("task upsert is not idempotent: %s", task.query)
	}
	if !strings.Contains(task.query, "state_changed_at") {
		t.Fatalf("task upsert does not capture state transitions: %s", task.query)
	}
	if len(task.args) != 15 || task.args[0] != "c1" || task.args[1] != "task-1" {
		t.Fatalf("unexpected task args: %+v", task.args)
	}

	sample := findExec(execs, "INSERT INTO prepuller_node_samples")
	if sample == nil || !strings.Contains(sample.query, "ON CONFLICT (cluster, node, sampled_at) DO UPDATE") {
		t.Fatalf("missing idempotent node sample upsert: %+v", sample)
	}

	seen := findExec(execs, "INSERT INTO prepuller_image_presence")
	if seen == nil || !strings.Contains(seen.query, "ON CONFLICT (cluster, node, image) WHERE removed_at IS NULL") {
		t.Fatalf("missing presence upsert: %+v", seen)
	}

	closed := findExec(execs, "UPDATE prepuller_image_presence")
	if closed == nil || !strings.Contains(closed.query, "removed_at IS NULL") {
		t.Fatalf("missing presence close: %+v", closed)
	}

	gc := findExec(execs, "INSERT INTO prepuller_gc_events")
	if gc == nil || !strings.Contains(gc.query, "ON CONFLICT (cluster, node, occurred_at) DO NOTHING") {
		t.Fatalf("missing gc event upsert: %+v", gc)
	}

	repull := findExec(execs, "INSERT INTO prepuller_repulls")
	if repull == nil || !strings.Contains(repull.query, "ON CONFLICT (cluster, task_id) DO NOTHING") {
		t.Fatalf("missing repull upsert: %+v", repull)
	}

	begins, commits, _ := db.counts()
	if begins != 1 || commits != 1 {
		t.Fatalf("begins=%d commits=%d, want 1/1", begins, commits)
	}
}

func TestApplyRollsBackOnError(t *testing.T) {
	db := &fakeDB{failOn: "INSERT INTO prepuller_gc_events"}
	store := openFakeStore(t, db)
	db.reset()

	err := store.Apply(context.Background(), Batch{
		Cluster:  "c1",
		GCEvents: []GCEvent{{Node: "n1", At: baseTime, RemovedCount: 1}},
	})
	if err == nil {
		t.Fatal("Apply succeeded despite failing statement")
	}
	_, commits, rollbacks := db.counts()
	if commits != 0 {
		t.Fatalf("commits = %d, want 0", commits)
	}
	if rollbacks == 0 {
		t.Fatal("transaction was not rolled back")
	}
}

func TestApplyEmptyBatchStillCommits(t *testing.T) {
	db := &fakeDB{}
	store := openFakeStore(t, db)
	db.reset()
	if err := store.Apply(context.Background(), Batch{Cluster: "c1"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(db.snapshot()) != 0 {
		t.Fatalf("empty batch executed statements: %+v", db.snapshot())
	}
	begins, commits, _ := db.counts()
	if begins != 1 || commits != 1 {
		t.Fatalf("begins=%d commits=%d, want 1/1", begins, commits)
	}
}

func TestPruneUsesRetentionHorizon(t *testing.T) {
	db := &fakeDB{}
	store := openFakeStore(t, db)
	db.reset()

	horizon := baseTime.Add(-24 * time.Hour)
	if err := store.Prune(context.Background(), horizon); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	execs := db.snapshot()
	if len(execs) != len(pruneStatements) {
		t.Fatalf("prune statements = %d, want %d", len(execs), len(pruneStatements))
	}
	for _, stmt := range execs {
		if !strings.HasPrefix(stmt.query, "DELETE FROM") {
			t.Fatalf("unexpected prune statement: %s", stmt.query)
		}
		if len(stmt.args) != 1 || stmt.args[0] != horizon {
			t.Fatalf("prune arg = %+v, want %v", stmt.args, horizon)
		}
	}
	presence := findExec(execs, "DELETE FROM prepuller_image_presence")
	if presence == nil || !strings.Contains(presence.query, "removed_at IS NOT NULL") {
		t.Fatalf("open presence intervals must not be pruned: %+v", presence)
	}
}
