package agent

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Client bundles the CRI v1 runtime and image clients of one node and adapts
// the protobuf API to the small CRI interface the agent uses.
// Dependency boundary (ADR-001): grpc and k8s.io/cri-api are allowed only in
// internal/agent.
type Client struct {
	conn    *grpc.ClientConn
	Runtime runtimeapi.RuntimeServiceClient
	Images  runtimeapi.ImageServiceClient

	// statFS is swappable in tests; production uses osStatFS.
	statFS func(mountpoint string) (capacity, available int64)
}

// DialCRI connects to the node CRI socket (for example
// /run/containerd/containerd.sock). The connection is lazy: no RPC is issued
// until the first call.
func DialCRI(_ context.Context, socketPath string) (*Client, error) {
	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("agent: dial CRI socket %s: %w", socketPath, err)
	}
	return &Client{
		conn:    conn,
		Runtime: runtimeapi.NewRuntimeServiceClient(conn),
		Images:  runtimeapi.NewImageServiceClient(conn),
		statFS:  osStatFS,
	}, nil
}

// Close releases the CRI connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// ListImages returns every image cached by the runtime.
func (c *Client) ListImages(ctx context.Context) ([]ImageInfo, error) {
	resp, err := c.Images.ListImages(ctx, &runtimeapi.ListImagesRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]ImageInfo, 0, len(resp.GetImages()))
	for _, img := range resp.GetImages() {
		out = append(out, imageInfoFromCRI(img))
	}
	return out, nil
}

// ImageStatus looks up one image by reference (tag, digest or ID).
func (c *Client) ImageStatus(ctx context.Context, image string) (ImageInfo, bool, error) {
	resp, err := c.Images.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{
		Image: &runtimeapi.ImageSpec{Image: image},
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return ImageInfo{}, false, nil
		}
		return ImageInfo{}, false, err
	}
	if resp.GetImage() == nil {
		return ImageInfo{}, false, nil
	}
	return imageInfoFromCRI(resp.GetImage()), true, nil
}

// PullImage pulls image through CRI and returns the runtime reference it
// resolved to.
func (c *Client) PullImage(ctx context.Context, image string) (string, error) {
	resp, err := c.Images.PullImage(ctx, &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: image},
	})
	if err != nil {
		return "", err
	}
	return resp.GetImageRef(), nil
}

// ImageFS reports image filesystem usage. CRI returns usage only, so capacity
// and available bytes come from statfs of the reported mountpoint.
func (c *Client) ImageFS(ctx context.Context) (ImageFSStats, error) {
	resp, err := c.Images.ImageFsInfo(ctx, &runtimeapi.ImageFsInfoRequest{})
	if err != nil {
		return ImageFSStats{}, err
	}
	if len(resp.GetImageFilesystems()) == 0 {
		return ImageFSStats{}, fmt.Errorf("agent: CRI returned no image filesystem info")
	}
	stats := ImageFSStats{}
	seenMounts := make(map[string]struct{}, len(resp.GetImageFilesystems()))
	for _, fs := range resp.GetImageFilesystems() {
		if used := fs.GetUsedBytes(); used != nil {
			stats.UsedBytes += int64(used.GetValue())
		}
		mountpoint := fs.GetFsId().GetMountpoint()
		if mountpoint == "" {
			continue
		}
		if stats.Mountpoint == "" {
			stats.Mountpoint = mountpoint
		}
		if _, ok := seenMounts[mountpoint]; ok {
			continue
		}
		seenMounts[mountpoint] = struct{}{}
		capacity, available := c.statFS(mountpoint)
		stats.CapacityBytes += capacity
		stats.AvailableBytes += available
	}
	if stats.CapacityBytes > 0 && stats.AvailableBytes > stats.CapacityBytes {
		stats.AvailableBytes = stats.CapacityBytes
	}
	stats.UsedRatio = UsedRatio(stats.UsedBytes, stats.CapacityBytes)
	return stats, nil
}

// RuntimeState reports runtime readiness and conditions.
func (c *Client) RuntimeState(ctx context.Context) (RuntimeState, error) {
	resp, err := c.Runtime.Status(ctx, &runtimeapi.StatusRequest{})
	if err != nil {
		return RuntimeState{}, err
	}
	rt := resp.GetStatus()
	state := RuntimeState{Ready: false}
	for _, cond := range rt.GetConditions() {
		condStatus := "False"
		if cond.GetStatus() {
			condStatus = "True"
		}
		state.Conditions = append(state.Conditions, Condition{
			Type:    cond.GetType(),
			Status:  condStatus,
			Reason:  cond.GetReason(),
			Message: cond.GetMessage(),
		})
		if cond.GetType() == "RuntimeReady" && cond.GetStatus() {
			state.Ready = true
		}
	}
	return state, nil
}

// RuntimeVersion reports the runtime name and version behind the CRI socket.
func (c *Client) RuntimeVersion(ctx context.Context) (RuntimeInfo, error) {
	resp, err := c.Runtime.Version(ctx, &runtimeapi.VersionRequest{})
	if err != nil {
		return RuntimeInfo{}, err
	}
	return RuntimeInfo{Name: resp.GetRuntimeName(), Version: resp.GetRuntimeVersion()}, nil
}

func imageInfoFromCRI(img *runtimeapi.Image) ImageInfo {
	info := ImageInfo{
		ID:        img.GetId(),
		Tags:      append([]string(nil), img.GetRepoTags()...),
		Digests:   append([]string(nil), img.GetRepoDigests()...),
		SizeBytes: int64(img.GetSize_()),
		Pinned:    img.GetPinned(),
	}
	if len(info.Tags) > 0 {
		info.Name = info.Tags[0]
	} else if len(info.Digests) > 0 {
		info.Name = info.Digests[0]
	} else {
		info.Name = info.ID
	}
	return info
}
