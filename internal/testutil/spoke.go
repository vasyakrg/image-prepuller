// Package testutil provides shared fakes and fixtures for contract tests
// across workstreams: a fake spoke, a fake hub, golden snapshots and helpers.
// It is imported by tests only.
package testutil

import (
	"net/http/httptest"
	"testing"

	"github.com/local/image-prepuller/internal/auth"
	"github.com/local/image-prepuller/internal/provider"
	"github.com/local/image-prepuller/internal/web"
)

// TestClusterToken is the Bearer token used by FakeSpoke.
const TestClusterToken = "test-spoke-token"

// TestHubUser and TestHubPass are the credentials of FakeHub.
const (
	TestHubUser = "admin"
	TestHubPass = "test-pass"
)

// FakeSpoke is an httptest spoke backed by MockProvider. Use it to test the
// hub↔spoke wire contract without Kubernetes or network access.
type FakeSpoke struct {
	Cluster  string
	Provider *provider.MockProvider
	Server   *httptest.Server
	Token    string
}

// NewFakeSpoke starts a spoke server and closes it via t.Cleanup.
func NewFakeSpoke(t testing.TB, clusterName string) *FakeSpoke {
	t.Helper()
	mock := provider.NewMockProvider(clusterName)
	srv := httptest.NewServer(web.NewSpokeServer(mock, TestClusterToken))
	t.Cleanup(srv.Close)
	return &FakeSpoke{Cluster: clusterName, Provider: mock, Server: srv, Token: TestClusterToken}
}

// URL is the base URL of the fake spoke.
func (f *FakeSpoke) URL() string { return f.Server.URL }

// Remote builds a RemoteProvider pointed at this spoke.
func (f *FakeSpoke) Remote() *provider.RemoteProvider {
	return provider.NewRemoteProvider(f.Cluster, f.URL(), f.Token)
}

// FakeHub is an httptest hub with cookie auth and an empty registry.
type FakeHub struct {
	Server   *web.Server
	Registry *provider.Registry
	Auth     *auth.Manager
	Server2  *httptest.Server
	Cookie   string
}

// NewFakeHub starts a hub with the given local mock clusters registered.
func NewFakeHub(t testing.TB, clusterIDs ...string) *FakeHub {
	t.Helper()
	mgr, err := auth.NewManager(auth.Config{
		Username:      TestHubUser,
		Password:      TestHubPass,
		SessionSecret: []byte("testutil-session-secret"),
	})
	if err != nil {
		t.Fatalf("testutil: auth manager: %v", err)
	}
	reg := provider.NewRegistry()
	for _, id := range clusterIDs {
		reg.Register(provider.ClusterEntry{ID: id, Name: id, Role: "local", Provider: provider.NewMockProvider(id)})
	}
	srv, err := web.NewServer(reg, mgr)
	if err != nil {
		t.Fatalf("testutil: web server: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &FakeHub{Server: srv, Registry: reg, Auth: mgr, Server2: ts}
}

// RegisterRemote adds a remote cluster backed by the given fake spoke.
func (h *FakeHub) RegisterRemote(f *FakeSpoke) {
	h.Registry.Register(provider.ClusterEntry{
		ID:       f.Cluster,
		Name:     f.Cluster,
		Role:     "remote",
		Endpoint: f.URL(),
		Provider: f.Remote(),
	})
}
