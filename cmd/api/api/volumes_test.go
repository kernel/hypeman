package api

import (
	"testing"

	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListVolumes_Empty(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.ListVolumes(ctx(), oapi.ListVolumesRequestObject{})
	require.NoError(t, err)

	list, ok := resp.(oapi.ListVolumes200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Empty(t, list)
}

func TestGetVolume_NotFound(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.GetVolume(ctx(), oapi.GetVolumeRequestObject{
		Id: "non-existent",
	})
	require.NoError(t, err)

	notFound, ok := resp.(oapi.GetVolume404JSONResponse)
	require.True(t, ok, "expected 404 response")
	assert.Equal(t, "not_found", notFound.Code)
}

