// Package reconcile implements the automatic image pre-pull policy for one
// cluster. It is a pure policy layer: it reads a model.Snapshot, decides which
// image+node slots should be warmed, and asks the Provider v2 interface to
// trigger those pulls. It performs no direct I/O and depends only on the
// standard library plus internal/model and internal/provider, so the whole
// policy is unit-testable with an in-memory fake provider.
//
// Rules (see docs/PLAN.md WS-C):
//
//   - offline nodes are skipped (NotReady, agent down, node absent from the snapshot);
//   - disk-paused nodes are skipped (NodeInfo.DiskPaused or ImageFS used/capacity
//     at or above the threshold);
//   - slots that are already present or pulling are skipped, and so are pairs
//     with a queued/running task in Snapshot.Tasks;
//   - failed slots are retried before missing ones; prod outranks staging, which
//     outranks infra, for equal slot state;
//   - images with policy violations (:latest, no immutable tag, Always) are
//     skipped unless Config.AllowPolicyViolations is set;
//   - each image+node pair is planned at most once per run;
//   - in-flight pulls count against MaxConcurrentPerNode/MaxConcurrentTotal,
//     including tasks that were already active before the run;
//   - failed attempts (or observed failed retries) back off exponentially per
//     image+node pair, and the backoff is reset once the image is observed as
//     present;
//   - every request carries Trigger=model.TriggerAuto and the configured actor
//     so automatic actions are visible in Snapshot.Activity.
package reconcile

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

// TriggerFailure is one TriggerPull call that the provider rejected. It is kept
// in Result for logging/metrics; the reconciler itself keeps going.
type TriggerFailure struct {
	Image string
	Node  string
	Error string
}

// Result describes what one reconciliation run did. The skip counters are
// grouped by reason and are intended to feed metrics (WS-H) and logs; they are
// zero-valued when nothing was skipped.
type Result struct {
	Cluster        string
	StartedAt      time.Time
	FinishedAt     time.Time
	Duration       time.Duration
	SnapshotStatus model.SyncStatus
	// SyncError is set when the pre-snapshot SyncNow call failed. The run still
	// continues with whatever the provider can report.
	SyncError string
	// Examined is the number of slots looked at, duplicate pairs included.
	Examined int
	// Planned is the deterministic list of pull requests for this run.
	Planned []model.PullRequest
	// Triggered is the subset of Planned that the provider accepted.
	Triggered []model.PullTask
	// Skipped counts skipped slots by reason.
	Skipped map[SkipReason]int
	// Failures lists TriggerPull calls that returned an error.
	Failures []TriggerFailure
}

// SkippedTotal returns the sum of all skip counters.
func (r Result) SkippedTotal() int {
	total := 0
	for _, n := range r.Skipped {
		total += n
	}
	return total
}

// Reconciler computes and executes the automatic pre-pull plan. It is safe for
// concurrent use, though Start calls it sequentially by design.
type Reconciler struct {
	cfg     Config
	backoff *backoffState
	mu      sync.Mutex
}

// New builds a Reconciler. The zero Config is valid; defaults are documented on
// Config.
func New(cfg Config) *Reconciler {
	return &Reconciler{
		cfg:     cfg.withDefaults(),
		backoff: newBackoffState(),
	}
}

// Config returns the effective configuration (with defaults applied).
func (r *Reconciler) Config() Config {
	return r.cfg
}

// RunOnce performs one reconciliation pass and triggers at most the pulls the
// concurrency limits allow. A provider failure on a single pull is recorded in
// Result.Failures (and backs off that pair); it does not abort the run. The
// returned error is non-nil only when ctx is canceled.
func (r *Reconciler) RunOnce(ctx context.Context, p provider.Provider, cluster string) (Result, error) {
	if p == nil {
		return Result{}, errors.New("reconcile: nil provider")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.cfg.Now()
	res := Result{Cluster: cluster, StartedAt: now, Skipped: make(map[SkipReason]int)}
	if err := ctx.Err(); err != nil {
		r.finish(&res)
		return res, err
	}

	if !r.cfg.DisableSyncBeforeSnapshot {
		if err := p.SyncNow(ctx, r.cfg.Actor); err != nil {
			res.SyncError = err.Error()
			if err := ctx.Err(); err != nil {
				r.finish(&res)
				return res, err
			}
		}
	}

	snap := p.Snapshot(ctx)
	if res.Cluster == "" {
		res.Cluster = snap.ClusterName
	}
	res.SnapshotStatus = snap.SyncStatus
	if snap.SyncStatus == model.SyncOffline || len(snap.Nodes) == 0 {
		// The provider cannot see the cluster (see AGENTS §8: an unreachable
		// spoke yields SyncStatus=offline and TotalWorkers=0). Planning from
		// such a snapshot would only produce noise, so the whole run is skipped.
		res.Skipped[SkipClusterOffline]++
		r.finish(&res)
		return res, nil
	}

	r.backoff.prune(now, r.cfg.BackoffMax)
	r.observeSuccess(snap)
	r.observeFailures(snap, now)

	sel := selectCandidates(snap, r.cfg)
	res.Examined = sel.examined
	for reason, count := range sel.skipped {
		res.Skipped[reason] += count
	}

	ready := make([]candidate, 0, len(sel.candidates))
	for _, c := range sel.candidates {
		if r.backoff.blocked(c.key, now) {
			res.Skipped[SkipBackoff]++
			continue
		}
		ready = append(ready, c)
	}

	remainingTotal := r.cfg.MaxConcurrentTotal - sel.busyTotal
	remainingPerNode := make(map[string]int, len(sel.busyPerNode))
	for node, busy := range sel.busyPerNode {
		remainingPerNode[node] = r.cfg.MaxConcurrentPerNode - busy
	}

	plan := make([]model.PullRequest, 0, len(ready))
	for _, c := range ready {
		if remainingTotal <= 0 || remainingPerNode[c.request.Node] <= 0 {
			res.Skipped[SkipConcurrencyLimit]++
			continue
		}
		remainingTotal--
		remainingPerNode[c.request.Node]--
		req := c.request
		req.Cluster = res.Cluster
		plan = append(plan, req)
	}
	res.Planned = plan

	for _, req := range plan {
		if err := ctx.Err(); err != nil {
			r.finish(&res)
			return res, err
		}
		k := key(req.Image, req.Node)
		task, err := p.TriggerPull(ctx, req)
		if err != nil {
			r.backoff.recordFailure(k, r.cfg.Now(), r.cfg, err.Error())
			res.Failures = append(res.Failures, TriggerFailure{
				Image: req.Image,
				Node:  req.Node,
				Error: err.Error(),
			})
			continue
		}
		r.backoff.recordAttempt(k, r.cfg.Now())
		res.Triggered = append(res.Triggered, task)
	}

	r.finish(&res)
	return res, nil
}

// Start runs the reconciler immediately and then once per Config.Interval until
// ctx is canceled. It blocks, so callers usually run it in a goroutine. onResult
// is called after every run and may be nil; it can be used for logging and
// metrics without making the policy loop depend on them.
//
// Start is intentionally not wired into cmd/prepuller/main.go by WS-C: the
// integration owner decides when automatic warming is enabled.
func (r *Reconciler) Start(ctx context.Context, p provider.Provider, cluster string, onResult func(Result)) {
	run := func() bool {
		res, err := r.RunOnce(ctx, p, cluster)
		if err != nil {
			// Only a canceled context returns an error; any other failure is
			// reflected in Result.
			return false
		}
		if onResult != nil {
			onResult(res)
		}
		return true
	}

	if !run() {
		return
	}

	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !run() {
				return
			}
		}
	}
}

// observeSuccess clears backoff for pairs that are already warm: a present slot
// or a succeeded pull task means the previous failures are history.
func (r *Reconciler) observeSuccess(snap model.Snapshot) {
	for _, workload := range snap.Workloads {
		for _, image := range workload.Images {
			for _, slot := range image.Nodes {
				if slot.State == model.StatePresent {
					r.backoff.reset(key(image.Image, slot.Node))
				}
			}
		}
	}
	for _, task := range snap.Tasks {
		if task.State == model.TaskSucceeded {
			r.backoff.reset(key(task.Image, task.Node))
		}
	}
}

// observeFailures escalates the backoff when a slot that this process already
// attempted is still failed in a newer observation. Without this, an accepted
// pull that fails asynchronously (registry 401, CRI error) would be retried on
// every run - the retry storm the policy must avoid.
//
// A failed slot that was never attempted by this process has no entry and is
// retried immediately, which is what the "failed before missing" priority is
// for.
func (r *Reconciler) observeFailures(snap model.Snapshot, now time.Time) {
	if len(r.backoff.entries) == 0 {
		return
	}
	for _, workload := range snap.Workloads {
		for _, image := range workload.Images {
			for _, slot := range image.Nodes {
				if slot.State != model.StateFailed {
					continue
				}
				k := key(image.Image, slot.Node)
				e := r.backoff.entries[k]
				if e == nil || e.lastAttempt.IsZero() || !e.next.IsZero() {
					continue
				}
				if slot.LastChecked.Before(e.lastAttempt) {
					// The failed state predates our attempt, so it is a stale
					// observation, not a fresh failure.
					continue
				}
				r.backoff.recordFailure(k, now, r.cfg, slot.Error)
			}
		}
	}
}

// finish fills in the timing fields of a result.
func (r *Reconciler) finish(res *Result) {
	res.FinishedAt = r.cfg.Now()
	res.Duration = res.FinishedAt.Sub(res.StartedAt)
	if res.Skipped == nil {
		res.Skipped = make(map[SkipReason]int)
	}
}
