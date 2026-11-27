package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestClassifyAgentError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "unauthorized sentinel", err: fmt.Errorf("inventory: %w", ErrAgentUnauthorized), want: ReasonUnauthorized},
		{
			name: "unauthorized through the transport wrapper",
			err:  fmt.Errorf("%w: %w: node %q: http 401: unauthorized", ErrNodeInventoryUnavailable, ErrAgentUnauthorized, "worker-01"),
			want: ReasonUnauthorized,
		},
		{
			name: "deadline exceeded through the transport wrapper",
			err: fmt.Errorf("%w: %w: node %q: %w",
				ErrNodeInventoryUnavailable, ErrAgentUnavailable, "worker-01", context.DeadlineExceeded),
			want: ReasonTimeout,
		},
		{
			name: "canceled",
			err:  fmt.Errorf("%w: %w", ErrNodeInventoryUnavailable, context.Canceled),
			want: ReasonTimeout,
		},
		{
			name: "unsupported api version",
			err: fmt.Errorf("%w: %w: node %q reports apiVersion %q, want %q",
				ErrNodeInventoryUnavailable, ErrAgentUnavailable, "worker-01", "v9", agentAPIVersion),
			want: ReasonProtocol,
		},
		{
			name: "agent answered for another node",
			err: fmt.Errorf("%w: %w: node %q answered for node %q",
				ErrNodeInventoryUnavailable, ErrAgentUnavailable, "worker-01", "worker-99"),
			want: ReasonProtocol,
		},
		{
			name: "malformed body",
			err: fmt.Errorf("%w: %w: node %q: decode %s: invalid character",
				ErrNodeInventoryUnavailable, ErrAgentUnavailable, "worker-01", agentSnapshotPath),
			want: ReasonProtocol,
		},
		{
			name: "agent returned a server error",
			err:  fmt.Errorf("%w: %w: node %q: http 500: snapshot failed", ErrNodeInventoryUnavailable, ErrAgentUnavailable, "worker-01"),
			want: ReasonUnavailable,
		},
		{
			name: "connection refused",
			err:  fmt.Errorf("%w: %w: node %q: dial tcp: connection refused", ErrNodeInventoryUnavailable, ErrAgentUnavailable, "worker-01"),
			want: ReasonUnavailable,
		},
		{
			name: "unknown error",
			err:  errors.New("boom"),
			want: ReasonUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyAgentError(tt.err); got != tt.want {
				t.Fatalf("ClassifyAgentError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}

	// Reason order: an unauthorized response that also carries a canceled
	// context stays unauthorized, because auth is the actionable cause.
	both := fmt.Errorf("%w: %w: %w", ErrNodeInventoryUnavailable, ErrAgentUnauthorized, context.Canceled)
	if got := ClassifyAgentError(both); got != ReasonUnauthorized {
		t.Fatalf("ClassifyAgentError(unauthorized+canceled) = %q, want %q", got, ReasonUnauthorized)
	}
}

// TestK8sProviderAgentErrorCounts proves that a failing node-agent inventory
// bumps the (node, reason) counter exactly once per snapshot and that the map
// stays a copy.
func TestK8sProviderAgentErrorCounts(t *testing.T) {
	tests := []struct {
		name       string
		agent      *fakeAgent
		wantReason string
	}{
		{name: "unauthorized", agent: &fakeAgent{token: "s3cret"}, wantReason: ReasonUnauthorized},
		{name: "malformed body", agent: &fakeAgent{snapshotBody: "{not json"}, wantReason: ReasonProtocol},
		{name: "unsupported api version", agent: &fakeAgent{apiVersion: "v9"}, wantReason: ReasonProtocol},
		{name: "server error", agent: &fakeAgent{snapshotStatus: http.StatusInternalServerError}, wantReason: ReasonUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := startFakeAgent(t, tt.agent)
			provider := newAgentProvider(t, server, []runtime.Object{
				testNodeWithIP("worker-01", "127.0.0.1", true),
			})

			snapshot := provider.Snapshot(context.Background())
			if len(snapshot.Nodes) != 1 || snapshot.Nodes[0].AgentUp {
				t.Fatalf("node = %+v, want agent down", snapshot.Nodes)
			}

			key := AgentErrorKey{Node: "worker-01", Reason: tt.wantReason}
			want := map[AgentErrorKey]uint64{key: 1}
			if got := provider.AgentErrors(); !reflect.DeepEqual(got, want) {
				t.Fatalf("AgentErrors() = %+v, want %+v", got, want)
			}

			// SyncNow bypasses the snapshot TTL: the same failure is counted again.
			if err := provider.SyncNow(context.Background(), "test"); err != nil {
				t.Fatalf("SyncNow: %v", err)
			}
			provider.Snapshot(context.Background())
			want[key] = 2
			if got := provider.AgentErrors(); !reflect.DeepEqual(got, want) {
				t.Fatalf("AgentErrors() after second snapshot = %+v, want %+v", got, want)
			}

			// AgentErrors returns a copy: callers cannot corrupt provider state.
			got := provider.AgentErrors()
			got[key] = 999
			if again := provider.AgentErrors(); again[key] != 2 {
				t.Fatalf("AgentErrors() state was mutated through the returned map: %+v", again)
			}
		})
	}
}

// TestK8sProviderAgentErrorTimeout proves a hung agent is classified as a timeout
// (the transport keeps context.DeadlineExceeded in the error chain).
func TestK8sProviderAgentErrorTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)

	provider := newAgentProvider(t, slow, []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
	}, WithAgentTimeout(20*time.Millisecond))

	provider.Snapshot(context.Background())
	want := map[AgentErrorKey]uint64{{Node: "worker-01", Reason: ReasonTimeout}: 1}
	if got := provider.AgentErrors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentErrors() = %+v, want %+v", got, want)
	}
}

// TestK8sProviderAgentErrorsPerNode proves the counter is keyed per node, not
// deduplicated across the cluster.
func TestK8sProviderAgentErrorsPerNode(t *testing.T) {
	server := startFakeAgent(t, &fakeAgent{token: "s3cret"})
	provider := newAgentProvider(t, server, []runtime.Object{
		testNodeWithIP("worker-01", "127.0.0.1", true),
		testNodeWithIP("worker-02", "127.0.0.1", true),
	})

	provider.Snapshot(context.Background())
	want := map[AgentErrorKey]uint64{
		{Node: "worker-01", Reason: ReasonUnauthorized}: 1,
		{Node: "worker-02", Reason: ReasonUnauthorized}: 1,
	}
	if got := provider.AgentErrors(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentErrors() = %+v, want %+v", got, want)
	}
}

func TestK8sProviderAgentErrorsWithoutTransport(t *testing.T) {
	// A provider without the agent transport must not panic and reports an empty
	// map (the optional capability still satisfied).
	provider := &K8sProvider{}
	if got := provider.AgentErrors(); len(got) != 0 {
		t.Fatalf("AgentErrors() = %+v, want empty", got)
	}
}
