package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/kernel/hypeman/lib/imagepush"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/require"
)

// fakePushManager implements imagepush.Manager for handler tests.
type fakePushManager struct {
	createErr  error
	createdReq imagepush.PushRequest
	push       *imagepush.Push
	getErr     error
	listErr    error
	pushes     []imagepush.Push
}

func (f *fakePushManager) CreatePush(_ context.Context, req imagepush.PushRequest) (*imagepush.Push, error) {
	f.createdReq = req
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.push, nil
}

func (f *fakePushManager) GetPush(_ context.Context, _ string) (*imagepush.Push, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.push, nil
}

func (f *fakePushManager) ListPushes(_ context.Context) ([]imagepush.Push, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.pushes, nil
}

func (f *fakePushManager) WaitForPush(_ context.Context, _ string) error { return nil }

func (f *fakePushManager) InProgressDigests() []string        { return nil }
func (f *fakePushManager) LiveCacheManifestDigests() []string { return nil }

func TestCreatePush_MapsRequestAndCredentials(t *testing.T) {
	t.Parallel()

	now := time.Now().Truncate(time.Second)
	fake := &fakePushManager{push: &imagepush.Push{
		ID:        "push-1",
		Image:     "docker.io/library/alpine:latest",
		Digest:    "sha256:abc",
		Target:    "registry.example.com/app:v1",
		Status:    imagepush.StatusQueued,
		CreatedAt: now,
	}}
	svc := &ApiService{PushManager: fake}

	insecure := true
	username, password, token := "pusher", "hunter2", "bearer-tok"
	resp, err := svc.CreatePush(context.Background(), oapi.CreatePushRequestObject{
		Body: &oapi.CreatePushRequest{
			Image:    "alpine:latest",
			Target:   "registry.example.com/app:v1",
			Insecure: &insecure,
			Credentials: &oapi.PushCredentials{
				Username:      &username,
				Password:      &password,
				RegistryToken: &token,
			},
		},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.CreatePush202JSONResponse{}, resp)

	got := resp.(oapi.CreatePush202JSONResponse)
	require.Equal(t, "push-1", got.Id)
	require.Equal(t, oapi.PushStatus(imagepush.StatusQueued), got.Status)
	require.Equal(t, now, got.CreatedAt)

	// Borrowed credentials must reach the manager as an auth config.
	require.Equal(t, "alpine:latest", fake.createdReq.Image)
	require.True(t, fake.createdReq.Insecure)
	require.NotNil(t, fake.createdReq.Credentials)
	require.Equal(t, &authn.AuthConfig{
		Username:      "pusher",
		Password:      "hunter2",
		RegistryToken: "bearer-tok",
	}, fake.createdReq.Credentials)
}

func TestCreatePush_NoCredentialsStaysNil(t *testing.T) {
	t.Parallel()

	fake := &fakePushManager{push: &imagepush.Push{ID: "push-1", Status: imagepush.StatusQueued}}
	svc := &ApiService{PushManager: fake}

	resp, err := svc.CreatePush(context.Background(), oapi.CreatePushRequestObject{
		Body: &oapi.CreatePushRequest{Image: "alpine:latest", Target: "registry.example.com/app:v1"},
	})
	require.NoError(t, err)
	require.IsType(t, oapi.CreatePush202JSONResponse{}, resp)
	require.Nil(t, fake.createdReq.Credentials)
}

func TestCreatePush_ErrorStatusMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		err      error
		wantType any
		wantCode string
	}{
		{
			name:     "invalid name -> 400",
			err:      fmt.Errorf("lookup: %w", images.ErrInvalidName),
			wantType: oapi.CreatePush400JSONResponse{},
			wantCode: "invalid_name",
		},
		{
			name:     "invalid target -> 400",
			err:      fmt.Errorf("parse: %w", imagepush.ErrInvalidTarget),
			wantType: oapi.CreatePush400JSONResponse{},
			wantCode: "invalid_target",
		},
		{
			name:     "image not found -> 404",
			err:      fmt.Errorf("lookup: %w", images.ErrNotFound),
			wantType: oapi.CreatePush404JSONResponse{},
			wantCode: "not_found",
		},
		{
			name:     "image not ready -> 409",
			err:      fmt.Errorf("lookup: %w", imagepush.ErrImageNotReady),
			wantType: oapi.CreatePush409JSONResponse{},
			wantCode: "image_not_ready",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := &ApiService{PushManager: &fakePushManager{createErr: tc.err}}

			resp, err := svc.CreatePush(context.Background(), oapi.CreatePushRequestObject{
				Body: &oapi.CreatePushRequest{Image: "alpine:latest", Target: "registry.example.com/app:v1"},
			})
			require.NoError(t, err)
			require.IsType(t, tc.wantType, resp)
			require.Equal(t, tc.wantCode, pushErrorCodeOf(resp))
		})
	}
}

func TestGetPush_NotFound(t *testing.T) {
	t.Parallel()

	svc := &ApiService{PushManager: &fakePushManager{getErr: imagepush.ErrNotFound}}
	resp, err := svc.GetPush(context.Background(), oapi.GetPushRequestObject{Id: "missing"})
	require.NoError(t, err)
	require.IsType(t, oapi.GetPush404JSONResponse{}, resp)
	require.Equal(t, "not_found", pushErrorCodeOf(resp))
}

func TestGetPush_OmitsEmptyCounters(t *testing.T) {
	t.Parallel()

	fake := &fakePushManager{push: &imagepush.Push{
		ID:     "push-1",
		Status: imagepush.StatusQueued,
	}}
	svc := &ApiService{PushManager: fake}
	resp, err := svc.GetPush(context.Background(), oapi.GetPushRequestObject{Id: "push-1"})
	require.NoError(t, err)
	got, ok := resp.(oapi.GetPush200JSONResponse)
	require.True(t, ok)
	require.Nil(t, got.Layers)
	require.Nil(t, got.Bytes)
}

func TestListPushes_Empty(t *testing.T) {
	t.Parallel()

	svc := &ApiService{PushManager: &fakePushManager{}}
	resp, err := svc.ListPushes(context.Background(), oapi.ListPushesRequestObject{})
	require.NoError(t, err)
	got, ok := resp.(oapi.ListPushes200JSONResponse)
	require.True(t, ok)
	require.Empty(t, got)
}

func TestListPushes_ReturnsAll(t *testing.T) {
	t.Parallel()

	fake := &fakePushManager{pushes: []imagepush.Push{
		{ID: "push-2", Status: imagepush.StatusPushed, Layers: 3, Bytes: 1024},
		{ID: "push-1", Status: imagepush.StatusFailed},
	}}
	svc := &ApiService{PushManager: fake}
	resp, err := svc.ListPushes(context.Background(), oapi.ListPushesRequestObject{})
	require.NoError(t, err)
	got, ok := resp.(oapi.ListPushes200JSONResponse)
	require.True(t, ok)
	require.Len(t, got, 2)
	require.Equal(t, "push-2", got[0].Id)
	require.NotNil(t, got[0].Layers)
	require.Equal(t, 3, *got[0].Layers)
	require.NotNil(t, got[0].Bytes)
	require.Equal(t, int64(1024), *got[0].Bytes)
}

// pushErrorCodeOf extracts the Code field from any CreatePush/GetPush error response.
func pushErrorCodeOf(resp any) string {
	switch r := resp.(type) {
	case oapi.CreatePush400JSONResponse:
		return r.Code
	case oapi.CreatePush404JSONResponse:
		return r.Code
	case oapi.CreatePush409JSONResponse:
		return r.Code
	case oapi.GetPush404JSONResponse:
		return r.Code
	default:
		return ""
	}
}
