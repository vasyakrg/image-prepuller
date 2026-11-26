package model

import (
	"strings"
	"time"
)

// APIVersion is the wire contract version shared by hub, spoke and node agent.
// The hub is upgraded before the spoke, and the spoke keeps accepting the
// previous version for one release (see docs/PLAN.md section 10).
const APIVersion = "v2"

// PreviousAPIVersion is still accepted on incoming spoke requests while the
// rollout window is open.
const PreviousAPIVersion = "v1"

type PullState string

const (
	StatePresent    PullState = "present"
	StatePulling    PullState = "pulling"
	StateMissing    PullState = "missing"
	StateFailed     PullState = "failed"
	StatePausedDisk PullState = "paused_disk"
	StateOffline    PullState = "offline"
)

// TaskState is the lifecycle of one asynchronous pull task. Tasks are created
// by Provider.TriggerPull and observed through Snapshot.Tasks.
type TaskState string

const (
	TaskQueued    TaskState = "queued"
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
	TaskCanceled  TaskState = "canceled"
)

// Active reports whether the task still occupies a pull slot.
func (s TaskState) Active() bool {
	return s == TaskQueued || s == TaskRunning
}

// PullTrigger tells who started a pull. Actor carries the concrete user.
type PullTrigger string

const (
	TriggerAuto   PullTrigger = "auto"
	TriggerManual PullTrigger = "manual"
)

// PullPolicy is a typed imagePullPolicy value.
type PullPolicy string

const (
	PullPolicyAlways       PullPolicy = "Always"
	PullPolicyIfNotPresent PullPolicy = "IfNotPresent"
	PullPolicyNever        PullPolicy = "Never"
	PullPolicyUnknown      PullPolicy = "Unknown"
)

// ParsePullPolicy normalizes a raw imagePullPolicy string; empty means the
// Kubernetes default (IfNotPresent for tagged images, Always for :latest).
func ParsePullPolicy(raw string) PullPolicy {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "always":
		return PullPolicyAlways
	case "ifnotpresent":
		return PullPolicyIfNotPresent
	case "never":
		return PullPolicyNever
	case "":
		return PullPolicyIfNotPresent
	default:
		return PullPolicyUnknown
	}
}

// SyncStatus summarizes the last reconciliation with the cluster.
type SyncStatus string

const (
	SyncOK       SyncStatus = "ok"
	SyncDegraded SyncStatus = "degraded"
	SyncOffline  SyncStatus = "offline"
	SyncUnknown  SyncStatus = "unknown"
)

// ImageRef is a parsed container image reference. Image keeps the full
// reference as written in the pod spec; Tag and Digest are extracted parts.
type ImageRef struct {
	Image  string `json:"image"`
	Tag    string `json:"tag,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// ParseImageRef splits a container image reference into tag and digest parts.
func ParseImageRef(image string) ImageRef {
	ref := ImageRef{Image: strings.TrimSpace(image)}
	if i := strings.Index(ref.Image, "@"); i >= 0 {
		ref.Digest = ref.Image[i+1:]
		ref.Image = ref.Image[:i]
	}
	lastSlash := strings.LastIndex(ref.Image, "/")
	if i := strings.LastIndex(ref.Image, ":"); i > lastSlash {
		ref.Tag = ref.Image[i+1:]
	}
	return ref
}

// NodeCondition mirrors the Kubernetes Node condition subset the hub shows.
type NodeCondition struct {
	Type           string    `json:"type"`
	Status         string    `json:"status"` // True | False | Unknown
	Reason         string    `json:"reason,omitempty"`
	Message        string    `json:"message,omitempty"`
	LastTransition time.Time `json:"lastTransition,omitempty"`
}

type PolicyViolation struct {
	Reason  string `json:"reason"` // "latest_tag" | "pull_policy_always"
	Message string `json:"message"`
}

type ImageNodeStatus struct {
	Node        string    `json:"node"`
	State       PullState `json:"state"`
	SizeBytes   int64     `json:"sizeBytes,omitempty"`
	LastChecked time.Time `json:"lastChecked"`
	Error       string    `json:"error,omitempty"`
}

type WorkloadImage struct {
	Image            string            `json:"image"`
	Ref              ImageRef          `json:"ref"`
	ContainerName    string            `json:"containerName"`
	IsInit           bool              `json:"isInit"`
	PullPolicy       PullPolicy        `json:"pullPolicy"`
	PolicyViolations []PolicyViolation `json:"policyViolations,omitempty"`
	Nodes            []ImageNodeStatus `json:"nodes"`
}

type Workload struct {
	Namespace        string          `json:"namespace"`
	Kind             string          `json:"kind"` // Deployment | StatefulSet | DaemonSet
	Name             string          `json:"name"`
	Replicas         int32           `json:"replicas"`
	ImagePullSecrets []string        `json:"imagePullSecrets,omitempty"`
	Images           []WorkloadImage `json:"images"`
}

type CachedImage struct {
	Image     string `json:"image"`
	SizeBytes int64  `json:"sizeBytes"`
}

type NodeInfo struct {
	Name                 string          `json:"name"`
	Ready                bool            `json:"ready"`
	AgentUp              bool            `json:"agentUp"`
	AgentVersion         string          `json:"agentVersion,omitempty"`
	Conditions           []NodeCondition `json:"conditions,omitempty"`
	DiskPaused           bool            `json:"diskPaused"`
	DiskPauseThreshold   float64         `json:"diskPauseThreshold"` // e.g. 0.78
	KubeletGCThreshold   float64         `json:"kubeletGcThreshold"` // e.g. 0.85
	ImageFSUsedBytes     int64           `json:"imageFsUsedBytes"`
	ImageFSCapacityBytes int64           `json:"imageFsCapacityBytes"`
	CachedImageCount     int             `json:"cachedImageCount"`
	ActivePulls          int             `json:"activePulls"`
	CachedImages         []CachedImage   `json:"cachedImages,omitempty"`
	LastSeen             time.Time       `json:"lastSeen"`
}

type PullEvent struct {
	ID         string      `json:"id"`
	Timestamp  time.Time   `json:"timestamp"`
	Node       string      `json:"node"`
	Image      string      `json:"image"`
	Trigger    PullTrigger `json:"trigger"`
	Actor      string      `json:"actor,omitempty"`
	Result     string      `json:"result"` // "pulling" | "success" | "error" | "canceled"
	DurationMs int64       `json:"durationMs,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// PullTask is one asynchronous image pull. The hub renders it in the pull-tasks
// view and the spoke exposes it over /internal/v1/pulls.
type PullTask struct {
	ID         string      `json:"id"`
	Cluster    string      `json:"cluster,omitempty"`
	Image      string      `json:"image"`
	Node       string      `json:"node"`
	State      TaskState   `json:"state"`
	Attempt    int         `json:"attempt"`
	Trigger    PullTrigger `json:"trigger"`
	Actor      string      `json:"actor,omitempty"`
	Error      string      `json:"error,omitempty"`
	CreatedAt  time.Time   `json:"createdAt"`
	StartedAt  *time.Time  `json:"startedAt,omitempty"`
	FinishedAt *time.Time  `json:"finishedAt,omitempty"`
	DurationMs int64       `json:"durationMs,omitempty"`
}

// PullRequest asks a provider to pull one image onto one node.
type PullRequest struct {
	Cluster string      `json:"cluster,omitempty"`
	Image   string      `json:"image"`
	Node    string      `json:"node"`
	Actor   string      `json:"actor,omitempty"`
	Trigger PullTrigger `json:"trigger,omitempty"`
	Policy  PullPolicy  `json:"policy,omitempty"`
}

// WorkloadPullRequest asks a provider to pull every image of one workload.
type WorkloadPullRequest struct {
	Cluster   string      `json:"cluster,omitempty"`
	Namespace string      `json:"namespace"`
	Kind      string      `json:"kind"`
	Name      string      `json:"name"`
	Actor     string      `json:"actor,omitempty"`
	Trigger   PullTrigger `json:"trigger,omitempty"`
}

// Health is the provider liveness view used by /readyz, metrics and the UI.
type Health struct {
	Online    bool      `json:"online"`
	Version   string    `json:"version,omitempty"`
	Error     string    `json:"error,omitempty"`
	LatencyMs int64     `json:"latencyMs,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

type Summary struct {
	TotalWorkloads  int     `json:"totalWorkloads"`
	Deployments     int     `json:"deployments"`
	StatefulSets    int     `json:"statefulSets"`
	DaemonSets      int     `json:"daemonSets"`
	UniqueImages    int     `json:"uniqueImages"`
	ReadyWorkers    int     `json:"readyWorkers"`
	TotalWorkers    int     `json:"totalWorkers"`
	DiskPausedNodes int     `json:"diskPausedNodes"`
	PresentSlots    int     `json:"presentSlots"`
	TotalSlots      int     `json:"totalSlots"`
	CoveragePercent float64 `json:"coveragePercent"`
	ActivePulls     int     `json:"activePulls"`
	QueuedTasks     int     `json:"queuedTasks"`
	RunningTasks    int     `json:"runningTasks"`
	FailedTasks     int     `json:"failedTasks"`
	PolicyWarnings  int     `json:"policyWarnings"`
}

type PullMetricSample struct {
	Image  string `json:"image"`
	Node   string `json:"node"`
	Result string `json:"result"`
	Count  uint64 `json:"count"`
}

type Snapshot struct {
	ClusterName string      `json:"clusterName"`
	APIVersion  string      `json:"apiVersion"`
	MockMode    bool        `json:"mockMode"`
	UpdatedAt   time.Time   `json:"updatedAt"`
	LastSyncAt  time.Time   `json:"lastSyncAt"`
	SyncStatus  SyncStatus  `json:"syncStatus"`
	Partial     bool        `json:"partial"`
	Summary     Summary     `json:"summary"`
	Workloads   []Workload  `json:"workloads"`
	Nodes       []NodeInfo  `json:"nodes"`
	Activity    []PullEvent `json:"activity"`
	Tasks       []PullTask  `json:"tasks,omitempty"`
}
