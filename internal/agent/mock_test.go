package agent

import (
	"context"
	"testing"
)

func TestMockBackendInitialState(t *testing.T) {
	m := NewMockBackend()
	images, err := m.ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 4 {
		t.Fatalf("images = %d, want 4", len(images))
	}
	fs, err := m.ImageFS(context.Background())
	if err != nil {
		t.Fatalf("ImageFS: %v", err)
	}
	if fs.CapacityBytes <= 0 || fs.UsedBytes <= 0 || fs.UsedBytes >= fs.CapacityBytes {
		t.Fatalf("unexpected image fs: %+v", fs)
	}
	if fs.UsedRatio < 0.5 || fs.UsedRatio >= 0.78 {
		t.Fatalf("mock image fs ratio = %v, want a value below the pause threshold", fs.UsedRatio)
	}
	state, err := m.RuntimeState(context.Background())
	if err != nil || !state.Ready {
		t.Fatalf("RuntimeState = %+v, %v; want ready", state, err)
	}
	info, err := m.RuntimeVersion(context.Background())
	if err != nil || info.Name != "containerd" {
		t.Fatalf("RuntimeVersion = %+v, %v", info, err)
	}
}

func TestMockBackendImageStatus(t *testing.T) {
	m := NewMockBackend()
	img, ok, err := m.ImageStatus(context.Background(), "ghcr.io/acme/api-gateway:1.14.2")
	if err != nil || !ok {
		t.Fatalf("ImageStatus(known) = %+v, %v, %v", img, ok, err)
	}
	if img.SizeBytes != 142*miB {
		t.Fatalf("size = %d, want %d", img.SizeBytes, 142*miB)
	}
	if _, ok, err := m.ImageStatus(context.Background(), "missing:1"); err != nil || ok {
		t.Fatalf("ImageStatus(missing) = ok=%v, err=%v; want false, nil", ok, err)
	}
}

func TestMockBackendPullIsIdempotent(t *testing.T) {
	m := NewMockBackend()
	before, _ := m.ImageFS(context.Background())
	ref1, err := m.PullImage(context.Background(), "registry.internal/new:1.0.0")
	if err != nil || ref1 == "" {
		t.Fatalf("PullImage = %q, %v", ref1, err)
	}
	images, _ := m.ListImages(context.Background())
	if len(images) != 5 {
		t.Fatalf("images after pull = %d, want 5", len(images))
	}
	after, _ := m.ImageFS(context.Background())
	if after.UsedBytes <= before.UsedBytes {
		t.Fatalf("image fs usage did not grow: before=%d after=%d", before.UsedBytes, after.UsedBytes)
	}

	ref2, err := m.PullImage(context.Background(), "registry.internal/new:1.0.0")
	if err != nil || ref2 != ref1 {
		t.Fatalf("second pull = %q, %v; want %q", ref2, err, ref1)
	}
	images, _ = m.ListImages(context.Background())
	if len(images) != 5 {
		t.Fatalf("idempotent pull added an image: %d", len(images))
	}
}

func TestMockBackendPullHonorsContext(t *testing.T) {
	m := NewMockBackend()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.PullImage(ctx, "nginx:latest"); err == nil {
		t.Fatal("PullImage with canceled context = nil error")
	}
}

func TestAgentRunMockWithoutCRISocket(t *testing.T) {
	// The whole point of -mock: no containerd socket path is required.
	a, err := New(Config{NodeName: "talos-worker-01", ListenAddr: "127.0.0.1:0", DiskPauseRatio: 0.78, Mock: true})
	if err != nil {
		t.Fatalf("New(mock without socket): %v", err)
	}
	if a.Config().Mock != true {
		t.Fatal("Config().Mock = false")
	}
	if _, err := New(Config{NodeName: "talos-worker-01", ListenAddr: "127.0.0.1:0", DiskPauseRatio: 0.78}); err == nil {
		t.Fatal("New(real without socket) = nil error")
	}
}
