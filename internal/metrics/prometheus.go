package metrics

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

// text exposition format version written by the handler.
const expositionVersion = "0.0.4"

// snapshotDeadline bounds one cluster's fetch (Snapshot plus PullCounters)
// inside a scrape. It exists because a hub<->agent partition makes
// K8sProvider.Snapshot cost its per-agent timeout (5s) for every node while
// holding the provider mutex: that is far above the usual Prometheus
// scrape_timeout (10s), so without a bound the whole scrape would fail and every
// hub series would vanish exactly when the cluster is in trouble.
//
// All clusters are fetched concurrently, so a scrape is bounded by this value
// regardless of how many clusters are registered. Three seconds stays well
// under the default scrape timeout and still fits a slow-but-healthy agent.
const snapshotDeadline = 3 * time.Second

// clusterSnapshot is the state of one cluster captured exactly once per scrape.
// Every series builder consumes this struct instead of calling Snapshot again,
// so a scrape costs one provider call per cluster.
//
// A cluster that answered too late is represented by its last successful
// snapshot (see collector); a cluster that never answered is omitted entirely.
type clusterSnapshot struct {
	id       string
	role     string
	snap     model.Snapshot
	counters []model.PullMetricSample
	// agentErrors is nil unless the provider implements
	// provider.AgentErrorCounter; the series is then not emitted at all.
	agentErrors map[provider.AgentErrorKey]uint64
}

// collector is the stateful part of /metrics. Handler creates it once so the
// last successful snapshot per cluster survives a failed scrape and can be
// served during a partition, keeping dashboards coherent (stale but complete)
// instead of blank.
type collector struct {
	reg *provider.Registry

	mu       sync.Mutex
	last     map[string]cachedSnapshot
	timeouts map[string]uint64
}

// cachedSnapshot is the last snapshot a cluster returned together with the local
// time it was captured, and the matching pull counters.
type cachedSnapshot struct {
	snap       model.Snapshot
	counters   []model.PullMetricSample
	capturedAt time.Time
}

func newCollector(reg *provider.Registry) *collector {
	return &collector{
		reg:      reg,
		last:     make(map[string]cachedSnapshot),
		timeouts: make(map[string]uint64),
	}
}

func (c *collector) remember(id string, snap model.Snapshot, counters []model.PullMetricSample, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last[id] = cachedSnapshot{snap: snap, counters: counters, capturedAt: at}
}

func (c *collector) cached(id string) (cachedSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.last[id]
	return cached, ok
}

func (c *collector) recordTimeouts(ids []string) {
	if len(ids) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		c.timeouts[id]++
	}
}

// timeoutCounts returns a copy of the scrape-timeout counters.
func (c *collector) timeoutCounts() map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]uint64, len(c.timeouts))
	for id, count := range c.timeouts {
		out[id] = count
	}
	return out
}

// prune drops cached state for clusters that are no longer registered.
func (c *collector) prune(ids []string) {
	present := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		present[id] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.last {
		if _, ok := present[id]; !ok {
			delete(c.last, id)
		}
	}
	for id := range c.timeouts {
		if _, ok := present[id]; !ok {
			delete(c.timeouts, id)
		}
	}
}

// exposition accumulates Prometheus text 0.0.4 output. Headers are always
// emitted for a family, even when it has no samples, so the series stays
// self-documenting for dashboards and alert rules.
type exposition struct {
	b strings.Builder
}

func (e *exposition) header(name, help, typ string) {
	fmt.Fprintf(&e.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (e *exposition) line(format string, args ...any) {
	fmt.Fprintf(&e.b, format+"\n", args...)
}

func (e *exposition) String() string {
	return e.b.String()
}

// Handler renders /metrics. Cluster order comes from Registry.AllIDs (sorted),
// node, workload and task order follows the provider snapshot, so the output is
// stable between scrapes for the same state.
//
// The handler is stateful on purpose: one collector is created here and reused
// for every scrape, so it can serve the last successful snapshot of a cluster
// that is currently partitioned instead of blanking the whole family. Handler
// itself never blocks longer than snapshotDeadline (plus output rendering).
func Handler(reg *provider.Registry) http.Handler {
	c := newCollector(reg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version="+expositionVersion+"; charset=utf-8")

		clusters, scrapeTimeouts := c.collect(r.Context())
		e := &exposition{}

		writeClusterUp(e, clusters)
		writeImagePresent(e, clusters)
		writePolicyViolations(e, clusters)
		writeNodeAgentUp(e, clusters)
		writeNodeDiskPaused(e, clusters)
		writeNodeImageFSUsed(e, clusters)
		writeNodeImageFSCapacity(e, clusters)

		// Stage 2/3 series.
		writeNodeImageFSFree(e, clusters)
		writeNodeReady(e, clusters)
		writeImagesCached(e, clusters)
		writeAgentInfo(e, clusters)
		writePullTasks(e, clusters)
		writePullTaskDuration(e, clusters)
		writeClusterLastSync(e, clusters)
		writeClusterSyncPartial(e, clusters)
		writeSnapshotAge(e, clusters, time.Now())

		// Agent-failure and partition visibility (hub side).
		writeNodeLastSeen(e, clusters)
		writeNodeAgentErrors(e, clusters)
		writeSnapshotScrapeTimeout(e, reg.AllIDs(), scrapeTimeouts)

		writeBuildInfo(e)

		writePullTotal(e, clusters)

		_, _ = w.Write([]byte(e.String()))
	})
}

// collect captures one snapshot per registered cluster. Keep this the only place
// that calls Provider.Snapshot from the metrics package (the
// TestHandlerCallsSnapshotOncePerCluster invariant).
//
// Every cluster is fetched in its own goroutine; a snapshot that misses
// snapshotDeadline increments the per-cluster timeout counter and is replaced by
// the last successful snapshot (if any). Results that arrive after the deadline
// are discarded: each channel is buffered, so the abandoned goroutine never
// blocks and no goroutine outlives the provider call it is stuck in. The request
// context is passed through, so once the scrape returns the abandoned call is
// cancelled and a provider holding a mutex is released promptly.
func (c *collector) collect(ctx context.Context) ([]clusterSnapshot, map[string]uint64) {
	ids := c.reg.AllIDs()

	type fetch struct {
		id      string
		role    string
		counter provider.AgentErrorCounter // nil when the provider has no counters
		snap    chan model.Snapshot
		metrics chan []model.PullMetricSample
	}
	fetches := make([]fetch, 0, len(ids))
	for _, id := range ids {
		entry, ok := c.reg.Get(id)
		if !ok {
			continue
		}
		f := fetch{
			id:      id,
			role:    entry.Role,
			snap:    make(chan model.Snapshot, 1),
			metrics: make(chan []model.PullMetricSample, 1),
		}
		// Optional capability: MockProvider and RemoteProvider do not implement
		// AgentErrorCounter, so their scrapes emit no agent-error samples.
		if counter, ok := entry.Provider.(provider.AgentErrorCounter); ok {
			f.counter = counter
		}
		fetches = append(fetches, f)
		go func(p provider.Provider, out fetch) {
			out.snap <- p.Snapshot(ctx)
			out.metrics <- p.PullCounters()
		}(entry.Provider, f)
	}

	timer := time.NewTimer(snapshotDeadline)
	defer timer.Stop()

	clusters := make([]clusterSnapshot, 0, len(fetches))
	var timedOut []string
	expired := false
	for _, f := range fetches {
		snap, fresh := model.Snapshot{}, false
		if !expired {
			select {
			case snap = <-f.snap:
				fresh = true
			case <-timer.C:
				expired = true
			}
		}
		if !fresh && expired {
			// Another cluster burned the deadline; pick up this one if it is
			// already done anyway, so a fast provider is not reported as slow.
			select {
			case snap = <-f.snap:
				fresh = true
			default:
			}
		}

		var counters []model.PullMetricSample
		if fresh && !expired {
			select {
			case counters = <-f.metrics:
			case <-timer.C:
				expired = true
			}
		} else if fresh {
			select {
			case counters = <-f.metrics:
			default:
			}
		}

		if !fresh {
			timedOut = append(timedOut, f.id)
			cached, ok := c.cached(f.id)
			if !ok {
				// Never snapshotted: emitting zeroes would make a hub that has
				// gone blind look healthy. The timeout counter is the only series
				// this cluster gets this scrape.
				continue
			}
			clusters = append(clusters, clusterSnapshot{
				id:          f.id,
				role:        f.role,
				snap:        cached.snap,
				counters:    cached.counters,
				agentErrors: agentErrorsOf(f.counter),
			})
			continue
		}

		now := time.Now()
		if counters == nil {
			// Pull counters may be a separate network call; when they miss the
			// deadline keep the previous values instead of dropping the series.
			if cached, ok := c.cached(f.id); ok {
				counters = cached.counters
			}
		}
		c.remember(f.id, snap, counters, now)
		clusters = append(clusters, clusterSnapshot{
			id:          f.id,
			role:        f.role,
			snap:        snap,
			counters:    counters,
			agentErrors: agentErrorsOf(f.counter),
		})
	}

	c.recordTimeouts(timedOut)
	c.prune(ids)
	return clusters, c.timeoutCounts()
}

// agentErrorsOf reads the optional provider capability. It is called after the
// snapshot returned, so counters incremented by that snapshot are included.
func agentErrorsOf(counter provider.AgentErrorCounter) map[provider.AgentErrorKey]uint64 {
	if counter == nil {
		return nil
	}
	return counter.AgentErrors()
}

func writeClusterUp(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_cluster_up",
		"Whether the cluster backend is reachable (1=up, 0=down).",
		"gauge")
	for _, c := range clusters {
		up := 0
		// An offline or partial snapshot must never look healthy, even when the
		// provider returns an empty snapshot (see the RemoteProvider gotcha).
		if c.snap.SyncStatus != model.SyncOffline && !c.snap.Partial {
			if c.snap.Summary.TotalWorkers == 0 || c.snap.Summary.ReadyWorkers > 0 {
				up = 1
			}
		}
		e.line("prepuller_cluster_up{cluster=%q,role=%q} %d", c.id, c.role, up)
	}
}

func writeImagePresent(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_image_present",
		"Whether the workload image is present on the worker node (1=present, 0=not present).",
		"gauge")
	for _, c := range clusters {
		for _, wl := range c.snap.Workloads {
			for _, img := range wl.Images {
				for _, ns := range img.Nodes {
					val := 0
					if ns.State == model.StatePresent {
						val = 1
					}
					e.line("prepuller_image_present{cluster=%q,namespace=%q,workload_kind=%q,workload_name=%q,image=%q,node=%q} %d",
						c.id, wl.Namespace, wl.Kind, wl.Name, img.Image, ns.Node, val)
				}
			}
		}
	}
}

func writePolicyViolations(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_workload_policy_violation",
		"Policy violation on a marked workload (1=violation).",
		"gauge")
	for _, c := range clusters {
		for _, wl := range c.snap.Workloads {
			for _, img := range wl.Images {
				for _, v := range img.PolicyViolations {
					e.line("prepuller_workload_policy_violation{cluster=%q,namespace=%q,workload_kind=%q,workload_name=%q,image=%q,reason=%q} 1",
						c.id, wl.Namespace, wl.Kind, wl.Name, img.Image, v.Reason)
				}
			}
		}
	}
}

func writeNodeAgentUp(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_node_agent_up",
		"Whether the prepuller node agent is reachable (1=up, 0=down).",
		"gauge")
	for _, c := range clusters {
		for _, n := range c.snap.Nodes {
			val := 0
			if n.AgentUp {
				val = 1
			}
			e.line("prepuller_node_agent_up{cluster=%q,node=%q} %d", c.id, n.Name, val)
		}
	}
}

func writeNodeDiskPaused(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_node_disk_paused",
		"Whether auto-prepulling is paused on the node due to ImageFS disk usage.",
		"gauge")
	for _, c := range clusters {
		for _, n := range c.snap.Nodes {
			val := 0
			if n.DiskPaused {
				val = 1
			}
			e.line("prepuller_node_disk_paused{cluster=%q,node=%q} %d", c.id, n.Name, val)
		}
	}
}

func writeNodeImageFSUsed(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_node_imagefs_used_bytes",
		"Used bytes on node ImageFS (EPHEMERAL).",
		"gauge")
	for _, c := range clusters {
		for _, n := range c.snap.Nodes {
			e.line("prepuller_node_imagefs_used_bytes{cluster=%q,node=%q} %d", c.id, n.Name, n.ImageFSUsedBytes)
		}
	}
}

func writeNodeImageFSCapacity(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_node_imagefs_capacity_bytes",
		"Capacity bytes on node ImageFS (EPHEMERAL).",
		"gauge")
	for _, c := range clusters {
		for _, n := range c.snap.Nodes {
			e.line("prepuller_node_imagefs_capacity_bytes{cluster=%q,node=%q} %d", c.id, n.Name, n.ImageFSCapacityBytes)
		}
	}
}

func writeNodeImageFSFree(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_node_imagefs_free_bytes",
		"Free bytes on node ImageFS (capacity - used; omitted when capacity is unknown).",
		"gauge")
	for _, c := range clusters {
		for _, n := range c.snap.Nodes {
			if n.ImageFSCapacityBytes <= 0 {
				continue
			}
			free := n.ImageFSCapacityBytes - n.ImageFSUsedBytes
			if free < 0 {
				free = 0
			}
			e.line("prepuller_node_imagefs_free_bytes{cluster=%q,node=%q} %d", c.id, n.Name, free)
		}
	}
}

func writeNodeReady(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_node_ready",
		"Whether the Kubernetes node is Ready (1=Ready, 0=NotReady).",
		"gauge")
	for _, c := range clusters {
		for _, n := range c.snap.Nodes {
			val := 0
			if n.Ready {
				val = 1
			}
			e.line("prepuller_node_ready{cluster=%q,node=%q} %d", c.id, n.Name, val)
		}
	}
}

func writeImagesCached(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_images_cached_total",
		"Number of workload images currently cached on the node.",
		"gauge")
	for _, c := range clusters {
		for _, n := range c.snap.Nodes {
			cached := len(n.CachedImages)
			if cached == 0 {
				// Providers may report only the count without the image list.
				cached = n.CachedImageCount
			}
			e.line("prepuller_images_cached_total{cluster=%q,node=%q} %d", c.id, n.Name, cached)
		}
	}
}

func writeAgentInfo(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_agent_info",
		"Prepuller node agent build information (always 1; version label carries the build).",
		"gauge")
	for _, c := range clusters {
		for _, n := range c.snap.Nodes {
			if n.AgentVersion == "" {
				continue
			}
			e.line("prepuller_agent_info{cluster=%q,node=%q,version=%q} 1", c.id, n.Name, n.AgentVersion)
		}
	}
}

// pullTaskStates is the fixed emission order for prepuller_pull_tasks so the
// output does not depend on map iteration or task ordering.
var pullTaskStates = []model.TaskState{
	model.TaskQueued,
	model.TaskRunning,
	model.TaskSucceeded,
	model.TaskFailed,
	model.TaskCanceled,
}

func writePullTasks(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_pull_tasks",
		"Number of pull tasks per state.",
		"gauge")
	for _, c := range clusters {
		counts := make(map[model.TaskState]int, len(pullTaskStates))
		for _, t := range c.snap.Tasks {
			counts[t.State]++
		}
		for _, state := range pullTaskStates {
			e.line("prepuller_pull_tasks{cluster=%q,state=%q} %d", c.id, state, counts[state])
		}
	}
}

func writePullTaskDuration(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_pull_task_duration_seconds",
		"Average duration of finished pull tasks in seconds, by terminal state.",
		"gauge")
	for _, c := range clusters {
		for _, state := range []model.TaskState{model.TaskSucceeded, model.TaskFailed, model.TaskCanceled} {
			var totalMs int64
			count := 0
			for _, t := range c.snap.Tasks {
				if t.State != state || t.DurationMs <= 0 {
					continue
				}
				totalMs += t.DurationMs
				count++
			}
			if count == 0 {
				continue
			}
			avg := float64(totalMs) / float64(count) / 1000.0
			e.line("prepuller_pull_task_duration_seconds{cluster=%q,state=%q} %s",
				c.id, state, strconv.FormatFloat(avg, 'g', -1, 64))
		}
	}
}

func writeClusterLastSync(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_cluster_last_sync_timestamp_seconds",
		"Unix timestamp of the last successful cluster sync (omitted when unknown).",
		"gauge")
	for _, c := range clusters {
		if c.snap.LastSyncAt.IsZero() {
			continue
		}
		e.line("prepuller_cluster_last_sync_timestamp_seconds{cluster=%q} %d", c.id, c.snap.LastSyncAt.Unix())
	}
}

func writeClusterSyncPartial(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_cluster_sync_partial",
		"Whether the last cluster snapshot is partial (1=partial, 0=complete).",
		"gauge")
	for _, c := range clusters {
		val := 0
		if c.snap.Partial {
			val = 1
		}
		e.line("prepuller_cluster_sync_partial{cluster=%q} %d", c.id, val)
	}
}

func writeNodeLastSeen(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_node_last_seen_timestamp_seconds",
		"Unix timestamp of the last successful node-agent contact (omitted when unknown).",
		"gauge")
	for _, c := range clusters {
		for _, n := range c.snap.Nodes {
			// Zero means the agent never answered (or the provider does not track
			// it); a fabricated timestamp would hide a dead agent behind a fresh
			// looking series, so the value is omitted instead.
			if n.LastSeen.IsZero() {
				continue
			}
			e.line("prepuller_node_last_seen_timestamp_seconds{cluster=%q,node=%q} %d",
				c.id, n.Name, n.LastSeen.Unix())
		}
	}
}

// agentErrorReasons is the closed reason set in stable emission order.
var agentErrorReasons = []string{
	provider.ReasonUnauthorized,
	provider.ReasonProtocol,
	provider.ReasonTimeout,
	provider.ReasonUnavailable,
}

// writeNodeAgentErrors emits the optional per-node agent failure counters. Only
// reasons actually observed are emitted (counters are monotonic, so a series
// appears after the first failure and then stays). Providers that do not
// implement provider.AgentErrorCounter -- MockProvider and RemoteProvider --
// emit no samples at all; that is expected, not a bug.
func writeNodeAgentErrors(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_node_agent_errors_total",
		"Node-agent inventory failures per node and reason (only observed reasons are emitted; providers without the optional AgentErrorCounter capability emit nothing).",
		"counter")
	for _, c := range clusters {
		if len(c.agentErrors) == 0 {
			continue
		}
		nodes := make([]string, 0, len(c.agentErrors))
		seen := make(map[string]struct{}, len(c.agentErrors))
		for key := range c.agentErrors {
			if _, ok := seen[key.Node]; ok {
				continue
			}
			seen[key.Node] = struct{}{}
			nodes = append(nodes, key.Node)
		}
		sort.Strings(nodes)
		for _, node := range nodes {
			for _, reason := range agentErrorReasons {
				count, ok := c.agentErrors[provider.AgentErrorKey{Node: node, Reason: reason}]
				if !ok {
					continue
				}
				e.line("prepuller_node_agent_errors_total{cluster=%q,node=%q,reason=%q} %d",
					c.id, node, reason, count)
			}
		}
	}
}

func writeSnapshotAge(e *exposition, clusters []clusterSnapshot, now time.Time) {
	e.header("prepuller_snapshot_age_seconds",
		"Seconds since the last successful cluster snapshot (omitted while the cluster never synced; use prepuller_cluster_up then).",
		"gauge")
	for _, c := range clusters {
		// Zero LastSyncAt means the cluster never synced successfully; its
		// liveness is prepuller_cluster_up, not an age of unix epoch.
		if c.snap.LastSyncAt.IsZero() {
			continue
		}
		// A stale cached snapshot keeps its original LastSyncAt, so the age rises
		// during a partition: that is exactly the "hub went blind" signal.
		age := now.Sub(c.snap.LastSyncAt).Seconds()
		if age < 0 {
			// Clock skew between hub and spoke must not produce a negative age.
			age = 0
		}
		e.line("prepuller_snapshot_age_seconds{cluster=%q} %d", c.id, int64(age))
	}
}

func writeSnapshotScrapeTimeout(e *exposition, ids []string, timeouts map[string]uint64) {
	e.header("prepuller_snapshot_scrape_timeout_total",
		"Scrapes where the cluster snapshot did not return within the scrape deadline.",
		"counter")
	for _, id := range ids {
		e.line("prepuller_snapshot_scrape_timeout_total{cluster=%q} %d", id, timeouts[id])
	}
}

// buildInfo reports the binary version, commit and Go version. The project
// does not pass -ldflags at build time, so the values come from
// debug.ReadBuildInfo (module version and vcs.revision build settings).
func buildInfo() (version, commit, goVersion string) {
	version, goVersion = "devel", runtime.Version()
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
		if bi.GoVersion != "" {
			goVersion = bi.GoVersion
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				commit = s.Value
			}
		}
	}
	if commit == "" {
		commit = "unknown"
	}
	return version, commit, goVersion
}

func writeBuildInfo(e *exposition) {
	e.header("prepuller_build_info",
		"Prepuller hub/spoke build information (always 1; version labels carry the build).",
		"gauge")
	version, commit, goVersion := buildInfo()
	e.line("prepuller_build_info{version=%q,commit=%q,go_version=%q} 1", version, commit, goVersion)
}

func writePullTotal(e *exposition, clusters []clusterSnapshot) {
	e.header("prepuller_pull_total",
		"Total image pull attempts.",
		"counter")
	for _, c := range clusters {
		for _, m := range c.counters {
			e.line("prepuller_pull_total{cluster=%q,image=%q,node=%q,result=%q} %d",
				c.id, m.Image, m.Node, m.Result, m.Count)
		}
	}
}
