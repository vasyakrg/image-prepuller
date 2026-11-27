package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// timeAfter is a tiny helper so tests can fail fast without importing time
// everywhere.
func timeAfter(t *testing.T, seconds int) <-chan time.Time {
	t.Helper()
	return time.After(time.Duration(seconds) * time.Second)
}

// fakeCRI wraps MockBackend and lets tests inject CRI failures. It is the only
// fake the agent needs: no containerd, no network.
type fakeCRI struct {
	*MockBackend

	mu         sync.Mutex
	listErr    error
	fsErr      error
	stateErr   error
	runtimeErr error
	statusErr  error
	pullErr    error
	pulls      []string
	pullAuths  []*RegistryAuth

	// pullHook, when set, replaces the mock pull so a test can simulate a hung
	// or stalled CRI call. fsHook does the same for ImageFS sampling.
	pullHook func(ctx context.Context, image string) (string, error)
	fsHook   func(ctx context.Context) (ImageFSStats, error)
}

func newFakeCRI() *fakeCRI { return &fakeCRI{MockBackend: NewMockBackend()} }

func (f *fakeCRI) ListImages(ctx context.Context) ([]ImageInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.MockBackend.ListImages(ctx)
}

func (f *fakeCRI) ImageStatus(ctx context.Context, image string) (ImageInfo, bool, error) {
	if f.statusErr != nil {
		return ImageInfo{}, false, f.statusErr
	}
	return f.MockBackend.ImageStatus(ctx, image)
}

func (f *fakeCRI) PullImage(ctx context.Context, image string, auth *RegistryAuth) (string, error) {
	f.mu.Lock()
	f.pulls = append(f.pulls, image)
	f.pullAuths = append(f.pullAuths, auth)
	f.mu.Unlock()
	if f.pullHook != nil {
		return f.pullHook(ctx, image)
	}
	if f.pullErr != nil {
		return "", f.pullErr
	}
	return f.MockBackend.PullImage(ctx, image, auth)
}

func (f *fakeCRI) ImageFS(ctx context.Context) (ImageFSStats, error) {
	if f.fsHook != nil {
		return f.fsHook(ctx)
	}
	if f.fsErr != nil {
		return ImageFSStats{}, f.fsErr
	}
	return f.MockBackend.ImageFS(ctx)
}

func (f *fakeCRI) RuntimeState(ctx context.Context) (RuntimeState, error) {
	if f.stateErr != nil {
		return RuntimeState{}, f.stateErr
	}
	return f.MockBackend.RuntimeState(ctx)
}

func (f *fakeCRI) RuntimeVersion(ctx context.Context) (RuntimeInfo, error) {
	if f.runtimeErr != nil {
		return RuntimeInfo{}, f.runtimeErr
	}
	return f.MockBackend.RuntimeVersion(ctx)
}

func (f *fakeCRI) pullCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pulls...)
}

// pullAuthsSeen returns the credentials the server forwarded to CRI, one entry
// per pull call (nil for an anonymous pull).
func (f *fakeCRI) pullAuthsSeen() []*RegistryAuth {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*RegistryAuth(nil), f.pullAuths...)
}

func testConfig() Config {
	return Config{
		NodeName:       "talos-worker-01",
		ListenAddr:     "127.0.0.1:0",
		DiskPauseRatio: 0.78,
		// The fixture images live on registry.internal, which does not resolve;
		// tests that exercise the preflight probe opt back in explicitly.
		DisableRegistryProbe: true,
	}
}
