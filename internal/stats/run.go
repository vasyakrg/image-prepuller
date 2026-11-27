package stats

import (
	"context"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// Run starts the best-effort statistics loop. It snapshots every cluster once
// per Interval, applies the resulting Batch and commits it only on success. A
// failed Apply is logged and retried on the next tick; the hub never blocks on
// the database. Prune runs every PruneInterval with the Retention horizon.
//
// Run owns the store and closes it when ctx is done. A nil store disables the
// loop. Run returns when ctx is canceled.
func Run(ctx context.Context, cfg Config, clusters []Cluster, store Store, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if store == nil {
		logf("stats: disabled: no store")
		return
	}
	r := &runner{
		cfg:       cfg.withDefaults(),
		clusters:  clusters,
		store:     store,
		logf:      logf,
		collector: NewCollector(),
	}
	r.run(ctx)
}

type runner struct {
	cfg       Config
	clusters  []Cluster
	store     Store
	logf      func(string, ...any)
	collector *Collector
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.SnapshotTimeout <= 0 {
		c.SnapshotTimeout = DefaultSnapshotTimeout
	}
	if c.PruneInterval <= 0 {
		c.PruneInterval = c.Interval
	}
	return c
}

func (r *runner) run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	// A zero/negative Retention disables pruning: never interpret a missing
	// retention as "delete everything older than now".
	var pruneC <-chan time.Time
	if r.cfg.Retention > 0 {
		pruneTicker := time.NewTicker(r.cfg.PruneInterval)
		defer pruneTicker.Stop()
		pruneC = pruneTicker.C
	}

	// Collect once right away so the first data point does not wait a full interval.
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			if err := r.store.Close(); err != nil {
				r.logf("stats: close store: %v", err)
			}
			return
		case <-ticker.C:
			r.tick(ctx)
		case <-pruneC:
			r.prune(ctx)
		}
	}
}

// tick snapshots every cluster and applies/commits its batch. Errors are logged
// and never returned: the next tick retries the same diff.
func (r *runner) tick(ctx context.Context) {
	for _, cluster := range r.clusters {
		if ctx.Err() != nil {
			return
		}
		if cluster.ID == "" || cluster.Snapshot == nil {
			continue
		}
		snap, ok := r.snapshot(ctx, cluster)
		if !ok {
			continue
		}
		batch := r.collector.Step(cluster.ID, snap, time.Now())
		if err := r.store.Apply(ctx, batch); err != nil {
			r.logf("stats: apply %s: %v (retrying next tick)", cluster.ID, err)
			continue
		}
		r.collector.Commit(cluster.ID, batch)
	}
}

// snapshot calls one Cluster.Snapshot with a timeout and converts a panic into a
// logged skip so a broken provider cannot take the hub down. The call runs in a
// goroutine so a provider that ignores its context cannot block the stats loop.
func (r *runner) snapshot(ctx context.Context, cluster Cluster) (model.Snapshot, bool) {
	snapCtx, cancel := context.WithTimeout(ctx, r.cfg.SnapshotTimeout)
	defer cancel()

	type result struct {
		snap  model.Snapshot
		panic any
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- result{panic: recovered}
			}
		}()
		done <- result{snap: cluster.Snapshot(snapCtx)}
	}()

	select {
	case <-snapCtx.Done():
		r.logf("stats: snapshot %s: %v", cluster.ID, snapCtx.Err())
		return model.Snapshot{}, false
	case res := <-done:
		if res.panic != nil {
			r.logf("stats: snapshot %s panicked: %v", cluster.ID, res.panic)
			return model.Snapshot{}, false
		}
		return res.snap, true
	}
}

func (r *runner) prune(ctx context.Context) {
	if r.cfg.Retention <= 0 {
		return
	}
	if ctx.Err() != nil {
		return
	}
	horizon := time.Now().Add(-r.cfg.Retention)
	if err := r.store.Prune(ctx, horizon); err != nil {
		r.logf("stats: prune: %v", err)
	}
}
