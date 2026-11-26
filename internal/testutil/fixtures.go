package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/local/image-prepuller/internal/model"
)

// GoldenSnapshot loads testdata/snapshot_v2.json: a complete contract-v2
// snapshot used by UI, metrics and provider tests as a stable fixture.
func GoldenSnapshot(t testing.TB) model.Snapshot {
	t.Helper()
	path := filepath.Join("testdata", "snapshot_v2.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testutil: read golden snapshot %s: %v", path, err)
	}
	var snap model.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("testutil: decode golden snapshot: %v", err)
	}
	return snap
}

// GoldenSnapshotJSON returns the raw golden snapshot bytes (for golden tests).
func GoldenSnapshotJSON(t testing.TB) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "snapshot_v2.json"))
	if err != nil {
		t.Fatalf("testutil: read golden snapshot: %v", err)
	}
	return raw
}

// RequireContractV2 asserts that a snapshot carries every contract-v2 field
// that WS-A..WS-H are allowed to rely on.
func RequireContractV2(t testing.TB, snap model.Snapshot) {
	t.Helper()
	if snap.APIVersion != model.APIVersion {
		t.Errorf("snapshot apiVersion = %q, want %q", snap.APIVersion, model.APIVersion)
	}
	if snap.SyncStatus == "" {
		t.Error("snapshot syncStatus is empty")
	}
	if snap.LastSyncAt.IsZero() {
		t.Error("snapshot lastSyncAt is zero")
	}
	if len(snap.Nodes) == 0 {
		t.Fatal("snapshot has no nodes")
	}
	if snap.Nodes[0].AgentVersion == "" {
		t.Error("node agentVersion is empty")
	}
	if len(snap.Nodes[0].Conditions) == 0 {
		t.Error("node conditions are empty")
	}
	if len(snap.Workloads) == 0 {
		t.Fatal("snapshot has no workloads")
	}
	img := snap.Workloads[0].Images[0]
	if img.Ref.Image == "" {
		t.Error("workload image ref.image is empty")
	}
	switch img.PullPolicy {
	case model.PullPolicyAlways, model.PullPolicyIfNotPresent, model.PullPolicyNever:
	default:
		t.Errorf("workload image pullPolicy = %q, want a typed policy", img.PullPolicy)
	}
}

// FixedTime is a stable timestamp for fixtures (avoids flaky golden diffs).
var FixedTime = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
