package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// WireAPIVersion is the version of the agent HTTP/JSON wire contract. The hub
// sends it in every request it cares about and the agent echoes it in every
// response; bump it only together with a contract PR.
const WireAPIVersion = "v1"

// MaxSnapshotImages bounds one snapshot response. Hub only needs the images it
// can map to workloads; hundreds of stale tags are dropped by SelectImages.
const MaxSnapshotImages = 500

// ErrNotImplemented marks agent features that WS-A still has to build.
var ErrNotImplemented = errors.New("agent: not implemented yet")

// Condition mirrors one CRI runtime condition (RuntimeReady, NetworkReady, ...).
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"` // "True" | "False" | "Unknown"
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// RuntimeState is the readiness subset of the container runtime the agent
// forwards to the hub.
type RuntimeState struct {
	Ready      bool        `json:"ready"`
	Conditions []Condition `json:"conditions,omitempty"`
}

// RuntimeInfo identifies the container runtime behind the CRI socket.
type RuntimeInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// ImageFSStats describes the image filesystem of the node. Capacity and
// Available come from statfs (CRI ImageFsInfo only reports Usage), so they may
// be zero when the mountpoint is unknown; then the agent never pauses on disk.
type ImageFSStats struct {
	Mountpoint     string  `json:"mountpoint,omitempty"`
	UsedBytes      int64   `json:"usedBytes"`
	CapacityBytes  int64   `json:"capacityBytes"`
	AvailableBytes int64   `json:"availableBytes"`
	UsedRatio      float64 `json:"usedRatio"`
}

// ImageInfo is one image cached on the node.
type ImageInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name,omitempty"` // primary reference: first tag, digest or ID
	Tags      []string `json:"tags,omitempty"`
	Digests   []string `json:"digests,omitempty"`
	SizeBytes int64    `json:"sizeBytes"`
	Pinned    bool     `json:"pinned,omitempty"`
}

// Snapshot is the wire response of GET /agent/v1/snapshot: everything the hub
// needs to know about one node.
type Snapshot struct {
	APIVersion       string       `json:"apiVersion"`
	NodeName         string       `json:"node"`
	AgentVersion     string       `json:"agentVersion"`
	GeneratedAt      time.Time    `json:"generatedAt"`
	Ready            bool         `json:"ready"`
	Conditions       []Condition  `json:"conditions,omitempty"`
	Runtime          RuntimeInfo  `json:"runtime"`
	AuthEnabled      bool         `json:"authEnabled"`
	DiskPauseRatio   float64      `json:"diskPauseRatio"`
	DiskPaused       bool         `json:"diskPaused"`
	ImageFS          ImageFSStats `json:"imageFs"`
	CachedImageCount int          `json:"cachedImageCount"`
	TotalImageBytes  int64        `json:"totalImageBytes"`
	Images           []ImageInfo  `json:"images"`
}

// RegistryAuth is the registry credential the hub resolved for an image from the
// workload's imagePullSecrets and sent along with the pull request.
//
// A node agent is not the kubelet: CRI PullImage runs anonymously unless the
// caller passes an AuthConfig, and Kubernetes applies imagePullSecrets only to
// pods it creates itself. So without this hand-off a private registry answers
// 403 to every pull on every node where the image is not cached yet — even
// though the workload that references it does have a pull secret.
//
// The fields mirror the subset of the CRI AuthConfig that a registry login
// needs. A nil Auth means "pull anonymously", exactly like a pod without
// imagePullSecrets.
type RegistryAuth struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identityToken,omitempty"`
	RegistryToken string `json:"registryToken,omitempty"`
}

// Empty reports whether the credential carries nothing a runtime could use.
func (a *RegistryAuth) Empty() bool {
	if a == nil {
		return true
	}
	return a.Username == "" && a.Password == "" && a.Auth == "" &&
		a.IdentityToken == "" && a.RegistryToken == ""
}

// ImageService is the CRI image surface used by the agent. Keep it small: the
// only implementations are the real CRI client and the in-memory fake.
type ImageService interface {
	ListImages(ctx context.Context) ([]ImageInfo, error)
	ImageStatus(ctx context.Context, image string) (ImageInfo, bool, error)
	PullImage(ctx context.Context, image string, auth *RegistryAuth) (string, error)
	ImageFS(ctx context.Context) (ImageFSStats, error)
}

// RuntimeService is the CRI runtime surface used by the agent.
type RuntimeService interface {
	RuntimeState(ctx context.Context) (RuntimeState, error)
	RuntimeVersion(ctx context.Context) (RuntimeInfo, error)
}

// CRI is the full CRI backend the agent depends on. Tests substitute a fake.
type CRI interface {
	ImageService
	RuntimeService
}

// BuildSnapshot inventories images, ImageFS and runtime state of one node.
// Any CRI failure aborts the snapshot so the hub can mark the agent degraded
// instead of showing a half-true cache inventory.
func BuildSnapshot(ctx context.Context, cri CRI, cfg Config) (Snapshot, error) {
	if cri == nil {
		return Snapshot{}, errors.New("agent: nil CRI backend")
	}
	images, err := cri.ListImages(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("agent: list images: %w", err)
	}
	fs, err := cri.ImageFS(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("agent: image filesystem info: %w", err)
	}
	state, err := cri.RuntimeState(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("agent: runtime status: %w", err)
	}
	runtime, err := cri.RuntimeVersion(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("agent: runtime version: %w", err)
	}

	selected := SelectImages(images, MaxSnapshotImages)
	var total int64
	for _, img := range selected {
		total += img.SizeBytes
	}
	fs.UsedRatio = UsedRatio(fs.UsedBytes, fs.CapacityBytes)

	return Snapshot{
		APIVersion:       WireAPIVersion,
		NodeName:         cfg.NodeName,
		AgentVersion:     Version,
		GeneratedAt:      time.Now().UTC(),
		Ready:            state.Ready,
		Conditions:       state.Conditions,
		Runtime:          runtime,
		AuthEnabled:      cfg.Token != "",
		DiskPauseRatio:   cfg.DiskPauseRatio,
		DiskPaused:       ShouldPause(fs.UsedBytes, fs.CapacityBytes, cfg.DiskPauseRatio),
		ImageFS:          fs,
		CachedImageCount: len(selected),
		TotalImageBytes:  total,
		Images:           selected,
	}, nil
}

// UsedRatio returns used/capacity clamped to [0,1]; unknown capacity yields 0.
func UsedRatio(used, capacity int64) float64 {
	if capacity <= 0 || used <= 0 {
		return 0
	}
	ratio := float64(used) / float64(capacity)
	if ratio > 1 {
		return 1
	}
	return ratio
}

// ShouldPause reports whether auto-prepull must stay paused for the given
// image filesystem usage. A ratio at or above the threshold pauses (the plan
// says "ImageFS >= 78%"). Unknown capacity never pauses; a threshold outside
// (0,1) is treated as misconfiguration and never pauses either.
func ShouldPause(used, capacity int64, threshold float64) bool {
	if capacity <= 0 {
		return false
	}
	if threshold <= 0 || threshold >= 1 {
		return false
	}
	return UsedRatio(used, capacity) >= threshold
}

// SelectImages normalizes a raw CRI image list for the wire snapshot: it drops
// unidentifiable entries, picks a primary name, de-duplicates by image ID,
// sorts by size (largest first, ties by name) and truncates to limit
// (limit <= 0 means no truncation). It never mutates the input slice.
func SelectImages(all []ImageInfo, limit int) []ImageInfo {
	seen := make(map[string]struct{}, len(all))
	out := make([]ImageInfo, 0, len(all))
	for _, img := range all {
		img = cloneImage(img)
		if img.Name == "" {
			img.Name = primaryReference(img)
		}
		if img.Name == "" && img.ID == "" {
			continue
		}
		key := img.ID
		if key == "" {
			key = img.Name
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, img)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SizeBytes != out[j].SizeBytes {
			return out[i].SizeBytes > out[j].SizeBytes
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func primaryReference(img ImageInfo) string {
	if len(img.Tags) > 0 {
		return img.Tags[0]
	}
	if len(img.Digests) > 0 {
		return img.Digests[0]
	}
	return img.ID
}

func cloneImage(img ImageInfo) ImageInfo {
	img.Tags = append([]string(nil), img.Tags...)
	img.Digests = append([]string(nil), img.Digests...)
	return img
}
