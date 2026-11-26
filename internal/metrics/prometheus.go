package metrics

import (
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

// text exposition format version written by the handler.
const expositionVersion = "0.0.4"

// clusterSnapshot is the state of one cluster captured exactly once per scrape.
// Every series builder consumes this struct instead of calling Snapshot again,
// so a scrape costs one provider call per cluster.
type clusterSnapshot struct {
	id       string
	role     string
	snap     model.Snapshot
	counters []model.PullMetricSample
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
func Handler(reg *provider.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version="+expositionVersion+"; charset=utf-8")

		clusters := collectClusters(reg, r)
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
		writeBuildInfo(e)

		writePullTotal(e, clusters)

		_, _ = w.Write([]byte(e.String()))
	})
}

// collectClusters captures one snapshot per registered cluster. Keep this the
// only place that calls Provider.Snapshot from the metrics package.
func collectClusters(reg *provider.Registry, r *http.Request) []clusterSnapshot {
	ids := reg.AllIDs()
	clusters := make([]clusterSnapshot, 0, len(ids))
	for _, id := range ids {
		entry, ok := reg.Get(id)
		if !ok {
			continue
		}
		clusters = append(clusters, clusterSnapshot{
			id:       id,
			role:     entry.Role,
			snap:     entry.Provider.Snapshot(r.Context()),
			counters: entry.Provider.PullCounters(),
		})
	}
	return clusters
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
