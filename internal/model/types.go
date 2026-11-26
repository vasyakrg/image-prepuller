package model

import "time"

type PullState string

const (
	StatePresent    PullState = "present"
	StatePulling    PullState = "pulling"
	StateMissing    PullState = "missing"
	StateFailed     PullState = "failed"
	StatePausedDisk PullState = "paused_disk"
	StateOffline    PullState = "offline"
)

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
	ContainerName    string            `json:"containerName"`
	IsInit           bool              `json:"isInit"`
	PullPolicy       string            `json:"pullPolicy"`
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
	Name                 string        `json:"name"`
	Ready                bool          `json:"ready"`
	AgentUp              bool          `json:"agentUp"`
	DiskPaused           bool          `json:"diskPaused"`
	DiskPauseThreshold   float64       `json:"diskPauseThreshold"` // e.g. 0.78
	KubeletGCThreshold   float64       `json:"kubeletGcThreshold"` // e.g. 0.85
	ImageFSUsedBytes     int64         `json:"imageFsUsedBytes"`
	ImageFSCapacityBytes int64         `json:"imageFsCapacityBytes"`
	CachedImageCount     int           `json:"cachedImageCount"`
	ActivePulls          int           `json:"activePulls"`
	CachedImages         []CachedImage `json:"cachedImages,omitempty"`
	LastSeen             time.Time     `json:"lastSeen"`
}

type PullEvent struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Node       string    `json:"node"`
	Image      string    `json:"image"`
	Trigger    string    `json:"trigger"` // "auto" | "manual (admin)"
	Result     string    `json:"result"`  // "pulling" | "success" | "error"
	DurationMs int64     `json:"durationMs,omitempty"`
	Error      string    `json:"error,omitempty"`
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
	PolicyWarnings  int     `json:"policyWarnings"`
}

type PullMetricSample struct {
	Image  string
	Node   string
	Result string
	Count  uint64
}

type Snapshot struct {
	ClusterName string      `json:"clusterName"`
	MockMode    bool        `json:"mockMode"`
	UpdatedAt   time.Time   `json:"updatedAt"`
	Summary     Summary     `json:"summary"`
	Workloads   []Workload  `json:"workloads"`
	Nodes       []NodeInfo  `json:"nodes"`
	Activity    []PullEvent `json:"activity"`
}
