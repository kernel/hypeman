package instances

import (
	"context"
	"fmt"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockImageManager implements images.Manager for testing the auto-pull flow.
type mockImageManager struct {
	images.Manager // embed for unimplemented methods

	getImageCalls    []string
	createImageCalls []string
	waitForReadyCalls []string

	// getImageResults maps call index → result. On first call returns ErrNotFound,
	// on second call (after pull) returns the ready image.
	getImageResults []getImageResult
	createImageErr  error
	waitForReadyErr error
}

type getImageResult struct {
	image *images.Image
	err   error
}

func (m *mockImageManager) GetImage(_ context.Context, name string) (*images.Image, error) {
	idx := len(m.getImageCalls)
	m.getImageCalls = append(m.getImageCalls, name)
	if idx < len(m.getImageResults) {
		r := m.getImageResults[idx]
		return r.image, r.err
	}
	return nil, fmt.Errorf("unexpected GetImage call #%d", idx)
}

func (m *mockImageManager) CreateImage(_ context.Context, req images.CreateImageRequest) (*images.Image, error) {
	m.createImageCalls = append(m.createImageCalls, req.Name)
	if m.createImageErr != nil {
		return nil, m.createImageErr
	}
	return &images.Image{Name: req.Name, Status: images.StatusPulling}, nil
}

func (m *mockImageManager) WaitForReady(_ context.Context, name string) error {
	m.waitForReadyCalls = append(m.waitForReadyCalls, name)
	return m.waitForReadyErr
}

// newTestManagerWithMockImages creates a manager with a mock image manager for
// testing auto-pull logic without KVM.
func newTestManagerWithMockImages(t *testing.T, imgMgr images.Manager) *manager {
	t.Helper()
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)
	systemMgr := system.NewManager(p)

	return &manager{
		paths:         p,
		imageManager:  imgMgr,
		systemManager: systemMgr,
		limits: ResourceLimits{
			MaxOverlaySize: 100 * 1024 * 1024 * 1024, // 100GB
		},
		vmStarters: make(map[hypervisor.Type]hypervisor.VMStarter), // empty — will fail at getVMStarter
	}
}

func TestCreateInstance_AutoPullImage(t *testing.T) {
	t.Parallel()

	const imageName = "docker.io/library/alpine:latest"

	imgMgr := &mockImageManager{
		getImageResults: []getImageResult{
			// First call: image not found → triggers auto-pull
			{image: nil, err: images.ErrNotFound},
			// Second call (after pull): image is ready
			{image: &images.Image{
				Name:   imageName,
				Status: images.StatusReady,
				Digest: "sha256:abc123",
			}, err: nil},
		},
	}

	mgr := newTestManagerWithMockImages(t, imgMgr)

	_, err := mgr.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:  "test-auto-pull",
		Image: imageName,
	})

	// The call will fail downstream (no VM starter), but the auto-pull flow
	// should have been exercised before that point.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no VM starter", "should fail at VM starter, not at image lookup")

	// Verify auto-pull flow was triggered
	require.Len(t, imgMgr.getImageCalls, 2, "GetImage should be called twice (initial + post-pull)")
	assert.Equal(t, imageName, imgMgr.getImageCalls[0])
	assert.Equal(t, imageName, imgMgr.getImageCalls[1])

	require.Len(t, imgMgr.createImageCalls, 1, "CreateImage should be called once to trigger pull")
	assert.Equal(t, imageName, imgMgr.createImageCalls[0])

	require.Len(t, imgMgr.waitForReadyCalls, 1, "WaitForReady should be called once")
	assert.Equal(t, imageName, imgMgr.waitForReadyCalls[0])
}

func TestCreateInstance_AutoPullImage_CreateImageFails(t *testing.T) {
	t.Parallel()

	const imageName = "docker.io/library/alpine:latest"

	imgMgr := &mockImageManager{
		getImageResults: []getImageResult{
			{image: nil, err: images.ErrNotFound},
		},
		createImageErr: fmt.Errorf("registry unavailable"),
	}

	mgr := newTestManagerWithMockImages(t, imgMgr)

	_, err := mgr.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:  "test-auto-pull-fail",
		Image: imageName,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto-pull image")
	assert.Contains(t, err.Error(), "registry unavailable")

	// CreateImage was attempted but failed; WaitForReady should not be called
	require.Len(t, imgMgr.createImageCalls, 1)
	assert.Empty(t, imgMgr.waitForReadyCalls)
}

func TestCreateInstance_AutoPullImage_WaitForReadyFails(t *testing.T) {
	t.Parallel()

	const imageName = "docker.io/library/alpine:latest"

	imgMgr := &mockImageManager{
		getImageResults: []getImageResult{
			{image: nil, err: images.ErrNotFound},
		},
		waitForReadyErr: fmt.Errorf("image build failed"),
	}

	mgr := newTestManagerWithMockImages(t, imgMgr)

	_, err := mgr.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:  "test-auto-pull-wait-fail",
		Image: imageName,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "auto-pull image")
	assert.Contains(t, err.Error(), "image build failed")

	require.Len(t, imgMgr.createImageCalls, 1)
	require.Len(t, imgMgr.waitForReadyCalls, 1)
	// GetImage should only be called once since WaitForReady failed
	require.Len(t, imgMgr.getImageCalls, 1)
}

func TestCreateInstance_ImageAlreadyReady_NoAutoPull(t *testing.T) {
	t.Parallel()

	const imageName = "docker.io/library/alpine:latest"

	imgMgr := &mockImageManager{
		getImageResults: []getImageResult{
			// Image already exists and is ready
			{image: &images.Image{
				Name:   imageName,
				Status: images.StatusReady,
				Digest: "sha256:abc123",
			}, err: nil},
		},
	}

	mgr := newTestManagerWithMockImages(t, imgMgr)

	_, err := mgr.CreateInstance(context.Background(), CreateInstanceRequest{
		Name:  "test-no-auto-pull",
		Image: imageName,
	})

	// Fails downstream at VM starter, but no auto-pull should have happened
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no VM starter")

	// GetImage called once, no pull triggered
	require.Len(t, imgMgr.getImageCalls, 1)
	assert.Empty(t, imgMgr.createImageCalls, "CreateImage should NOT be called when image exists")
	assert.Empty(t, imgMgr.waitForReadyCalls, "WaitForReady should NOT be called when image exists")
}
