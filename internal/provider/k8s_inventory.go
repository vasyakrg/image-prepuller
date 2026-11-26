package provider

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// ErrNodeInventoryUnavailable reports that the node agent has not served its CRI
// image inventory for a node (agent down, not deployed yet, or a transport error).
// It degrades one node to offline slots instead of failing the whole snapshot.
var ErrNodeInventoryUnavailable = errors.New("node inventory unavailable")

// NodeInventory is the node-agent (CRI) view of one node: the images present in
// the container runtime image store, the ImageFS usage and the agent build.
type NodeInventory struct {
	Images               []model.CachedImage
	ImageFSUsedBytes     int64
	ImageFSCapacityBytes int64
	AgentVersion         string
	ActivePulls          int
	LastSeen             time.Time
}

// NodeInventorySource is the seam between the node agent (WS-A) and the hub-side
// Kubernetes provider. Only the agent can talk to containerd, so K8sProvider
// never inspects a runtime itself: it asks this interface per node.
//
// Integration point for WS-A: replace UnavailableInventorySource with a
// gRPC-backed client that dials the agent DaemonSet
// (cmd/prepuller/agent.go + internal/agent, see docs/PLAN.md WS-A). The contract
// is deliberately tiny -- one method, no pull operations yet -- so WS-A can land
// the transport first and add pull triggers in a later change.
type NodeInventorySource interface {
	Inventory(ctx context.Context, node string) (NodeInventory, error)
}

// UnavailableInventorySource is the offline/unknown stub used while the node
// agent transport is not wired in. It always reports ErrNodeInventoryUnavailable,
// which makes every slot offline and keeps the snapshot partial -- a truthful
// "agent absent" picture rather than fake missing images.
type UnavailableInventorySource struct{}

// Inventory implements NodeInventorySource.
func (UnavailableInventorySource) Inventory(context.Context, string) (NodeInventory, error) {
	return NodeInventory{}, ErrNodeInventoryUnavailable
}

// StaticInventorySource serves a fixed inventory map. Tests and offline
// snapshots (e.g. an imported agent export) use it; production uses the gRPC
// client from WS-A.
type StaticInventorySource struct {
	ByNode map[string]NodeInventory
}

// NewStaticInventorySource returns a source over the given per-node inventory.
func NewStaticInventorySource(byNode map[string]NodeInventory) *StaticInventorySource {
	return &StaticInventorySource{ByNode: byNode}
}

// Inventory implements NodeInventorySource.
func (s *StaticInventorySource) Inventory(_ context.Context, node string) (NodeInventory, error) {
	if s == nil || s.ByNode == nil {
		return NodeInventory{}, ErrNodeInventoryUnavailable
	}
	inv, ok := s.ByNode[node]
	if !ok {
		return NodeInventory{}, ErrNodeInventoryUnavailable
	}
	inv.Images = append([]model.CachedImage(nil), inv.Images...)
	return inv, nil
}

// findCachedImage looks up an image in a node inventory. The container runtime
// reports resolved references (docker.io/library/nginx:latest, repo:tag@sha256:...)
// while a pod spec may carry the short form, so matching is digest-first and
// falls back to a normalized repository plus tag comparison.
func findCachedImage(inv NodeInventory, want string) (model.CachedImage, bool) {
	want = strings.TrimSpace(want)
	if want == "" {
		return model.CachedImage{}, false
	}
	for _, cached := range inv.Images {
		if strings.TrimSpace(cached.Image) == want {
			return cached, true
		}
	}

	wantRepo, wantTag, wantDigest := splitImageParts(want)
	for _, cached := range inv.Images {
		repo, tag, digest := splitImageParts(cached.Image)
		if wantDigest != "" && digest != "" {
			if digest == wantDigest {
				return cached, true
			}
			continue
		}
		if normalizeImageRepo(repo) != normalizeImageRepo(wantRepo) {
			continue
		}
		switch {
		case wantTag != "" && tag != "" && wantTag == tag:
			return cached, true
		case wantTag == "" && tag == "":
			return cached, true
		}
	}
	return model.CachedImage{}, false
}

// splitImageParts splits an image reference into repository, tag and digest.
func splitImageParts(image string) (repo, tag, digest string) {
	ref := model.ParseImageRef(image)
	repo = ref.Image
	if ref.Tag != "" {
		repo = strings.TrimSuffix(repo, ":"+ref.Tag)
	}
	return repo, ref.Tag, ref.Digest
}

// normalizeImageRepo folds the implicit Docker Hub prefixes so that
// "docker.io/library/nginx" and "nginx" compare equal.
func normalizeImageRepo(repo string) string {
	r := strings.ToLower(strings.TrimSpace(repo))
	for _, prefix := range []string{
		"docker.io/library/",
		"index.docker.io/library/",
		"docker.io/",
		"index.docker.io/",
		"library/",
	} {
		if strings.HasPrefix(r, prefix) {
			return strings.TrimPrefix(r, prefix)
		}
	}
	return r
}
