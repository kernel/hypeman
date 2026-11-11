package images

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

func TestDockerHubSource(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		image, mirror, want string
		credentials         *authn.AuthConfig
	}{
		{"alpine:3.21", "cache.example/dockerhub", "cache.example/dockerhub/library/alpine:3.21", nil},
		{"docker.io/onkernel/base@" + digest, "cache.example/dockerhub", "cache.example/dockerhub/onkernel/base@" + digest, nil},
		{"ghcr.io/example/base:latest", "cache.example/dockerhub", "ghcr.io/example/base:latest", nil},
		{"alpine:3.21", "", "index.docker.io/library/alpine:3.21", nil},
		{"alpine:3.21", "cache.example/dockerhub", "index.docker.io/library/alpine:3.21", &authn.AuthConfig{Username: "caller", Password: "synthetic-secret"}},
	} {
		source, err := name.ParseReference(tc.image)
		require.NoError(t, err)
		ref, err := dockerHubSource(source, tc.mirror, tc.credentials)
		require.NoError(t, err)
		require.Equal(t, tc.want, ref.Name())
	}
	for _, invalid := range []string{"https://cache.example/dockerhub", "dockerhub", "cache.example/user@secret"} {
		_, err := dockerHubSource(name.MustParseReference("alpine"), invalid, nil)
		require.Error(t, err)
	}
}

func TestDockerHubMirrorPullAndBuildPrefetch(t *testing.T) {
	upstream := httptest.NewServer(registry.New())
	defer upstream.Close()
	local := httptest.NewServer(registry.New())
	defer local.Close()
	prefix := strings.TrimPrefix(upstream.URL, "http://") + "/dockerhub"
	img, err := random.Image(256, 1)
	require.NoError(t, err)
	digest, err := img.Digest()
	require.NoError(t, err)
	seed, err := name.ParseReference(prefix + "/library/alpine:3.21")
	require.NoError(t, err)
	require.NoError(t, remote.Write(seed, img))
	client, err := newOCIClient(t.TempDir(), WithDockerHubMirror(prefix))
	require.NoError(t, err)
	got, err := client.inspectManifest(context.Background(), "alpine:3.21")
	require.NoError(t, err)
	require.Equal(t, digest.String(), got)
	require.NoError(t, client.pullToOCILayout(context.Background(), "alpine@"+got, digestToLayoutTag(got)))

	result, err := MirrorBaseImage(context.Background(), local.URL, MirrorRequest{
		SourceImage: "alpine@" + got, DockerHubMirror: prefix,
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "library/alpine@"+got, result.LocalRef)
	destination, err := name.ParseReference(strings.TrimPrefix(local.URL, "http://") + "/" + result.LocalRef)
	require.NoError(t, err)
	cached, err := remote.Get(destination)
	require.NoError(t, err)
	require.Equal(t, digest, cached.Digest)
}
