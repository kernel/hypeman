package api

import (
	"os"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/oapi"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListInstances_Empty(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.ListInstances(ctx(), oapi.ListInstancesRequestObject{})
	require.NoError(t, err)

	list, ok := resp.(oapi.ListInstances200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Empty(t, list)
}

func TestGetInstance_NotFound(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.GetInstance(ctx(), oapi.GetInstanceRequestObject{
		Id: "non-existent",
	})
	require.NoError(t, err)

	notFound, ok := resp.(oapi.GetInstance404JSONResponse)
	require.True(t, ok, "expected 404 response")
	assert.Equal(t, "not_found", notFound.Code)
}

func TestCreateInstance_ParsesHumanReadableSizes(t *testing.T) {
	// Require KVM access for VM creation
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	svc := newTestService(t)

	// First, create and wait for the image to be ready
	t.Log("Creating alpine image...")
	imgResp, err := svc.CreateImage(ctx(), oapi.CreateImageRequestObject{
		Body: &oapi.CreateImageRequest{
			Name: "docker.io/library/alpine:latest",
		},
	})
	require.NoError(t, err)
	
	imgCreated, ok := imgResp.(oapi.CreateImage202JSONResponse)
	require.True(t, ok, "expected 202 accepted response for image creation")
	img := oapi.Image(imgCreated)
	
	// Wait for image to be ready
	t.Log("Waiting for image to be ready...")
	imageName := img.Name
	var image *oapi.Image
	for i := 0; i < 60; i++ {
		getImgResp, err := svc.GetImage(ctx(), oapi.GetImageRequestObject{Name: imageName})
		require.NoError(t, err)
		
		if getImg, ok := getImgResp.(oapi.GetImage200JSONResponse); ok {
			img := oapi.Image(getImg)
			if img.Status == "ready" {
				image = &img
				break
			}
			if img.Status == "failed" {
				t.Fatalf("Image build failed: %v", img.Error)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NotNil(t, image, "image should be ready within 6 seconds")
	t.Log("Image ready!")

	// Ensure system files (kernel and initramfs) are available
	t.Log("Ensuring system files (kernel and initramfs)...")
	systemMgr := system.NewManager(svc.Config.DataDir)
	err = systemMgr.EnsureSystemFiles(ctx())
	require.NoError(t, err)
	t.Log("System files ready!")
	
	// Now test instance creation with human-readable size strings
	size := "512MB"
	hotplugSize := "1GB"
	overlaySize := "5GB"

	t.Log("Creating instance with human-readable sizes...")
	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:        "test-sizes",
			Image:       "docker.io/library/alpine:latest",
			Size:        &size,
			HotplugSize: &hotplugSize,
			OverlaySize: &overlaySize,
		},
	})
	require.NoError(t, err)

	// Should successfully create the instance
	created, ok := resp.(oapi.CreateInstance201JSONResponse)
	require.True(t, ok, "expected 201 response")
	
	instance := oapi.Instance(created)
	
	// Verify the instance was created with our sizes
	assert.Equal(t, "test-sizes", instance.Name)
	assert.NotNil(t, instance.Size)
	assert.NotNil(t, instance.HotplugSize)
	assert.NotNil(t, instance.OverlaySize)
	
	// Verify sizes are formatted as human-readable strings (not raw bytes)
	t.Logf("Response sizes: size=%s, hotplug_size=%s, overlay_size=%s", 
		*instance.Size, *instance.HotplugSize, *instance.OverlaySize)
	
	// Verify exact formatted output from the API
	// Note: 1GB (1073741824 bytes) is formatted as 1024.0 MB by the .HR() method
	assert.Equal(t, "512.0 MB", *instance.Size, "size should be formatted as 512.0 MB")
	assert.Equal(t, "1024.0 MB", *instance.HotplugSize, "hotplug_size should be formatted as 1024.0 MB (1GB)")
	assert.Equal(t, "5.0 GB", *instance.OverlaySize, "overlay_size should be formatted as 5.0 GB")
}

func TestCreateInstance_InvalidSizeFormat(t *testing.T) {
	svc := newTestService(t)

	// Test with invalid size format
	invalidSize := "not-a-size"

	resp, err := svc.CreateInstance(ctx(), oapi.CreateInstanceRequestObject{
		Body: &oapi.CreateInstanceRequest{
			Name:  "test-invalid",
			Image: "docker.io/library/alpine:latest",
			Size:  &invalidSize,
		},
	})
	require.NoError(t, err)

	// Should get invalid_size error
	badReq, ok := resp.(oapi.CreateInstance400JSONResponse)
	require.True(t, ok, "expected 400 response")
	assert.Equal(t, "invalid_size", badReq.Code)
	assert.Contains(t, badReq.Message, "invalid size format")
}

