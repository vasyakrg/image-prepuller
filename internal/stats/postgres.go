package stats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// driverName is the database/sql driver used by OpenPostgres. Tests replace it
// with a fake driver; production always uses pgx.
var driverName = "pgx"

// OpenPostgres opens a pgx-backed Store and applies the idempotent schema.
// The DSN is never logged or wrapped into an error.
func OpenPostgres(ctx context.Context, dsn string) (Store, error) {
	if dsn == "" {
		return nil, errors.New("stats: empty postgres dsn")
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("stats: open postgres: %s", redactDSN(err.Error(), dsn))
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	store := &postgresStore{db: db}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("stats: ping postgres: %s", redactDSN(err.Error(), dsn))
	}
	if err := store.ensureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// redactDSN removes the connection string from a driver error before it reaches
// a log line. Errors are the only place a DSN could leak.
func redactDSN(message, dsn string) string {
	if dsn == "" {
		return message
	}
	return strings.ReplaceAll(message, dsn, "<redacted>")
}

// postgresStore is the SQL implementation of Store.
type postgresStore struct {
	db *sql.DB

	mu          sync.Mutex
	schemaReady bool
}

func (s *postgresStore) ensureSchema(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaReady {
		return nil
	}
	for _, stmt := range schemaStatements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("stats: apply schema: %w", err)
		}
	}
	s.schemaReady = true
	return nil
}

// Apply writes one batch atomically. The schema is applied lazily as well, so a
// store created by a custom opener still works.
func (s *postgresStore) Apply(ctx context.Context, b Batch) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("stats: begin batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, upsert := range b.Tasks {
		if upsert.Task.ID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, upsertTaskSQL, taskArgs(b.Cluster, upsert)...); err != nil {
			return fmt.Errorf("stats: upsert task %q: %w", upsert.Task.ID, err)
		}
	}
	for _, sample := range b.NodeSamples {
		if sample.Node == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, upsertNodeSampleSQL, nodeSampleArgs(b.Cluster, sample)...); err != nil {
			return fmt.Errorf("stats: upsert node sample %q: %w", sample.Node, err)
		}
	}
	for _, seen := range b.PresenceSeen {
		if seen.Node == "" || seen.Image == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, upsertPresenceSeenSQL, b.Cluster, seen.Node, seen.Image, seen.FirstSeenAt, seen.LastSeenAt, seen.SizeBytes); err != nil {
			return fmt.Errorf("stats: open presence %q on %q: %w", seen.Image, seen.Node, err)
		}
	}
	for _, removed := range b.PresenceRemoved {
		if removed.Node == "" || removed.Image == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, closePresenceSQL, b.Cluster, removed.Node, removed.Image, removed.RemovedAt); err != nil {
			return fmt.Errorf("stats: close presence %q on %q: %w", removed.Image, removed.Node, err)
		}
	}
	for _, event := range b.GCEvents {
		if event.Node == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, insertGCEventSQL, b.Cluster, event.Node, event.At, event.RemovedCount, event.RemovedBytes, event.WindowSeconds); err != nil {
			return fmt.Errorf("stats: insert gc event %q: %w", event.Node, err)
		}
	}
	for _, repull := range b.Repulls {
		if repull.TaskID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, insertRepullSQL, b.Cluster, repull.TaskID, repull.Node, repull.Image, repull.RemovedAt, repull.FinishedAt, repull.LagSeconds); err != nil {
			return fmt.Errorf("stats: insert repull %q: %w", repull.TaskID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("stats: commit batch: %w", err)
	}
	return nil
}

// Prune deletes rows older than the horizon. Open presence intervals are kept.
func (s *postgresStore) Prune(ctx context.Context, horizon time.Time) error {
	if err := s.ensureSchema(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("stats: begin prune: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range pruneStatements {
		if _, err := tx.ExecContext(ctx, stmt, horizon); err != nil {
			return fmt.Errorf("stats: prune: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("stats: commit prune: %w", err)
	}
	return nil
}

// Close releases the database pool.
func (s *postgresStore) Close() error {
	return s.db.Close()
}

func taskArgs(cluster string, upsert TaskUpsert) []any {
	task := upsert.Task
	observed := upsert.Observed
	if observed.IsZero() {
		observed = upsert.finishedAtOr(task.CreatedAt)
	}
	if observed.IsZero() {
		observed = time.Now()
	}
	var started, finished any
	if task.StartedAt != nil && !task.StartedAt.IsZero() {
		started = *task.StartedAt
	}
	if task.FinishedAt != nil && !task.FinishedAt.IsZero() {
		finished = *task.FinishedAt
	}
	var created any
	if !task.CreatedAt.IsZero() {
		created = task.CreatedAt
	}
	return []any{
		cluster,
		task.ID,
		task.Image,
		task.Node,
		string(task.State),
		task.Attempt,
		string(task.Trigger),
		task.Actor,
		task.Error,
		created,
		started,
		finished,
		task.DurationMs,
		observed,
		observed,
	}
}

func nodeSampleArgs(cluster string, sample NodeSample) []any {
	return []any{
		cluster,
		sample.Node,
		sample.SampledAt,
		sample.Ready,
		sample.AgentUp,
		sample.DiskPaused,
		sample.ImageFSUsedBytes,
		sample.ImageFSCapacityBytes,
		sample.ImageFSCapacityEstimated,
		sample.CachedImageCount,
	}
}

// finishedAtOr returns the finished time when set, otherwise the fallback.
func (t TaskUpsert) finishedAtOr(fallback time.Time) time.Time {
	if t.Task.FinishedAt != nil && !t.Task.FinishedAt.IsZero() {
		return *t.Task.FinishedAt
	}
	return fallback
}
