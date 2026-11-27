package provider

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/local/image-prepuller/internal/model"
)

// nodeConditionTypes is the Node condition subset the hub renders. Everything
// else in Node.Status.Conditions is internal plumbing and stays out of the API.
var nodeConditionTypes = map[corev1.NodeConditionType]struct{}{
	corev1.NodeReady:          {},
	corev1.NodeMemoryPressure: {},
	corev1.NodeDiskPressure:   {},
	corev1.NodePIDPressure:    {},
}

// k8sNode is one cluster node plus the inventory the node agent reported for it.
type k8sNode struct {
	info        model.NodeInfo
	inventory   NodeInventory
	inventoryOK bool
}

// listNodes lists cluster nodes together with their node-agent inventory. A
// per-node inventory error degrades that node (offline slots) but never fails the
// whole snapshot, so one dead agent cannot blank the UI.
func (p *K8sProvider) listNodes(ctx context.Context) ([]k8sNode, error) {
	list, err := p.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: p.nodeLabelSelector})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	out := make([]k8sNode, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, p.nodeFromK8s(ctx, &list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].info.Name < out[j].info.Name })
	return out, nil
}

func (p *K8sProvider) nodeFromK8s(ctx context.Context, node *corev1.Node) k8sNode {
	info := model.NodeInfo{
		Name:               node.Name,
		DiskPauseThreshold: p.diskPauseRatio,
		KubeletGCThreshold: p.kubeletGCRatio,
	}
	for _, cond := range node.Status.Conditions {
		if _, ok := nodeConditionTypes[cond.Type]; !ok {
			continue
		}
		info.Conditions = append(info.Conditions, model.NodeCondition{
			Type:           string(cond.Type),
			Status:         string(cond.Status),
			Reason:         cond.Reason,
			Message:        cond.Message,
			LastTransition: cond.LastTransitionTime.Time,
		})
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			info.Ready = true
		}
	}

	inventory, err := p.inventory.Inventory(ctx, node.Name)
	if err != nil {
		// Agent absent or unreachable: the kubelet view is still valid, but the
		// image cache is unknown, so slots stay offline.
		p.recordAgentError(node.Name, err)
		return k8sNode{info: info}
	}

	info.AgentUp = true
	info.AgentVersion = inventory.AgentVersion
	info.ImageFSUsedBytes = inventory.ImageFSUsedBytes
	info.ImageFSCapacityBytes = inventory.ImageFSCapacityBytes
	info.ActivePulls = inventory.ActivePulls
	info.CachedImages = append([]model.CachedImage(nil), inventory.Images...)
	info.CachedImageCount = len(info.CachedImages)
	info.LastSeen = inventory.LastSeen
	if info.LastSeen.IsZero() {
		info.LastSeen = p.now()
	}
	if info.Ready && info.ImageFSCapacityBytes > 0 {
		info.DiskPaused = float64(info.ImageFSUsedBytes)/float64(info.ImageFSCapacityBytes) >= p.diskPauseRatio
	}
	return k8sNode{info: info, inventory: inventory, inventoryOK: true}
}
