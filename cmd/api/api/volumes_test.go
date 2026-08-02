package api

import (
	"testing"

	"github.com/kernel/hypeman/lib/oapi"
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
