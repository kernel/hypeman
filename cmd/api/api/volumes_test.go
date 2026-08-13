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

func TestGetVolume_ByPathLikeName(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	// Create a volume
	createResp, err := svc.CreateVolume(ctx(), oapi.CreateVolumeRequestObject{
		Body: &oapi.CreateVolumeRequest{
			Name:   "team/data",
			SizeGb: 1,
		},
	})
	require.NoError(t, err)
	created := createResp.(oapi.CreateVolume201JSONResponse)

	// Get by name (not ID) - use ctxWithVolume to simulate middleware
	resp, err := svc.GetVolume(ctxWithVolume(svc, "team/data"), oapi.GetVolumeRequestObject{
		Id: "team/data", // using name instead of ID
	})
	require.NoError(t, err)

	vol, ok := resp.(oapi.GetVolume200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Equal(t, created.Id, vol.Id)
	assert.Equal(t, "team/data", vol.Name)
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

func TestCreateVolume_InvalidID(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	id := "../outside"

	resp, err := svc.CreateVolume(ctx(), oapi.CreateVolumeRequestObject{Body: &oapi.CreateVolumeRequest{Name: "invalid", SizeGb: 1, Id: &id}})
	require.NoError(t, err)
	_, ok := resp.(oapi.CreateVolume400JSONResponse)
	require.True(t, ok, "expected 400 for invalid ID")

	archiveResp, err := svc.CreateVolumeFromArchive(ctx(), oapi.CreateVolumeFromArchiveRequestObject{
		Params: oapi.CreateVolumeFromArchiveParams{Name: "invalid", SizeGb: 1, Id: &id},
	})
	require.NoError(t, err)
	_, ok = archiveResp.(oapi.CreateVolumeFromArchive400JSONResponse)
	require.True(t, ok, "expected 400 for invalid archive volume ID")
}

func TestCreateVolume_ReservedIDPrefix(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	id := "builder-disk-abc123"
	resp, err := svc.CreateVolume(ctx(), oapi.CreateVolumeRequestObject{
		Body: &oapi.CreateVolumeRequest{
			Name:   "squat",
			SizeGb: 1,
			Id:     &id,
		},
	})
	require.NoError(t, err)
	bad, ok := resp.(oapi.CreateVolume400JSONResponse)
	require.True(t, ok, "expected 400 for reserved ID prefix")
	assert.Contains(t, bad.Message, "reserved for internal use")

	// A near-prefix that does not match exactly stays allowed.
	id = "builder-disks"
	resp, err = svc.CreateVolume(ctx(), oapi.CreateVolumeRequestObject{
		Body: &oapi.CreateVolumeRequest{
			Name:   "not-reserved",
			SizeGb: 1,
			Id:     &id,
		},
	})
	require.NoError(t, err)
	_, ok = resp.(oapi.CreateVolume201JSONResponse)
	assert.True(t, ok, "expected 201 for non-reserved ID")
}

func TestCreateVolume_ReservedTagNamespace(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resourceTags := oapi.Tags{"hypeman.system/managed-by": "builder"}
	resp, err := svc.CreateVolume(ctx(), oapi.CreateVolumeRequestObject{
		Body: &oapi.CreateVolumeRequest{
			Name:   "spoofed",
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

	resourceTags := oapi.Tags{"hypeman.system/managed-by": "builder"}
	resp, err := svc.CreateVolumeFromArchive(ctx(), oapi.CreateVolumeFromArchiveRequestObject{
		Params: oapi.CreateVolumeFromArchiveParams{
			Name:   "spoofed",
			SizeGb: 1,
			Tags:   &resourceTags,
		},
	})
	require.NoError(t, err)
	bad, ok := resp.(oapi.CreateVolumeFromArchive400JSONResponse)
	require.True(t, ok, "expected 400 for reserved tag key")
	assert.Contains(t, bad.Message, "reserved for internal use")
}

func TestDeleteVolume_ReservedIDPrefix(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	id := "builder-disk-abc123"
	_, err := svc.VolumeManager.CreateVolume(ctx(), volumes.CreateVolumeRequest{
		Id:     &id,
		Name:   id,
		SizeGb: 1,
	})
	require.NoError(t, err)

	resp, err := svc.DeleteVolume(ctxWithVolume(svc, id), oapi.DeleteVolumeRequestObject{Id: id})
	require.NoError(t, err)
	conflict, ok := resp.(oapi.DeleteVolume409JSONResponse)
	require.True(t, ok, "expected 409 for reserved ID prefix")
	assert.Contains(t, conflict.Message, "reserved for internal use")

	_, err = svc.VolumeManager.GetVolume(ctx(), id)
	require.NoError(t, err, "reserved volume must not be deleted")
}
