package api

import (
	"testing"

	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListImages_Empty(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.ListImages(ctx(), oapi.ListImagesRequestObject{})
	require.NoError(t, err)

	list, ok := resp.(oapi.ListImages200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Empty(t, list)
}

func TestGetImage_NotFound(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.GetImage(ctx(), oapi.GetImageRequestObject{
		Id: "non-existent",
	})
	require.NoError(t, err)

	notFound, ok := resp.(oapi.GetImage404JSONResponse)
	require.True(t, ok, "expected 404 response")
	assert.Equal(t, "not_found", notFound.Code)
	assert.Equal(t, "image not found", notFound.Message)
}

