package agent

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// fakeImageService overrides the methods the agent uses; the embedded interface
// panics on anything else, which is what we want from a test double.
type fakeImageService struct {
	runtimeapi.ImageServiceClient

	listResp   *runtimeapi.ListImagesResponse
	listErr    error
	statusResp *runtimeapi.ImageStatusResponse
	statusErr  error
	pullResp   *runtimeapi.PullImageResponse
	pullErr    error
	fsResp     *runtimeapi.ImageFsInfoResponse
	fsErr      error
}

func (f *fakeImageService) ListImages(context.Context, *runtimeapi.ListImagesRequest, ...grpc.CallOption) (*runtimeapi.ListImagesResponse, error) {
	return f.listResp, f.listErr
}

func (f *fakeImageService) ImageStatus(context.Context, *runtimeapi.ImageStatusRequest, ...grpc.CallOption) (*runtimeapi.ImageStatusResponse, error) {
	return f.statusResp, f.statusErr
}

func (f *fakeImageService) PullImage(context.Context, *runtimeapi.PullImageRequest, ...grpc.CallOption) (*runtimeapi.PullImageResponse, error) {
	return f.pullResp, f.pullErr
}

func (f *fakeImageService) ImageFsInfo(context.Context, *runtimeapi.ImageFsInfoRequest, ...grpc.CallOption) (*runtimeapi.ImageFsInfoResponse, error) {
	return f.fsResp, f.fsErr
}

type fakeRuntimeService struct {
	runtimeapi.RuntimeServiceClient

	statusResp  *runtimeapi.StatusResponse
	statusErr   error
	versionResp *runtimeapi.VersionResponse
	versionErr  error
}

func (f *fakeRuntimeService) Status(context.Context, *runtimeapi.StatusRequest, ...grpc.CallOption) (*runtimeapi.StatusResponse, error) {
	return f.statusResp, f.statusErr
}

func (f *fakeRuntimeService) Version(context.Context, *runtimeapi.VersionRequest, ...grpc.CallOption) (*runtimeapi.VersionResponse, error) {
	return f.versionResp, f.versionErr
}

func TestImageInfoFromCRI(t *testing.T) {
	img := imageInfoFromCRI(&runtimeapi.Image{
		Id:          "sha256:abc",
		RepoTags:    []string{"registry/a:1", "registry/a:latest"},
		RepoDigests: []string{"registry/a@sha256:def"},
		Size_:       1024,
		Pinned:      true,
	})
	if img.ID != "sha256:abc" || img.Name != "registry/a:1" || img.SizeBytes != 1024 || !img.Pinned {
		t.Fatalf("unexpected ImageInfo: %+v", img)
	}
	if len(img.Tags) != 2 || len(img.Digests) != 1 {
		t.Fatalf("tags/digests not copied: %+v", img)
	}

	digestOnly := imageInfoFromCRI(&runtimeapi.Image{Id: "sha256:xyz", RepoDigests: []string{"r@sha256:1"}})
	if digestOnly.Name != "r@sha256:1" {
		t.Fatalf("digest fallback name = %q", digestOnly.Name)
	}
	idOnly := imageInfoFromCRI(&runtimeapi.Image{Id: "sha256:id"})
	if idOnly.Name != "sha256:id" {
		t.Fatalf("ID fallback name = %q", idOnly.Name)
	}
}

func TestClientListImages(t *testing.T) {
	c := &Client{Images: &fakeImageService{listResp: &runtimeapi.ListImagesResponse{
		Images: []*runtimeapi.Image{
			{Id: "id-1", RepoTags: []string{"a:1"}, Size_: 10},
			{Id: "id-2", RepoTags: []string{"b:1"}, Size_: 20},
		},
	}}}
	images, err := c.ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 2 || images[0].ID != "id-1" || images[1].SizeBytes != 20 {
		t.Fatalf("unexpected images: %+v", images)
	}

	c.Images = &fakeImageService{listErr: errors.New("rpc")}
	if _, err := c.ListImages(context.Background()); err == nil {
		t.Fatal("ListImages error = nil")
	}
}

func TestClientImageStatus(t *testing.T) {
	ref := &runtimeapi.Image{Id: "id-1", RepoTags: []string{"a:1"}, Size_: 10}
	c := &Client{Images: &fakeImageService{statusResp: &runtimeapi.ImageStatusResponse{Image: ref}}}
	img, ok, err := c.ImageStatus(context.Background(), "a:1")
	if err != nil || !ok || img.ID != "id-1" {
		t.Fatalf("ImageStatus = %+v, %v, %v", img, ok, err)
	}

	c.Images = &fakeImageService{statusResp: &runtimeapi.ImageStatusResponse{}}
	if _, ok, err := c.ImageStatus(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("ImageStatus(missing) = %v, %v; want false, nil", ok, err)
	}

	c.Images = &fakeImageService{statusErr: status.Error(codes.NotFound, "no image")}
	if _, ok, err := c.ImageStatus(context.Background(), "missing"); err != nil || ok {
		t.Fatalf("ImageStatus(NotFound) = %v, %v; want false, nil", ok, err)
	}

	boom := errors.New("boom")
	c.Images = &fakeImageService{statusErr: boom}
	if _, _, err := c.ImageStatus(context.Background(), "a:1"); !errors.Is(err, boom) {
		t.Fatalf("ImageStatus(other error) = %v, want boom", err)
	}
}

func TestClientPullImage(t *testing.T) {
	c := &Client{Images: &fakeImageService{pullResp: &runtimeapi.PullImageResponse{ImageRef: "sha256:abc"}}}
	ref, err := c.PullImage(context.Background(), "nginx:latest")
	if err != nil || ref != "sha256:abc" {
		t.Fatalf("PullImage = %q, %v", ref, err)
	}
	c.Images = &fakeImageService{pullErr: errors.New("401 unauthorized")}
	if _, err := c.PullImage(context.Background(), "nginx:latest"); err == nil {
		t.Fatal("PullImage error = nil")
	}
}

func TestClientImageFS(t *testing.T) {
	c := &Client{
		Images: &fakeImageService{fsResp: &runtimeapi.ImageFsInfoResponse{
			ImageFilesystems: []*runtimeapi.FilesystemUsage{
				{
					FsId:      &runtimeapi.FilesystemIdentifier{Mountpoint: "/var/lib/containerd"},
					UsedBytes: &runtimeapi.UInt64Value{Value: 600},
				},
				{
					FsId:      &runtimeapi.FilesystemIdentifier{Mountpoint: "/var/lib/containerd"},
					UsedBytes: &runtimeapi.UInt64Value{Value: 0},
				},
			},
		}},
		statFS: func(mountpoint string) (int64, int64) {
			if mountpoint != "/var/lib/containerd" {
				t.Fatalf("statFS(%q)", mountpoint)
			}
			return 1000, 400
		},
	}
	fs, err := c.ImageFS(context.Background())
	if err != nil {
		t.Fatalf("ImageFS: %v", err)
	}
	if fs.UsedBytes != 600 || fs.CapacityBytes != 1000 || fs.AvailableBytes != 400 {
		t.Fatalf("unexpected stats: %+v", fs)
	}
	if fs.Mountpoint != "/var/lib/containerd" || fs.UsedRatio != 0.6 {
		t.Fatalf("unexpected stats: %+v", fs)
	}

	c.Images = &fakeImageService{fsResp: &runtimeapi.ImageFsInfoResponse{}}
	if _, err := c.ImageFS(context.Background()); err == nil {
		t.Fatal("ImageFS with no filesystems = nil error")
	}
}

func TestClientRuntimeState(t *testing.T) {
	c := &Client{Runtime: &fakeRuntimeService{statusResp: &runtimeapi.StatusResponse{
		Status: &runtimeapi.RuntimeStatus{Conditions: []*runtimeapi.RuntimeCondition{
			{Type: "RuntimeReady", Status: true},
			{Type: "NetworkReady", Status: true, Reason: "ok"},
		}},
	}}}
	state, err := c.RuntimeState(context.Background())
	if err != nil {
		t.Fatalf("RuntimeState: %v", err)
	}
	if !state.Ready || len(state.Conditions) != 2 {
		t.Fatalf("unexpected state: %+v", state)
	}

	c.Runtime = &fakeRuntimeService{statusResp: &runtimeapi.StatusResponse{
		Status: &runtimeapi.RuntimeStatus{Conditions: []*runtimeapi.RuntimeCondition{
			{Type: "RuntimeReady", Status: false, Reason: "containerd_down"},
		}},
	}}
	state, err = c.RuntimeState(context.Background())
	if err != nil {
		t.Fatalf("RuntimeState: %v", err)
	}
	if state.Ready {
		t.Fatalf("RuntimeState reported ready on RuntimeReady=False: %+v", state)
	}
}

func TestClientRuntimeVersion(t *testing.T) {
	c := &Client{Runtime: &fakeRuntimeService{versionResp: &runtimeapi.VersionResponse{
		RuntimeName:    "containerd",
		RuntimeVersion: "2.1.3",
	}}}
	info, err := c.RuntimeVersion(context.Background())
	if err != nil || info.Name != "containerd" || info.Version != "2.1.3" {
		t.Fatalf("RuntimeVersion = %+v, %v", info, err)
	}
	c.Runtime = &fakeRuntimeService{versionErr: errors.New("unimplemented")}
	if _, err := c.RuntimeVersion(context.Background()); err == nil {
		t.Fatal("RuntimeVersion error = nil")
	}
}

func TestClientCloseWithoutConnection(t *testing.T) {
	if err := (&Client{}).Close(); err != nil {
		t.Fatalf("Close(nil conn) = %v", err)
	}
}
