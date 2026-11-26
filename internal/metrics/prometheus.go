package metrics

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/local/image-prepuller/internal/model"
	"github.com/local/image-prepuller/internal/provider"
)

func Handler(reg *provider.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids := reg.AllIDs()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var b strings.Builder

		b.WriteString("# HELP prepuller_cluster_up Whether the cluster backend is reachable (1=up, 0=down).\n")
		b.WriteString("# TYPE prepuller_cluster_up gauge\n")
		for _, id := range ids {
			e, _ := reg.Get(id)
			snap := e.Provider.Snapshot(r.Context())
			up := 0
			if snap.Summary.TotalWorkers == 0 || snap.Summary.ReadyWorkers > 0 {
				up = 1
			}
			fmt.Fprintf(&b, "prepuller_cluster_up{cluster=%q,role=%q} %d\n", id, e.Role, up)
		}

		b.WriteString("\n# HELP prepuller_image_present Whether the workload image is present on the worker node (1=present, 0=not present).\n")
		b.WriteString("# TYPE prepuller_image_present gauge\n")
		for _, id := range ids {
			e, _ := reg.Get(id)
			snap := e.Provider.Snapshot(r.Context())
			for _, wl := range snap.Workloads {
				for _, img := range wl.Images {
					for _, ns := range img.Nodes {
						val := 0
						if ns.State == model.StatePresent {
							val = 1
						}
						fmt.Fprintf(&b, "prepuller_image_present{cluster=%q,namespace=%q,workload_kind=%q,workload_name=%q,image=%q,node=%q} %d\n",
							id, wl.Namespace, wl.Kind, wl.Name, img.Image, ns.Node, val)
					}
				}
			}
		}

		b.WriteString("\n# HELP prepuller_workload_policy_violation Policy violation on a marked workload (1=violation).\n")
		b.WriteString("# TYPE prepuller_workload_policy_violation gauge\n")
		for _, id := range ids {
			e, _ := reg.Get(id)
			snap := e.Provider.Snapshot(r.Context())
			for _, wl := range snap.Workloads {
				for _, img := range wl.Images {
					for _, v := range img.PolicyViolations {
						fmt.Fprintf(&b, "prepuller_workload_policy_violation{cluster=%q,namespace=%q,workload_kind=%q,workload_name=%q,image=%q,reason=%q} 1\n",
							id, wl.Namespace, wl.Kind, wl.Name, img.Image, v.Reason)
					}
				}
			}
		}

		b.WriteString("\n# HELP prepuller_node_agent_up Whether the prepuller node agent is reachable (1=up, 0=down).\n")
		b.WriteString("# TYPE prepuller_node_agent_up gauge\n")
		for _, id := range ids {
			e, _ := reg.Get(id)
			snap := e.Provider.Snapshot(r.Context())
			for _, n := range snap.Nodes {
				val := 0
				if n.AgentUp {
					val = 1
				}
				fmt.Fprintf(&b, "prepuller_node_agent_up{cluster=%q,node=%q} %d\n", id, n.Name, val)
			}
		}

		b.WriteString("\n# HELP prepuller_node_disk_paused Whether auto-prepulling is paused on the node due to ImageFS disk usage.\n")
		b.WriteString("# TYPE prepuller_node_disk_paused gauge\n")
		for _, id := range ids {
			e, _ := reg.Get(id)
			snap := e.Provider.Snapshot(r.Context())
			for _, n := range snap.Nodes {
				val := 0
				if n.DiskPaused {
					val = 1
				}
				fmt.Fprintf(&b, "prepuller_node_disk_paused{cluster=%q,node=%q} %d\n", id, n.Name, val)
			}
		}

		b.WriteString("\n# HELP prepuller_node_imagefs_used_bytes Used bytes on node ImageFS (EPHEMERAL).\n")
		b.WriteString("# TYPE prepuller_node_imagefs_used_bytes gauge\n")
		for _, id := range ids {
			e, _ := reg.Get(id)
			snap := e.Provider.Snapshot(r.Context())
			for _, n := range snap.Nodes {
				fmt.Fprintf(&b, "prepuller_node_imagefs_used_bytes{cluster=%q,node=%q} %d\n", id, n.Name, n.ImageFSUsedBytes)
			}
		}

		b.WriteString("\n# HELP prepuller_node_imagefs_capacity_bytes Capacity bytes on node ImageFS (EPHEMERAL).\n")
		b.WriteString("# TYPE prepuller_node_imagefs_capacity_bytes gauge\n")
		for _, id := range ids {
			e, _ := reg.Get(id)
			snap := e.Provider.Snapshot(r.Context())
			for _, n := range snap.Nodes {
				fmt.Fprintf(&b, "prepuller_node_imagefs_capacity_bytes{cluster=%q,node=%q} %d\n", id, n.Name, n.ImageFSCapacityBytes)
			}
		}

		b.WriteString("\n# HELP prepuller_pull_total Total image pull attempts.\n")
		b.WriteString("# TYPE prepuller_pull_total counter\n")
		for _, id := range ids {
			e, _ := reg.Get(id)
			for _, c := range e.Provider.PullCounters() {
				fmt.Fprintf(&b, "prepuller_pull_total{cluster=%q,image=%q,node=%q,result=%q} %d\n",
					id, c.Image, c.Node, c.Result, c.Count)
			}
		}

		_, _ = w.Write([]byte(b.String()))
	})
}
