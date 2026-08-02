package api

import (
	"bytes"
	"context"
	"mime/multipart"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/builds"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/scopes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubBuildManager implements builds.Manager for handler tests.
type stubBuildManager struct {
	createReq    *builds.CreateBuildRequest
	createSource []byte
}

func (m *stubBuildManager) Start(ctx context.Context) error { return nil }

func (m *stubBuildManager) CreateBuild(ctx context.Context, req builds.CreateBuildRequest, sourceData []byte) (*builds.Build, error) {
	m.createReq = &req
	m.createSource = sourceData
	return &builds.Build{ID: "build-1", Status: builds.StatusQueued}, nil
}

func (m *stubBuildManager) GetBuild(ctx context.Context, id string) (*builds.Build, error) {
	return nil, builds.ErrNotFound
}

func (m *stubBuildManager) ListBuilds(ctx context.Context) ([]*builds.Build, error) {
	return []*builds.Build{}, nil
}

func (m *stubBuildManager) CancelBuild(ctx context.Context, id string) error { return nil }

func (m *stubBuildManager) GetBuildLogs(ctx context.Context, id string) ([]byte, error) {
	return nil, nil
}

func (m *stubBuildManager) StreamBuildEvents(ctx context.Context, id string, follow bool) (<-chan builds.BuildEvent, error) {
	ch := make(chan builds.BuildEvent)
	close(ch)
	return ch, nil
}

func (m *stubBuildManager) RecoverPendingBuilds() {}

// createBuildRequest builds a multipart CreateBuild request object with the
// given form fields.
func createBuildRequest(t *testing.T, fields map[string]string) oapi.CreateBuildRequestObject {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	src, err := w.CreateFormFile("source", "source.tar.gz")
	require.NoError(t, err)
	_, err = src.Write([]byte("fake-tarball-data"))
	require.NoError(t, err)

	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	require.NoError(t, w.Close())

	return oapi.CreateBuildRequestObject{
		Body: multipart.NewReader(&buf, w.Boundary()),
	}
}

func TestResolveEffectiveCacheScope(t *testing.T) {
	// Admin with explicit scope: used as-is
	scope, err := resolveEffectiveCacheScope("user-alice", "my-team", true, false)
	require.NoError(t, err)
	assert.Equal(t, "my-team", scope)

	// Admin with invalid explicit scope: rejected
	_, err = resolveEffectiveCacheScope("user-alice", "ab", true, false)
	require.Error(t, err)

	// Admin without scope, derivation enabled: derived from identity
	scope, err = resolveEffectiveCacheScope("user-alice", "", true, true)
	require.NoError(t, err)
	assert.Equal(t, builds.DeriveCacheScope("user-alice"), scope)

	// Ordinary caller, derivation enabled: derived from identity, supplied
	// scope never reaches here
	scope, err = resolveEffectiveCacheScope("user-bob", "", false, true)
	require.NoError(t, err)
	assert.Equal(t, builds.DeriveCacheScope("user-bob"), scope)
	assert.NotEqual(t, builds.DeriveCacheScope("user-alice"), scope)

	// Derivation disabled (default): no cache scope even with an identity
	scope, err = resolveEffectiveCacheScope("user-bob", "", false, false)
	require.NoError(t, err)
	assert.Empty(t, scope)
	scope, err = resolveEffectiveCacheScope("user-alice", "", true, false)
	require.NoError(t, err)
	assert.Empty(t, scope)

	// No identity: no cache scope
	scope, err = resolveEffectiveCacheScope("", "", false, true)
	require.NoError(t, err)
	assert.Empty(t, scope)
}

func TestCreateBuild_RejectsAdminBuildWithoutAdminScope(t *testing.T) {
	svc := &ApiService{BuildManager: &stubBuildManager{}}
	ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{scopes.BuildWrite})

	resp, err := svc.CreateBuild(ctx, createBuildRequest(t, map[string]string{
		"dockerfile":     "FROM alpine",
		"is_admin_build": "true",
	}))

	require.NoError(t, err)
	forbidden, ok := resp.(oapi.CreateBuild403JSONResponse)
	require.True(t, ok, "expected 403, got %T", resp)
	assert.Contains(t, forbidden.Message, "build:admin")
}

func TestCreateBuild_RejectsCacheScopeWithoutAdminScope(t *testing.T) {
	svc := &ApiService{BuildManager: &stubBuildManager{}}
	ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{scopes.BuildWrite})

	resp, err := svc.CreateBuild(ctx, createBuildRequest(t, map[string]string{
		"dockerfile":  "FROM alpine",
		"cache_scope": "my-team",
	}))

	require.NoError(t, err)
	forbidden, ok := resp.(oapi.CreateBuild403JSONResponse)
	require.True(t, ok, "expected 403, got %T", resp)
	assert.Contains(t, forbidden.Message, "build:admin")
}

func TestCreateBuild_AdminScopePassesThrough(t *testing.T) {
	mgr := &stubBuildManager{}
	svc := &ApiService{BuildManager: mgr}
	// Legacy full-access context (no permissions claim) counts as admin.
	ctx := context.Background()

	resp, err := svc.CreateBuild(ctx, createBuildRequest(t, map[string]string{
		"dockerfile":  "FROM alpine",
		"cache_scope": "my-team",
	}))

	require.NoError(t, err)
	_, ok := resp.(oapi.CreateBuild202JSONResponse)
	require.True(t, ok, "expected 202, got %T", resp)
	require.NotNil(t, mgr.createReq)
	assert.Equal(t, "my-team", mgr.createReq.CacheScope)
}

func TestCreateBuild_AdminScopeValidated(t *testing.T) {
	svc := &ApiService{BuildManager: &stubBuildManager{}}
	ctx := context.Background()

	resp, err := svc.CreateBuild(ctx, createBuildRequest(t, map[string]string{
		"dockerfile":  "FROM alpine",
		"cache_scope": "ab",
	}))

	require.NoError(t, err)
	badReq, ok := resp.(oapi.CreateBuild400JSONResponse)
	require.True(t, ok, "expected 400, got %T", resp)
	assert.Contains(t, badReq.Message, "cache_scope")
}

func TestCreateBuild_OrdinaryCallerNoScopeByDefault(t *testing.T) {
	mgr := &stubBuildManager{}
	// Nil config: tenant scope derivation is disabled by default.
	svc := &ApiService{BuildManager: mgr}
	ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{scopes.BuildWrite})

	resp, err := svc.CreateBuild(ctx, createBuildRequest(t, map[string]string{
		"dockerfile": "FROM alpine",
	}))

	require.NoError(t, err)
	_, ok := resp.(oapi.CreateBuild202JSONResponse)
	require.True(t, ok, "expected 202, got %T", resp)
	require.NotNil(t, mgr.createReq)
	// Derivation disabled: the scope stays empty regardless of identity,
	// preserving the pre-derivation default behavior.
	assert.Empty(t, mgr.createReq.CacheScope)
	assert.False(t, mgr.createReq.IsAdminBuild)
}

func TestCreateBuild_OrdinaryCallerGetsDerivedScopeWhenEnabled(t *testing.T) {
	mgr := &stubBuildManager{}
	svc := &ApiService{
		BuildManager: mgr,
		Config: &config.Config{
			Build: config.BuildConfig{
				Cache: config.BuildCacheConfig{DeriveTenantScope: true},
			},
		},
	}
	ctx := scopes.ContextWithPermissions(context.Background(), []scopes.Scope{scopes.BuildWrite})
	ctx = mw.ContextWithUserID(ctx, "user-bob")

	resp, err := svc.CreateBuild(ctx, createBuildRequest(t, map[string]string{
		"dockerfile": "FROM alpine",
	}))

	require.NoError(t, err)
	_, ok := resp.(oapi.CreateBuild202JSONResponse)
	require.True(t, ok, "expected 202, got %T", resp)
	require.NotNil(t, mgr.createReq)
	assert.Equal(t, builds.DeriveCacheScope("user-bob"), mgr.createReq.CacheScope)
}
