package api

import (
	"testing"

	"github.com/onkernel/hypeman/lib/oapi"
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

