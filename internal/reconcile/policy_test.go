package reconcile

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/local/image-prepuller/internal/model"
)

func TestRunOnceRules(t *testing.T) {
	offlineNode := readyNode("n2")
	offlineNode.Ready = false
	offlineNode.AgentUp = false

	agentDownNode := readyNode("n2")
	agentDownNode.AgentUp = false

	diskPausedNode := readyNode("n2")
	diskPausedNode.DiskPaused = true

	overThreshold := readyNode("n2")
	overThreshold.DiskPauseThreshold = 0 // fall back to Config.DiskPauseThreshold
	overThreshold.ImageFSUsedBytes = 80
	overThreshold.ImageFSCapacityBytes = 100

	ownThreshold := readyNode("n3")
	ownThreshold.DiskPauseThreshold = 0.5
	ownThreshold.ImageFSUsedBytes = 60
	ownThreshold.ImageFSCapacityBytes = 100

	policyImage := testImage("nginx:latest", model.PullPolicyAlways, testSlot("n1", model.StateMissing))
	policyImage.PolicyViolations = []model.PolicyViolation{
		{Reason: "latest_tag", Message: "no immutable tag"},
		{Reason: "pull_policy_always", Message: "Always defeats offline failover"},
	}

	cases := []struct {
		name         string
		cfg          Config
		build        func() model.Snapshot
		wantPlan     []string
		wantSkipped  map[SkipReason]int
		wantExamined int
	}{
		{
			name: "missing slot on ready node is planned",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1"), readyNode("n2")},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StatePresent),
							testSlot("n2", model.StateMissing)))},
				)
			},
			wantPlan:     []string{testImageA + "|n2"},
			wantSkipped:  map[SkipReason]int{SkipPresent: 1},
			wantExamined: 2,
		},
		{
			name: "offline node is skipped",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1"), offlineNode},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing),
							testSlot("n2", model.StateMissing)))},
				)
			},
			wantPlan:     []string{testImageA + "|n1"},
			wantSkipped:  map[SkipReason]int{SkipOfflineNode: 1},
			wantExamined: 2,
		},
		{
			name: "node with down agent is skipped",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1"), agentDownNode},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing),
							testSlot("n2", model.StateMissing)))},
				)
			},
			wantPlan:     []string{testImageA + "|n1"},
			wantSkipped:  map[SkipReason]int{SkipOfflineNode: 1},
			wantExamined: 2,
		},
		{
			name: "slot pointing at an unknown node is skipped as offline",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing),
							testSlot("ghost", model.StateMissing)))},
				)
			},
			wantPlan:     []string{testImageA + "|n1"},
			wantSkipped:  map[SkipReason]int{SkipOfflineNode: 1},
			wantExamined: 2,
		},
		{
			name: "disk-paused flag is skipped",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1"), diskPausedNode},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing),
							testSlot("n2", model.StateMissing)))},
				)
			},
			wantPlan:     []string{testImageA + "|n1"},
			wantSkipped:  map[SkipReason]int{SkipDiskPaused: 1},
			wantExamined: 2,
		},
		{
			name: "disk threshold uses node value then config fallback",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1"), overThreshold, ownThreshold},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing),
							testSlot("n2", model.StateMissing),
							testSlot("n3", model.StateMissing)))},
				)
			},
			wantPlan:     []string{testImageA + "|n1"},
			wantSkipped:  map[SkipReason]int{SkipDiskPaused: 2},
			wantExamined: 3,
		},
		{
			name: "pulling slot is skipped",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StatePulling)))},
				)
			},
			wantSkipped:  map[SkipReason]int{SkipPulling: 1},
			wantExamined: 1,
		},
		{
			name: "offline and paused_disk slot states are skipped",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateOffline)),
						testImage(testImageB, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StatePausedDisk)))},
				)
			},
			wantSkipped:  map[SkipReason]int{SkipOfflineNode: 1, SkipDiskPaused: 1},
			wantExamined: 2,
		},
		{
			name: "active task for the pair is skipped",
			build: func() model.Snapshot {
				snap := testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing)))},
				)
				snap.Tasks = []model.PullTask{{Image: testImageA, Node: "n1", State: model.TaskQueued}}
				return snap
			},
			wantSkipped:  map[SkipReason]int{SkipActiveTask: 1},
			wantExamined: 1,
		},
		{
			name: "failed slots are planned before missing ones",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{
						testWorkload("prod", "missing", testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing))),
						testWorkload("prod", "failed", testImage(testImageB, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateFailed))),
					},
				)
			},
			wantPlan:     []string{testImageB + "|n1", testImageA + "|n1"},
			wantSkipped:  map[SkipReason]int{},
			wantExamined: 2,
		},
		{
			name: "namespace priority breaks ties between equal slot states",
			cfg:  Config{MaxConcurrentPerNode: 10, MaxConcurrentTotal: 10},
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{
						testWorkload("infra", "sidecar", testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing))),
						testWorkload("staging", "worker", testImage(testImageC, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing))),
						testWorkload("prod", "api", testImage(testImageB, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing))),
					},
				)
			},
			wantPlan:     []string{testImageB + "|n1", testImageC + "|n1", testImageA + "|n1"},
			wantSkipped:  map[SkipReason]int{},
			wantExamined: 3,
		},
		{
			name: "policy violations are skipped by default",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{testWorkload("prod", "legacy", policyImage)},
				)
			},
			wantSkipped:  map[SkipReason]int{SkipPolicyViolation: 1},
			wantExamined: 1,
		},
		{
			name: "policy violations can be allowed explicitly",
			cfg:  Config{AllowPolicyViolations: true},
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{testWorkload("prod", "legacy", policyImage)},
				)
			},
			wantPlan:     []string{"nginx:latest|n1"},
			wantSkipped:  map[SkipReason]int{},
			wantExamined: 1,
		},
		{
			name: "empty slot state is treated as missing",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent, testSlot("n1", "")))},
				)
			},
			wantPlan:     []string{testImageA + "|n1"},
			wantSkipped:  map[SkipReason]int{},
			wantExamined: 1,
		},
		{
			name: "cluster offline skips the whole run",
			build: func() model.Snapshot {
				snap := testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{testWorkload("prod", "api",
						testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing)))},
				)
				snap.SyncStatus = model.SyncOffline
				return snap
			},
			wantSkipped:  map[SkipReason]int{SkipClusterOffline: 1},
			wantExamined: 0,
		},
		{
			name: "empty snapshot is skipped as cluster offline",
			build: func() model.Snapshot {
				return testSnapshot(nil, nil)
			},
			wantSkipped:  map[SkipReason]int{SkipClusterOffline: 1},
			wantExamined: 0,
		},
		{
			name: "duplicate image+node pair is planned once",
			build: func() model.Snapshot {
				return testSnapshot(
					[]model.NodeInfo{readyNode("n1")},
					[]model.Workload{
						testWorkload("prod", "api", testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing))),
						testWorkload("prod", "api-canary", testImage(testImageA, model.PullPolicyIfNotPresent,
							testSlot("n1", model.StateMissing))),
					},
				)
			},
			wantPlan:     []string{testImageA + "|n1"},
			wantSkipped:  map[SkipReason]int{SkipDuplicate: 1},
			wantExamined: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeProvider(tc.build())
			rec := New(tc.cfg)

			res, err := rec.RunOnce(context.Background(), fake, "test")
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if got := plannedPairs(res); !slices.Equal(got, tc.wantPlan) {
				t.Fatalf("planned = %v, want %v", got, tc.wantPlan)
			}
			if !maps.Equal(res.Skipped, tc.wantSkipped) {
				t.Fatalf("skipped = %v, want %v", res.Skipped, tc.wantSkipped)
			}
			if res.Examined != tc.wantExamined {
				t.Fatalf("examined = %d, want %d", res.Examined, tc.wantExamined)
			}
			if len(res.Triggered) != len(tc.wantPlan) {
				t.Fatalf("triggered = %d, want %d", len(res.Triggered), len(tc.wantPlan))
			}
			if got := len(fake.triggers()); got != len(tc.wantPlan) {
				t.Fatalf("provider TriggerPull calls = %d, want %d", got, len(tc.wantPlan))
			}
		})
	}
}

func TestConcurrencyLimits(t *testing.T) {
	threeImages := func() model.Snapshot {
		return testSnapshot(
			[]model.NodeInfo{readyNode("n1")},
			[]model.Workload{testWorkload("prod", "api",
				testImage(testImageA, model.PullPolicyIfNotPresent, testSlot("n1", model.StateMissing)),
				testImage(testImageB, model.PullPolicyIfNotPresent, testSlot("n1", model.StateMissing)),
				testImage(testImageC, model.PullPolicyIfNotPresent, testSlot("n1", model.StateMissing)),
			)},
		)
	}
	twoNodes := func() model.Snapshot {
		return testSnapshot(
			[]model.NodeInfo{readyNode("n1"), readyNode("n2")},
			[]model.Workload{testWorkload("prod", "api",
				testImage(testImageA, model.PullPolicyIfNotPresent,
					testSlot("n1", model.StateMissing),
					testSlot("n2", model.StateMissing)))},
		)
	}

	cases := []struct {
		name        string
		cfg         Config
		build       func() model.Snapshot
		wantPlan    []string
		wantSkipped int // SkipConcurrencyLimit count
	}{
		{
			name: "per-node limit accounts for ActivePulls",
			cfg:  Config{MaxConcurrentPerNode: 2, MaxConcurrentTotal: 10},
			build: func() model.Snapshot {
				snap := threeImages()
				snap.Nodes[0].ActivePulls = 1
				return snap
			},
			wantPlan: []string{testImageA + "|n1"},
			// n1 has one active pull already, so only one of three fits.
			wantSkipped: 2,
		},
		{
			name: "per-node limit accounts for active tasks",
			cfg:  Config{MaxConcurrentPerNode: 1, MaxConcurrentTotal: 10},
			build: func() model.Snapshot {
				snap := threeImages()
				snap.Tasks = []model.PullTask{{Image: "other:1", Node: "n1", State: model.TaskRunning}}
				return snap
			},
			wantPlan:    nil,
			wantSkipped: 3,
		},
		{
			name:        "total limit across nodes",
			cfg:         Config{MaxConcurrentPerNode: 10, MaxConcurrentTotal: 1},
			build:       twoNodes,
			wantPlan:    []string{testImageA + "|n1"},
			wantSkipped: 1,
		},
		{
			name: "total limit is consumed by active tasks",
			cfg:  Config{MaxConcurrentPerNode: 10, MaxConcurrentTotal: 2},
			build: func() model.Snapshot {
				snap := twoNodes()
				snap.Tasks = []model.PullTask{
					{Image: "other:1", Node: "n1", State: model.TaskRunning},
					{Image: "other:2", Node: "n2", State: model.TaskQueued},
				}
				return snap
			},
			wantPlan:    nil,
			wantSkipped: 2,
		},
		{
			name:        "defaults allow two per node and four in total",
			cfg:         Config{},
			build:       threeImages,
			wantPlan:    []string{testImageA + "|n1", testImageC + "|n1"},
			wantSkipped: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeProvider(tc.build())
			rec := New(tc.cfg)
			res, err := rec.RunOnce(context.Background(), fake, "test")
			if err != nil {
				t.Fatalf("RunOnce() error = %v", err)
			}
			if got := plannedPairs(res); !slices.Equal(got, tc.wantPlan) {
				t.Fatalf("planned = %v, want %v", got, tc.wantPlan)
			}
			if got := res.Skipped[SkipConcurrencyLimit]; got != tc.wantSkipped {
				t.Fatalf("concurrency_limit = %d, want %d (all skips: %v)", got, tc.wantSkipped, res.Skipped)
			}
		})
	}
}
