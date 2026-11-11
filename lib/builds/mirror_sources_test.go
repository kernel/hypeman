package builds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeBaseImageSources(t *testing.T) {
	sources, err := normalizeBaseImageSources(map[string]string{
		"docker.io/example/node-base": "registry.example/kernel/node-base",
	})
	require.NoError(t, err)
	require.Equal(t, "registry.example/kernel/node-base", sources["index.docker.io/example/node-base"])
	for _, invalid := range []map[string]string{
		{"example/node-base:latest": "registry.example/base"},
		{"example/node-base": "registry.example/base:latest"},
		{"example/node-base": ""},
		{"example/node-base": "registry.example/a", "docker.io/example/node-base": "registry.example/b"},
	} {
		_, err := normalizeBaseImageSources(invalid)
		require.Error(t, err)
	}
}

func TestRequiredBaseImageFailureStopsBuild(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	mgr, _, _, _, dir := setupTestManagerWithImageMgr(t)
	defer os.RemoveAll(dir)
	var err error
	mgr.config.BaseImageSources, err = normalizeBaseImageSources(map[string]string{
		"example/node-base": strings.TrimPrefix(upstream.URL, "http://") + "/kernel/node-base",
	})
	require.NoError(t, err)
	const id = "required-base"
	require.NoError(t, os.MkdirAll(mgr.paths.BuildSourceDir(id), 0755))
	createTarball(t, mgr.paths.BuildSourceDir(id)+"/source.tar.gz", map[string]string{
		"Dockerfile": "FROM example/node-base:1.2.3\n",
	})
	require.NoError(t, writeMetadata(mgr.paths, &buildMetadata{ID: id, Status: StatusQueued}))
	// A VM launch would panic. The archived Dockerfile must win over the inline scratch image.
	mgr.instanceManager = nil
	mgr.runBuild(context.Background(), id, CreateBuildRequest{Dockerfile: "FROM scratch\n"}, &BuildPolicy{TimeoutSeconds: 5})
	meta, err := readMetadata(mgr.paths, id)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, meta.Status)
	require.Contains(t, *meta.Error, "prefetch required base image")

	// Other repositories retain best-effort prefetch behavior.
	mgr.config.BaseImageSources = nil
	require.NoError(t, os.MkdirAll(mgr.paths.BuildSourceDir(id), 0755))
	createTarball(t, mgr.paths.BuildSourceDir(id)+"/source.tar.gz", map[string]string{
		"Dockerfile": "FROM " + strings.TrimPrefix(upstream.URL, "http://") + "/missing:latest\n",
	})
	require.NoError(t, mgr.mirrorBaseImagesForBuild(context.Background(), id, CreateBuildRequest{}))
}
