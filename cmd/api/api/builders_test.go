package api

import (
	"testing"

	"github.com/kernel/hypeman/lib/builders"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListBuilders_Empty(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.ListBuilders(ctx(), oapi.ListBuildersRequestObject{})
	require.NoError(t, err)

	list, ok := resp.(oapi.ListBuilders200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.NotNil(t, list, "list must be non-nil even when empty")
	assert.Empty(t, list)
}

func TestCreateBuilder(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	name := "team-cache"
	sizeGb := 20
	resp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{
			Name:       &name,
			DiskSizeGb: &sizeGb,
		},
	})
	require.NoError(t, err)
	created, ok := resp.(oapi.CreateBuilder201JSONResponse)
	require.True(t, ok, "expected 201 response")
	assert.NotEmpty(t, created.Id)
	require.NotNil(t, created.Name)
	assert.Equal(t, name, *created.Name)
	assert.Equal(t, sizeGb, created.DiskSizeGb)
	assert.Equal(t, oapi.BuilderStatusReady, created.Status)
	assert.Nil(t, created.LastUsedAt)

	// Disk provisioned eagerly.
	_, err = svc.VolumeManager.GetVolume(ctx(), builders.DiskVolumeID(created.Id))
	require.NoError(t, err)
}

func TestCreateBuilder_DefaultDiskSize(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{},
	})
	require.NoError(t, err)
	created, ok := resp.(oapi.CreateBuilder201JSONResponse)
	require.True(t, ok, "expected 201 response")
	assert.Equal(t, builders.DefaultDiskSizeGb, created.DiskSizeGb)
}

func TestCreateBuilder_CallerSuppliedID(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	id := "team-cache-1"
	resp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{Id: &id},
	})
	require.NoError(t, err)
	_, ok := resp.(oapi.CreateBuilder201JSONResponse)
	require.True(t, ok, "expected 201 response")

	// Replay with the same ID conflicts (control-plane idempotency).
	resp, err = svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{Id: &id},
	})
	require.NoError(t, err)
	_, ok = resp.(oapi.CreateBuilder409JSONResponse)
	assert.True(t, ok, "expected 409 response")
}

func TestCreateBuilder_InvalidID(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	id := "bad/id"
	resp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{Id: &id},
	})
	require.NoError(t, err)
	_, ok := resp.(oapi.CreateBuilder400JSONResponse)
	assert.True(t, ok, "expected 400 response")
}

func TestGetBuilder(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	name := "cache"
	createResp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{Name: &name},
	})
	require.NoError(t, err)
	created := createResp.(oapi.CreateBuilder201JSONResponse)

	resp, err := svc.GetBuilder(ctxWithBuilder(svc, created.Id), oapi.GetBuilderRequestObject{Id: created.Id})
	require.NoError(t, err)
	got, ok := resp.(oapi.GetBuilder200JSONResponse)
	require.True(t, ok, "expected 200 response")
	assert.Equal(t, created.Id, got.Id)
	require.NotNil(t, got.Name)
	assert.Equal(t, name, *got.Name)
}

func TestListBuilders_TagFilter(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	resourceTags := oapi.Tags{"team": "a"}
	_, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{Tags: &resourceTags},
	})
	require.NoError(t, err)
	_, err = svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{},
	})
	require.NoError(t, err)

	resp, err := svc.ListBuilders(ctx(), oapi.ListBuildersRequestObject{})
	require.NoError(t, err)
	list := resp.(oapi.ListBuilders200JSONResponse)
	assert.Len(t, list, 2)

	resp, err = svc.ListBuilders(ctx(), oapi.ListBuildersRequestObject{
		Params: oapi.ListBuildersParams{Tags: &resourceTags},
	})
	require.NoError(t, err)
	list = resp.(oapi.ListBuilders200JSONResponse)
	require.Len(t, list, 1)
	assert.Equal(t, "a", (*list[0].Tags)["team"])
}

func TestDeleteBuilder(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	createResp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{},
	})
	require.NoError(t, err)
	created := createResp.(oapi.CreateBuilder201JSONResponse)

	resp, err := svc.DeleteBuilder(ctxWithBuilder(svc, created.Id), oapi.DeleteBuilderRequestObject{Id: created.Id})
	require.NoError(t, err)
	_, ok := resp.(oapi.DeleteBuilder204Response)
	assert.True(t, ok, "expected 204 response")

	_, err = svc.BuilderManager.GetBuilder(ctx(), created.Id)
	assert.ErrorIs(t, err, builders.ErrNotFound)
}

func TestDeleteBuilder_InUse(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	createResp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{},
	})
	require.NoError(t, err)
	created := createResp.(oapi.CreateBuilder201JSONResponse)

	_, err = svc.BuilderManager.AcquireForBuild(ctx(), created.Id, "build-1")
	require.NoError(t, err)

	resp, err := svc.DeleteBuilder(ctxWithBuilder(svc, created.Id), oapi.DeleteBuilderRequestObject{Id: created.Id})
	require.NoError(t, err)
	_, ok := resp.(oapi.DeleteBuilder409JSONResponse)
	assert.True(t, ok, "expected 409 while a build holds the builder")
}

func TestPruneBuilder(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	createResp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{},
	})
	require.NoError(t, err)
	created := createResp.(oapi.CreateBuilder201JSONResponse)

	resp, err := svc.PruneBuilder(ctxWithBuilder(svc, created.Id), oapi.PruneBuilderRequestObject{Id: created.Id})
	require.NoError(t, err)
	pruned, ok := resp.(oapi.PruneBuilder202JSONResponse)
	require.True(t, ok, "expected 202 response")
	assert.Equal(t, oapi.BuilderStatusPruning, pruned.Status)

	// Identity preserved.
	assert.Equal(t, created.Id, pruned.Id)
}

func TestPruneBuilder_InUse(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	createResp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{},
	})
	require.NoError(t, err)
	created := createResp.(oapi.CreateBuilder201JSONResponse)

	_, err = svc.BuilderManager.AcquireForBuild(ctx(), created.Id, "build-1")
	require.NoError(t, err)

	resp, err := svc.PruneBuilder(ctxWithBuilder(svc, created.Id), oapi.PruneBuilderRequestObject{Id: created.Id})
	require.NoError(t, err)
	_, ok := resp.(oapi.PruneBuilder409JSONResponse)
	assert.True(t, ok, "expected 409 while a build holds the builder")
}

func TestDeleteBuilder_NotFoundAfterResolution(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)

	createResp, err := svc.CreateBuilder(ctx(), oapi.CreateBuilderRequestObject{
		Body: &oapi.CreateBuilderRequest{},
	})
	require.NoError(t, err)
	created := createResp.(oapi.CreateBuilder201JSONResponse)

	// Resolve, then delete underneath the handler to simulate the reaper
	// firing between resolution and the manager call.
	resolvedCtx := ctxWithBuilder(svc, created.Id)
	require.NoError(t, svc.BuilderManager.DeleteBuilder(ctx(), created.Id))

	resp, err := svc.DeleteBuilder(resolvedCtx, oapi.DeleteBuilderRequestObject{Id: created.Id})
	require.NoError(t, err)
	_, ok := resp.(oapi.DeleteBuilder404JSONResponse)
	assert.True(t, ok, "expected 404 when the builder is gone after resolution")
}
