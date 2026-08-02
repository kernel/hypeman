package api

import (
	"testing"

	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListVolumes_Empty(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.ListVolumes(ctx(), oapi.ListVolumesRequestObject{})
	require.NoError(t, err)

	list, ok := resp.(oapi.ListVolumes200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Empty(t, list)
}

func TestGetVolume_NotFound(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// With middleware, not-found would be handled before reaching handler.
	// For this test, we call the manager directly to verify the error.
	_, err := svc.VolumeManager.GetVolume(ctx(), "non-existent")
	require.Error(t, err)
}

func TestGetVolume_ByName(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// Create a volume
	createResp, err := svc.CreateVolume(ctx(), oapi.CreateVolumeRequestObject{
		Body: &oapi.CreateVolumeRequest{
			Name:   "my-data",
			SizeGb: 1,
		},
	})
	require.NoError(t, err)
	created := createResp.(oapi.CreateVolume201JSONResponse)

	// Get by name (not ID) - use ctxWithVolume to simulate middleware
	resp, err := svc.GetVolume(ctxWithVolume(svc, "my-data"), oapi.GetVolumeRequestObject{
		Id: "my-data", // using name instead of ID
	})
	require.NoError(t, err)

	vol, ok := resp.(oapi.GetVolume200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Equal(t, created.Id, vol.Id)
	assert.Equal(t, "my-data", vol.Name)
}

func TestDeleteVolume_ByName(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// Create a volume
	_, err := svc.CreateVolume(ctx(), oapi.CreateVolumeRequestObject{
		Body: &oapi.CreateVolumeRequest{
			Name:   "to-delete",
			SizeGb: 1,
		},
	})
	require.NoError(t, err)

	// Delete by name - use ctxWithVolume to simulate middleware
	resp, err := svc.DeleteVolume(ctxWithVolume(svc, "to-delete"), oapi.DeleteVolumeRequestObject{
		Id: "to-delete",
	})
	require.NoError(t, err)
	_, ok := resp.(oapi.DeleteVolume204Response)
	assert.True(t, ok, "expected 204 response")
}

func TestCreateVolume_ReservedTagNamespace(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resourceTags := oapi.Tags{"hypeman.system/managed-by": "build-cache"}
	resp, err := svc.CreateVolume(ctx(), oapi.CreateVolumeRequestObject{
		Body: &oapi.CreateVolumeRequest{
			Name:   "spoofed-cache",
			SizeGb: 1,
			Tags:   &resourceTags,
		},
	})
	require.NoError(t, err)
	bad, ok := resp.(oapi.CreateVolume400JSONResponse)
	require.True(t, ok, "expected 400 for reserved tag key")
	assert.Contains(t, bad.Message, "reserved for internal use")

	vols, err := svc.VolumeManager.ListVolumes(ctx())
	require.NoError(t, err)
	assert.Empty(t, vols, "spoofed volume should not be created")
}

func TestCreateVolumeFromArchive_ReservedTagNamespace(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resourceTags := oapi.Tags{"hypeman.system/managed-by": "build-cache"}
	resp, err := svc.CreateVolumeFromArchive(ctx(), oapi.CreateVolumeFromArchiveRequestObject{
		Params: oapi.CreateVolumeFromArchiveParams{
			Name:   "spoofed-cache",
			SizeGb: 1,
			Tags:   &resourceTags,
		},
	})
	require.NoError(t, err)
	bad, ok := resp.(oapi.CreateVolumeFromArchive400JSONResponse)
	require.True(t, ok, "expected 400 for reserved tag key")
	assert.Contains(t, bad.Message, "reserved for internal use")
}

func TestCreateVolume_ReservedIDPrefix(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	for _, id := range []string{"build-cache-abc", "build-disk-1", "build-source-1", "build-config-1"} {
		resp, err := svc.CreateVolume(ctx(), oapi.CreateVolumeRequestObject{
			Body: &oapi.CreateVolumeRequest{
				Name:   "squat",
				SizeGb: 1,
				Id:     &id,
			},
		})
		require.NoError(t, err)
		bad, ok := resp.(oapi.CreateVolume400JSONResponse)
		require.True(t, ok, "expected 400 for reserved ID %q", id)
		assert.Contains(t, bad.Message, "reserved for internal use")
	}
}

func TestCreateVolumeFromArchive_ReservedIDPrefix(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	id := "build-cache-abc"
	resp, err := svc.CreateVolumeFromArchive(ctx(), oapi.CreateVolumeFromArchiveRequestObject{
		Params: oapi.CreateVolumeFromArchiveParams{
			Name:   "squat",
			SizeGb: 1,
			Id:     &id,
		},
	})
	require.NoError(t, err)
	bad, ok := resp.(oapi.CreateVolumeFromArchive400JSONResponse)
	require.True(t, ok, "expected 400 for reserved ID")
	assert.Contains(t, bad.Message, "reserved for internal use")
}

func TestDeleteVolume_ReservedIDPrefix(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	for _, id := range []string{"build-cache-abc", "build-disk-1", "build-source-1", "build-config-1"} {
		_, err := svc.VolumeManager.CreateVolume(ctx(), volumes.CreateVolumeRequest{
			Id:     &id,
			Name:   id,
			SizeGb: 1,
		})
		require.NoError(t, err)

		resp, err := svc.DeleteVolume(ctxWithVolume(svc, id), oapi.DeleteVolumeRequestObject{Id: id})
		require.NoError(t, err)
		conflict, ok := resp.(oapi.DeleteVolume409JSONResponse)
		require.True(t, ok, "expected 409 for reserved ID %q", id)
		assert.Contains(t, conflict.Message, "reserved for internal use")

		_, err = svc.VolumeManager.GetVolume(ctx(), id)
		require.NoError(t, err, "reserved volume %q should not be deleted", id)
	}
}
