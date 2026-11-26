package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
)

const (
	miB = int64(1 << 20)
	giB = int64(1 << 30)
)

// mockImageSizes mirrors the demo images of MockProvider so that -mock agent
// output looks like the rest of the product.
var mockImageSizes = map[string]int64{
	"ghcr.io/acme/api-gateway:1.14.2":      142 * miB,
	"registry.internal/fin/payments:2.3.0": 312 * miB,
	"nginx:latest":                         187 * miB,
	"fluent/fluent-bit:3.1.8":              94 * miB,
}

// MockBackend is an in-memory CRI backend used by -mock and by tests. It is
// safe for concurrent use.
type MockBackend struct {
	mu       sync.Mutex
	images   []ImageInfo
	fs       ImageFSStats
	runtime  RuntimeInfo
	state    RuntimeState
	pullSize int64
}

// NewMockBackend builds a fake node with four pre-pulled images and an image
// filesystem at ~58% so that pulls are allowed by default.
func NewMockBackend() *MockBackend {
	m := &MockBackend{
		images: []ImageInfo{
			mockImage("ghcr.io/acme/api-gateway:1.14.2"),
			mockImage("registry.internal/fin/payments:2.3.0"),
			mockImage("nginx:latest"),
			mockImage("fluent/fluent-bit:3.1.8"),
		},
		fs: ImageFSStats{
			Mountpoint:    "/var/lib/containerd",
			UsedBytes:     58 * giB,
			CapacityBytes: 100 * giB,
		},
		runtime:  RuntimeInfo{Name: "containerd", Version: "2.1.3 (mock)"},
		state:    RuntimeState{Ready: true, Conditions: []Condition{{Type: "RuntimeReady", Status: "True"}}},
		pullSize: 64 * miB,
	}
	m.fs.AvailableBytes = m.fs.CapacityBytes - m.fs.UsedBytes
	m.fs.UsedRatio = UsedRatio(m.fs.UsedBytes, m.fs.CapacityBytes)
	return m
}

func mockImage(name string) ImageInfo {
	sum := sha256.Sum256([]byte(name))
	size := mockImageSizes[name]
	if size == 0 {
		size = 64 * miB
	}
	return ImageInfo{
		ID:        fmt.Sprintf("sha256:%x", sum),
		Name:      name,
		Tags:      []string{name},
		SizeBytes: size,
	}
}

// ListImages returns a copy of the cached image list.
func (m *MockBackend) ListImages(_ context.Context) ([]ImageInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ImageInfo, 0, len(m.images))
	for _, img := range m.images {
		out = append(out, cloneImage(img))
	}
	return out, nil
}

// ImageStatus looks up one image by name, tag, digest or ID.
func (m *MockBackend) ImageStatus(_ context.Context, image string) (ImageInfo, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, img := range m.images {
		if imageMatches(img, image) {
			return cloneImage(img), true, nil
		}
	}
	return ImageInfo{}, false, nil
}

// PullImage adds image to the fake cache and grows image filesystem usage.
func (m *MockBackend) PullImage(ctx context.Context, image string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	image = strings.TrimSpace(image)
	if image == "" {
		return "", fmt.Errorf("agent: empty image reference")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, img := range m.images {
		if imageMatches(img, image) {
			return img.ID, nil
		}
	}
	img := mockImage(image)
	if size, ok := mockImageSizes[image]; ok {
		img.SizeBytes = size
	} else {
		img.SizeBytes = m.pullSize
	}
	m.images = append(m.images, img)
	m.fs.UsedBytes += img.SizeBytes
	if m.fs.UsedBytes > m.fs.CapacityBytes {
		m.fs.UsedBytes = m.fs.CapacityBytes
	}
	m.fs.AvailableBytes = m.fs.CapacityBytes - m.fs.UsedBytes
	m.fs.UsedRatio = UsedRatio(m.fs.UsedBytes, m.fs.CapacityBytes)
	return img.ID, nil
}

// ImageFS returns the fake image filesystem usage.
func (m *MockBackend) ImageFS(_ context.Context) (ImageFSStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fs, nil
}

// RuntimeState returns the fake runtime readiness.
func (m *MockBackend) RuntimeState(_ context.Context) (RuntimeState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, nil
}

// RuntimeVersion returns the fake runtime version.
func (m *MockBackend) RuntimeVersion(_ context.Context) (RuntimeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime, nil
}

// SetImageFS replaces the fake image filesystem, used by tests and demos that
// need the disk guard engaged.
func (m *MockBackend) SetImageFS(stats ImageFSStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats.UsedRatio = UsedRatio(stats.UsedBytes, stats.CapacityBytes)
	if stats.AvailableBytes == 0 && stats.CapacityBytes >= stats.UsedBytes {
		stats.AvailableBytes = stats.CapacityBytes - stats.UsedBytes
	}
	m.fs = stats
}

func imageMatches(img ImageInfo, ref string) bool {
	if ref == "" {
		return false
	}
	if img.Name == ref || img.ID == ref {
		return true
	}
	for _, tag := range img.Tags {
		if tag == ref {
			return true
		}
	}
	for _, digest := range img.Digests {
		if digest == ref {
			return true
		}
	}
	return false
}
