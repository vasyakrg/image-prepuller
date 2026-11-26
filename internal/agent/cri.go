package agent

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// CRI bundles the CRI v1 runtime and image clients of one node.
// Dependency boundary (ADR-001): grpc and k8s.io/cri-api are allowed only in
// internal/agent.
type CRI struct {
	conn    *grpc.ClientConn
	Runtime runtimeapi.RuntimeServiceClient
	Images  runtimeapi.ImageServiceClient
}

// DialCRI connects to the node CRI socket (for example
// /run/containerd/containerd.sock). The connection is lazy: no RPC is issued
// until the first call.
func DialCRI(_ context.Context, socketPath string) (*CRI, error) {
	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("agent: dial CRI socket %s: %w", socketPath, err)
	}
	return &CRI{
		conn:    conn,
		Runtime: runtimeapi.NewRuntimeServiceClient(conn),
		Images:  runtimeapi.NewImageServiceClient(conn),
	}, nil
}

// Close releases the CRI connection.
func (c *CRI) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}
