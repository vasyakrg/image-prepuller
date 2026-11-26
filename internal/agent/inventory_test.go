package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestShouldPause(t *testing.T) {
	tests := []struct {
		name      string
		used      int64
		capacity  int64
		threshold float64
		want      bool
	}{
		{name: "below threshold", used: 770, capacity: 1000, threshold: 0.78, want: false},
		{name: "at threshold pauses", used: 780, capacity: 1000, threshold: 0.78, want: true},
		{name: "above threshold", used: 900, capacity: 1000, threshold: 0.78, want: true},
		{name: "used above capacity", used: 1200, capacity: 1000, threshold: 0.78, want: true},
		{name: "unknown capacity never pauses", used: 900, capacity: 0, threshold: 0.78, want: false},
		{name: "negative used never pauses", used: -10, capacity: 1000, threshold: 0.78, want: false},
		{name: "zero threshold is misconfig", used: 900, capacity: 1000, threshold: 0, want: false},
		{name: "threshold one is misconfig", used: 900, capacity: 1000, threshold: 1, want: false},
		{name: "custom threshold", used: 50, capacity: 100, threshold: 0.5, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldPause(tc.used, tc.capacity, tc.threshold); got != tc.want {
				t.Fatalf("ShouldPause(%d, %d, %v) = %v, want %v", tc.used, tc.capacity, tc.threshold, got, tc.want)
			}
		})
	}
}

func TestUsedRatio(t *testing.T) {
	tests := []struct {
		used, capacity int64
		want           float64
	}{
		{used: 0, capacity: 0, want: 0},
		{used: 50, capacity: 100, want: 0.5},
		{used: 100, capacity: 100, want: 1},
		{used: 150, capacity: 100, want: 1},
		{used: -5, capacity: 100, want: 0},
	}
	for _, tc := range tests {
		if got := UsedRatio(tc.used, tc.capacity); got != tc.want {
			t.Fatalf("UsedRatio(%d, %d) = %v, want %v", tc.used, tc.capacity, got, tc.want)
		}
	}
}

func TestSelectImages(t *testing.T) {
	all := []ImageInfo{
		{ID: "id-a", Tags: []string{"registry/a:1"}, SizeBytes: 10},
		{ID: "id-b", Digests: []string{"registry/b@sha256:abc"}, SizeBytes: 30},
		{ID: "id-c", SizeBytes: 20},
		{ID: "id-a", Tags: []string{"registry/a:dup"}, SizeBytes: 99},
		{ID: "", SizeBytes: 5},
	}
	got := SelectImages(all, 0)
	if len(got) != 3 {
		t.Fatalf("SelectImages returned %d images, want 3: %+v", len(got), got)
	}
	if got[0].ID != "id-b" || got[1].ID != "id-c" || got[2].ID != "id-a" {
		t.Fatalf("unexpected order: %+v", got)
	}
	if got[0].Name != "registry/b@sha256:abc" {
		t.Fatalf("digest should be the primary name, got %q", got[0].Name)
	}
	if got[1].Name != "id-c" {
		t.Fatalf("ID should be the fallback name, got %q", got[1].Name)
	}
	if got[2].Name != "registry/a:1" {
		t.Fatalf("first tag should win the primary name, got %q", got[2].Name)
	}
	if all[0].Name != "" {
		t.Fatalf("SelectImages mutated the input: %+v", all[0])
	}

	limited := SelectImages(all, 2)
	if len(limited) != 2 || limited[0].ID != "id-b" {
		t.Fatalf("limit not applied: %+v", limited)
	}
}

func TestBuildSnapshot(t *testing.T) {
	f := newFakeCRI()
	f.SetImageFS(ImageFSStats{UsedBytes: 80 * giB, CapacityBytes: 100 * giB, Mountpoint: "/var/lib/containerd"})
	cfg := testConfig()

	snap, err := BuildSnapshot(context.Background(), f, cfg)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if snap.APIVersion != WireAPIVersion {
		t.Errorf("apiVersion = %q, want %q", snap.APIVersion, WireAPIVersion)
	}
	if snap.NodeName != cfg.NodeName {
		t.Errorf("node = %q, want %q", snap.NodeName, cfg.NodeName)
	}
	if snap.AgentVersion != Version {
		t.Errorf("agentVersion = %q, want %q", snap.AgentVersion, Version)
	}
	if !snap.DiskPaused {
		t.Error("diskPaused = false at 80% usage, want true")
	}
	if snap.ImageFS.UsedRatio != 0.8 {
		t.Errorf("usedRatio = %v, want 0.8", snap.ImageFS.UsedRatio)
	}
	if !snap.Ready {
		t.Error("ready = false, want true")
	}
	if snap.Runtime.Name != "containerd" {
		t.Errorf("runtime = %+v, want containerd", snap.Runtime)
	}
	if snap.CachedImageCount != 4 || len(snap.Images) != 4 {
		t.Errorf("image count = %d/%d, want 4", snap.CachedImageCount, len(snap.Images))
	}
	var total int64
	for _, img := range snap.Images {
		total += img.SizeBytes
	}
	if total != snap.TotalImageBytes || total == 0 {
		t.Errorf("totalImageBytes = %d, want %d", snap.TotalImageBytes, total)
	}
	if snap.AuthEnabled {
		t.Error("authEnabled = true without a token")
	}
}

func TestBuildSnapshotAuthEnabled(t *testing.T) {
	cfg := testConfig()
	cfg.Token = "secret"
	snap, err := BuildSnapshot(context.Background(), newFakeCRI(), cfg)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	if !snap.AuthEnabled {
		t.Error("authEnabled = false with a token configured")
	}
}

func TestBuildSnapshotErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name   string
		mutate func(*fakeCRI)
		want   string
	}{
		{name: "list images", mutate: func(f *fakeCRI) { f.listErr = boom }, want: "list images"},
		{name: "image fs", mutate: func(f *fakeCRI) { f.fsErr = boom }, want: "image filesystem"},
		{name: "runtime state", mutate: func(f *fakeCRI) { f.stateErr = boom }, want: "runtime status"},
		{name: "runtime version", mutate: func(f *fakeCRI) { f.runtimeErr = boom }, want: "runtime version"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCRI()
			tc.mutate(f)
			_, err := BuildSnapshot(context.Background(), f, testConfig())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildSnapshot error = %v, want substring %q", err, tc.want)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("error %v does not wrap the CRI error", err)
			}
		})
	}
}

func TestNewValidatesConfig(t *testing.T) {
	valid := testConfig()
	valid.Mock = true
	if _, err := New(valid); err != nil {
		t.Fatalf("New(valid) = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing node name", mutate: func(c *Config) { c.NodeName = "" }},
		{name: "missing cri socket", mutate: func(c *Config) { c.Mock = false; c.CRISocket = "" }},
		{name: "missing listen addr", mutate: func(c *Config) { c.ListenAddr = "" }},
		{name: "zero disk ratio", mutate: func(c *Config) { c.DiskPauseRatio = 0 }},
		{name: "one disk ratio", mutate: func(c *Config) { c.DiskPauseRatio = 1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New(%+v) = nil error, want validation error", cfg)
			}
		})
	}
}

func TestAgentRunMockShutsDownOnCancel(t *testing.T) {
	a, err := New(Config{NodeName: "talos-worker-01", ListenAddr: "127.0.0.1:0", DiskPauseRatio: 0.78, Mock: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	// Serve binds 127.0.0.1:0 immediately; cancel right after start.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancel = %v, want nil", err)
		}
	case <-timeAfter(t, 5):
		t.Fatal("Run did not return after context cancellation")
	}
}
