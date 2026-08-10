package api

import (
	"bytes"
	"mime/multipart"
	"testing"

	"github.com/kernel/hypeman/lib/oapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildMultipartBody builds a POST /builds multipart body from small form
// fields plus the source tarball part.
func buildMultipartBody(t *testing.T, fields map[string]string, source []byte) *oapi.CreateBuildRequestObject {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for k, v := range fields {
		require.NoError(t, writer.WriteField(k, v))
	}
	if source != nil {
		part, err := writer.CreateFormFile("source", "source.tar.gz")
		require.NoError(t, err)
		_, err = part.Write(source)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	return &oapi.CreateBuildRequestObject{
		Body: multipart.NewReader(&body, writer.Boundary()),
	}
}

// overrideBuildUploadLimits shrinks the upload limits for the test and
// restores them on cleanup. Tests using it must not run in parallel.
func overrideBuildUploadLimits(t *testing.T, sourceSize, fieldSize int64) {
	t.Helper()
	origSource, origField := maxBuildSourceSize, maxBuildFormFieldSize
	maxBuildSourceSize, maxBuildFormFieldSize = sourceSize, fieldSize
	t.Cleanup(func() {
		maxBuildSourceSize, maxBuildFormFieldSize = origSource, origField
	})
}

func TestCreateBuild_SourceAtLimitAccepted(t *testing.T) {
	svc := newTestService(t)
	overrideBuildUploadLimits(t, 1024, 1024)

	resp, err := svc.CreateBuild(ctx(), *buildMultipartBody(t, nil, bytes.Repeat([]byte("a"), 1024)))
	require.NoError(t, err)
	_, ok := resp.(oapi.CreateBuild202JSONResponse)
	assert.True(t, ok, "expected 202 for source exactly at the limit, got %T", resp)

	// Wait for the build goroutine to finish writing before TempDir cleanup.
	require.NoError(t, svc.BuildManager.Shutdown(ctx()))
}

func TestCreateBuild_SourceOverLimitRejected(t *testing.T) {
	svc := newTestService(t)
	overrideBuildUploadLimits(t, 1024, 1024)

	resp, err := svc.CreateBuild(ctx(), *buildMultipartBody(t, nil, bytes.Repeat([]byte("a"), 1025)))
	require.NoError(t, err)
	r, ok := resp.(oapi.CreateBuild400JSONResponse)
	require.True(t, ok, "expected 400 for oversized source, got %T", resp)
	assert.Equal(t, "invalid_source", r.Code)
	assert.Contains(t, r.Message, "source exceeds the maximum size")
}

func TestCreateBuild_FormFieldAtLimitAccepted(t *testing.T) {
	svc := newTestService(t)
	overrideBuildUploadLimits(t, 1024, 1024)

	fields := map[string]string{"dockerfile": string(bytes.Repeat([]byte("a"), 1024))}
	resp, err := svc.CreateBuild(ctx(), *buildMultipartBody(t, fields, []byte("source")))
	require.NoError(t, err)
	_, ok := resp.(oapi.CreateBuild202JSONResponse)
	assert.True(t, ok, "expected 202 for field exactly at the limit, got %T", resp)

	// Wait for the build goroutine to finish writing before TempDir cleanup.
	require.NoError(t, svc.BuildManager.Shutdown(ctx()))
}

func TestCreateBuild_FormFieldOverLimitRejected(t *testing.T) {
	svc := newTestService(t)
	overrideBuildUploadLimits(t, 1024, 1024)

	fields := map[string]string{"dockerfile": string(bytes.Repeat([]byte("a"), 1025))}
	resp, err := svc.CreateBuild(ctx(), *buildMultipartBody(t, fields, []byte("source")))
	require.NoError(t, err)
	r, ok := resp.(oapi.CreateBuild400JSONResponse)
	require.True(t, ok, "expected 400 for oversized field, got %T", resp)
	assert.Equal(t, "invalid_request", r.Code)
	assert.Contains(t, r.Message, "dockerfile exceeds the maximum size")
}

func TestCreateBuild_InvalidSecretIDRejected(t *testing.T) {
	svc := newTestService(t)

	for _, secrets := range []string{
		`[{"id": "../escape"}]`,
		`[{"id": "a/b"}]`,
		`[{"id": ""}]`,
	} {
		fields := map[string]string{"secrets": secrets}
		resp, err := svc.CreateBuild(ctx(), *buildMultipartBody(t, fields, []byte("source")))
		require.NoError(t, err)
		r, ok := resp.(oapi.CreateBuild400JSONResponse)
		require.True(t, ok, "expected 400 for secrets %s, got %T", secrets, resp)
		assert.Equal(t, "invalid_request", r.Code)
		assert.Contains(t, r.Message, "invalid secret id")
	}
}

func TestCreateBuild_ValidSecretsAccepted(t *testing.T) {
	svc := newTestService(t)

	fields := map[string]string{"secrets": `[{"id": "npm_token", "env_var": "NPM_TOKEN"}]`}
	resp, err := svc.CreateBuild(ctx(), *buildMultipartBody(t, fields, []byte("source")))
	require.NoError(t, err)
	_, ok := resp.(oapi.CreateBuild202JSONResponse)
	assert.True(t, ok, "expected 202 for valid secrets, got %T", resp)

	// Wait for the build goroutine to finish writing before TempDir cleanup.
	require.NoError(t, svc.BuildManager.Shutdown(ctx()))
}
