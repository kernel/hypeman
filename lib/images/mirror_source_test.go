package images

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

func TestMirrorBaseImageSourceRepository(t *testing.T) {
	protectedRegistry := func(username, password string) *httptest.Server {
		handler := registry.New()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, _ := r.BasicAuth()
			if u != username || p != password {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			handler.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)
		return server
	}
	src := protectedRegistry("AWS", "synthetic-source-token")
	dst := protectedRegistry("local-build-token", "x")
	srcHost := strings.TrimPrefix(src.URL, "http://")
	dstHost := strings.TrimPrefix(dst.URL, "http://")
	dir := t.TempDir()
	config, err := json.Marshal(map[string]any{"auths": map[string]any{
		srcHost: map[string]string{"username": "AWS", "password": "synthetic-source-token"},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), config, 0600))
	t.Setenv("DOCKER_CONFIG", dir)
	index, err := random.Index(256, 1, 2)
	require.NoError(t, err)
	digest, err := index.Digest()
	require.NoError(t, err)
	seed, err := name.ParseReference(srcHost + "/kernel/node-base:1.2.3")
	require.NoError(t, err)
	require.NoError(t, remote.WriteIndex(seed, index, remote.WithAuth(authn.FromConfig(authn.AuthConfig{
		Username: "AWS", Password: "synthetic-source-token",
	}))))
	localAuth := &authn.AuthConfig{Username: "local-build-token", Password: "x"}
	for _, suffix := range []string{":1.2.3", "@" + digest.String()} {
		result, err := MirrorBaseImage(context.Background(), dstHost, MirrorRequest{
			SourceImage:      "docker.io/example/node-base" + suffix,
			SourceRepository: srcHost + "/kernel/node-base",
		}, localAuth)
		require.NoError(t, err)
		require.Equal(t, "example/node-base"+suffix, result.LocalRef)
		ref, err := name.ParseReference(dstHost + "/" + result.LocalRef)
		require.NoError(t, err)
		mirrored, err := remote.Get(ref, remote.WithAuth(authn.FromConfig(*localAuth)))
		require.NoError(t, err)
		if strings.HasPrefix(suffix, "@") {
			require.Equal(t, digest, mirrored.Digest)
			require.True(t, mirrored.MediaType.IsIndex())
		}
	}
}
