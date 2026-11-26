package reconcile

import (
	"sort"
	"strings"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// SkipReason explains why a slot was not scheduled for an automatic pull. The
// values are stable and are meant to be used as Prometheus label values by the
// metrics workstream (WS-H), so keep them short and snake_case.
type SkipReason string

const (
	// SkipClusterOffline means the whole run was abandoned: the provider
	// reports the cluster as offline (remote spoke unreachable, etc).
	SkipClusterOffline SkipReason = "cluster_offline"
	// SkipOfflineNode covers NotReady nodes, nodes with a down agent and slots
	// that point at a node missing from Snapshot.Nodes.
	SkipOfflineNode SkipReason = "offline_node"
	// SkipDiskPaused covers both NodeInfo.DiskPaused and ImageFS usage at or
	// above the pause threshold.
	SkipDiskPaused SkipReason = "disk_paused"
	// SkipPresent means the image is already in the node image cache.
	SkipPresent SkipReason = "present"
	// SkipPulling means the slot is already in the pulling state.
	SkipPulling SkipReason = "pulling"
	// SkipActiveTask means Snapshot.Tasks already has a queued or running task
	// for this image+node pair.
	SkipActiveTask SkipReason = "active_task"
	// SkipBackoff means a previous attempt for this image+node pair failed and
	// the exponential backoff window is still open.
	SkipBackoff SkipReason = "backoff"
	// SkipPolicyViolation means the image is unsafe for automatic pre-pulling
	// (:latest, no immutable tag, imagePullPolicy: Always) and the operator has
	// not explicitly allowed it via Config.AllowPolicyViolations.
	SkipPolicyViolation SkipReason = "policy_violation"
	// SkipConcurrencyLimit means the slot was eligible but the per-node or
	// global in-flight pull limit was already reached. It stays a candidate
	// for the next run.
	SkipConcurrencyLimit SkipReason = "concurrency_limit"
	// SkipDuplicate means the same image+node pair was already examined through
	// another workload in this run (regardless of the first occurrence's
	// outcome); at most one request is planned per pair per run.
	SkipDuplicate SkipReason = "duplicate"
)

// Config controls the automatic pre-pull policy. The zero value is safe: New
// fills in the defaults documented on each field.
type Config struct {
	// Interval is the period between reconciliation runs started by Start.
	// Defaults to 30s.
	Interval time.Duration
	// MaxConcurrentPerNode caps pull tasks in flight per node, including tasks
	// that were already active when the run started. Defaults to 2.
	MaxConcurrentPerNode int
	// MaxConcurrentTotal caps pull tasks in flight across the cluster,
	// including already active tasks. Defaults to 4.
	MaxConcurrentTotal int
	// DiskPauseThreshold is the fallback ImageFS used/capacity ratio at which
	// automatic pre-pulls are paused; NodeInfo.DiskPauseThreshold wins when it
	// is positive. Defaults to 0.78 (below the kubelet GC threshold of 0.85 on
	// purpose: stop warming before kubelet starts evicting images).
	DiskPauseThreshold float64
	// AllowPolicyViolations makes the reconciler pull images that carry policy
	// warnings (:latest / no immutable tag / imagePullPolicy: Always). The zero
	// value (false) is the safe default: such images are skipped because a
	// floating tag cannot be verified and Always defeats offline failover.
	AllowPolicyViolations bool
	// BackoffBase is the delay after the first failed attempt. Defaults to 30s.
	BackoffBase time.Duration
	// BackoffMax caps the exponential backoff delay. Defaults to 15m.
	BackoffMax time.Duration
	// BackoffFactor multiplies the delay after every consecutive failure.
	// Values below 1 are treated as 1 (constant delay); defaults to 2.
	BackoffFactor float64
	// DisableSyncBeforeSnapshot skips the provider SyncNow call before taking
	// the snapshot. By default the reconciler asks the provider to refresh
	// (SyncNow) so the plan is built from fresh data, not from a stale TTL
	// cache.
	DisableSyncBeforeSnapshot bool
	// Actor is recorded on every TriggerPull request and SyncNow call so that
	// automatic actions are distinguishable from operator actions in
	// Snapshot.Activity. Defaults to "reconciler".
	Actor string
	// NamespacePriority is the tie-break order used between slots of equal
	// state (failed before missing, then prod before staging before infra).
	// Defaults to []string{"prod", "staging", "infra"}. Namespaces that are not
	// listed rank after all listed ones and are ordered alphabetically.
	NamespacePriority []string
	// Now is the clock; tests inject a fake one. Defaults to time.Now.
	Now func() time.Time
}

// withDefaults returns a copy of the config with documented defaults filled in.
func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.MaxConcurrentPerNode <= 0 {
		c.MaxConcurrentPerNode = 2
	}
	if c.MaxConcurrentTotal <= 0 {
		c.MaxConcurrentTotal = 4
	}
	if c.DiskPauseThreshold <= 0 {
		c.DiskPauseThreshold = 0.78
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 30 * time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 15 * time.Minute
	}
	if c.BackoffMax < c.BackoffBase {
		c.BackoffMax = c.BackoffBase
	}
	if c.BackoffFactor < 1 {
		c.BackoffFactor = 2
	}
	if c.Actor == "" {
		c.Actor = "reconciler"
	}
	if len(c.NamespacePriority) == 0 {
		c.NamespacePriority = []string{"prod", "staging", "infra"}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// backoffDelay returns the delay before the next attempt after n consecutive
// failures: base * factor^(n-1), capped at BackoffMax.
func (c Config) backoffDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := float64(c.BackoffBase)
	for i := 1; i < failures; i++ {
		delay *= c.BackoffFactor
		if delay >= float64(c.BackoffMax) {
			return c.BackoffMax
		}
	}
	if delay > float64(c.BackoffMax) {
		return c.BackoffMax
	}
	return time.Duration(delay)
}

// candidate is one eligible image+node pair, before the backoff and
// concurrency filters are applied.
type candidate struct {
	request       model.PullRequest
	priority      int // 0 = failed slot (retry first), 1 = missing slot
	namespaceRank int
	key           string
}

// selection is the pure (provider-independent) part of a reconciliation run:
// slots examined, candidates that passed the per-slot rules and skip counters.
type selection struct {
	candidates  []candidate
	skipped     map[SkipReason]int
	examined    int
	busyPerNode map[string]int
	busyTotal   int
}

// key identifies one image+node pair; it is the backoff and deduplication key.
func key(image, node string) string {
	return image + "|" + node
}

// selectCandidates applies the per-slot policy to a snapshot. It never mutates
// the snapshot and performs no I/O; provider calls and backoff state are the
// caller's responsibility.
//
// The order of the returned candidates is deterministic: failed slots before
// missing ones, then the configured namespace priority, then image and node
// name. That makes planning and tests reproducible.
func selectCandidates(snap model.Snapshot, cfg Config) selection {
	sel := selection{
		skipped:     make(map[SkipReason]int),
		busyPerNode: make(map[string]int),
	}

	nodes := make(map[string]model.NodeInfo, len(snap.Nodes))
	for _, n := range snap.Nodes {
		nodes[n.Name] = n
	}

	sel.busyPerNode, sel.busyTotal = busySlots(snap, nodes)

	activeTasks := make(map[string]struct{}, len(snap.Tasks))
	for _, task := range snap.Tasks {
		if task.State.Active() {
			activeTasks[key(task.Image, task.Node)] = struct{}{}
		}
	}

	seen := make(map[string]struct{})
	for _, workload := range snap.Workloads {
		for _, image := range workload.Images {
			for _, slot := range image.Nodes {
				sel.examined++
				slotKey := key(image.Image, slot.Node)
				if _, ok := seen[slotKey]; ok {
					sel.skipped[SkipDuplicate]++
					continue
				}
				seen[slotKey] = struct{}{}

				node, ok := nodes[slot.Node]
				if !ok || !node.Ready || !node.AgentUp {
					sel.skipped[SkipOfflineNode]++
					continue
				}
				if diskPaused(node, cfg) {
					sel.skipped[SkipDiskPaused]++
					continue
				}

				switch slot.State {
				case model.StatePresent:
					sel.skipped[SkipPresent]++
					continue
				case model.StatePulling:
					sel.skipped[SkipPulling]++
					continue
				case model.StateOffline:
					sel.skipped[SkipOfflineNode]++
					continue
				case model.StatePausedDisk:
					sel.skipped[SkipDiskPaused]++
					continue
				}

				if _, ok := activeTasks[slotKey]; ok {
					sel.skipped[SkipActiveTask]++
					continue
				}
				if len(image.PolicyViolations) > 0 && !cfg.AllowPolicyViolations {
					sel.skipped[SkipPolicyViolation]++
					continue
				}

				priority := 1
				if slot.State == model.StateFailed {
					priority = 0
				}
				sel.candidates = append(sel.candidates, candidate{
					request: model.PullRequest{
						Image:   image.Image,
						Node:    slot.Node,
						Actor:   cfg.Actor,
						Trigger: model.TriggerAuto,
						Policy:  image.PullPolicy,
					},
					priority:      priority,
					namespaceRank: namespaceRank(workload.Namespace, cfg.NamespacePriority),
					key:           slotKey,
				})
			}
		}
	}

	sort.Slice(sel.candidates, func(i, j int) bool {
		a, b := sel.candidates[i], sel.candidates[j]
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		if a.namespaceRank != b.namespaceRank {
			return a.namespaceRank < b.namespaceRank
		}
		if a.request.Image != b.request.Image {
			return a.request.Image < b.request.Image
		}
		return a.request.Node < b.request.Node
	})
	return sel
}

// busySlots counts in-flight pulls per node and in total. Snapshot.Tasks is the
// contract source of truth; NodeInfo.ActivePulls is used as a floor because
// providers trim the task list (mock keeps the last 200 tasks only).
func busySlots(snap model.Snapshot, nodes map[string]model.NodeInfo) (map[string]int, int) {
	activePerNode := make(map[string]int)
	activeTotal := 0
	for _, task := range snap.Tasks {
		if !task.State.Active() {
			continue
		}
		activePerNode[task.Node]++
		activeTotal++
	}

	busyPerNode := make(map[string]int, len(nodes))
	total := 0
	for name, node := range nodes {
		busy := activePerNode[name]
		if node.ActivePulls > busy {
			busy = node.ActivePulls
		}
		busyPerNode[name] = busy
		total += busy
	}
	if activeTotal > total {
		total = activeTotal
	}
	return busyPerNode, total
}

// diskPaused reports whether automatic pre-pulls must pause on this node.
func diskPaused(node model.NodeInfo, cfg Config) bool {
	if node.DiskPaused {
		return true
	}
	threshold := node.DiskPauseThreshold
	if threshold <= 0 {
		threshold = cfg.DiskPauseThreshold
	}
	if node.ImageFSCapacityBytes <= 0 {
		return false
	}
	return float64(node.ImageFSUsedBytes)/float64(node.ImageFSCapacityBytes) >= threshold
}

// namespaceRank orders workloads: configured namespaces first, everything else
// afterwards in alphabetical order.
func namespaceRank(namespace string, priority []string) int {
	ns := strings.ToLower(strings.TrimSpace(namespace))
	for i, p := range priority {
		if ns == strings.ToLower(p) {
			return i
		}
	}
	return len(priority)
}
