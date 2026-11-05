package builds

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/require"
)

func TestRewriteMirroredFROMs(t *testing.T) {
	const source = "registry.example.com/runtime:1"
	const target = "10.0.0.1:4973/registry.example.com/runtime@sha256:abc"
	replacements := map[string]string{source: target}
	for _, tc := range []struct{ input, want string }{
		{"FROM " + source + " AS base\nRUN echo ok\n", "FROM " + target + " AS base\nRUN echo ok\n"},
		{"  from --platform=linux/amd64  " + source + " as base # comment", "  from --platform=linux/amd64  " + target + " as base # comment"},
		{"FROM ubuntu:22.04\nFROM base\nFROM scratch\nFROM ${BASE}\n# FROM " + source, "FROM ubuntu:22.04\nFROM base\nFROM scratch\nFROM ${BASE}\n# FROM " + source},
	} {
		require.Equal(t, tc.want, rewriteMirroredFROMs(tc.input, replacements))
	}
}

type mirrorTestTransport func(*http.Request) (*http.Response, error)

func (f mirrorTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMirrorUnresolvedBuildArgKeepsFallback(t *testing.T) {
	mgr, _, _, dir := setupTestManager(t)
	defer os.RemoveAll(dir)
	err := mgr.mirrorBaseImagesForBuild(context.Background(), "variable-base", CreateBuildRequest{
		Dockerfile: "ARG BASE=alpine:latest\nFROM $BASE\n",
	})
	require.NoError(t, err)
}

func TestMirrorPrivateBaseKeepsCredentialsOutOfBuildConfig(t *testing.T) {
	const password = "private-source-test-password"
	handler := registry.New()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "reader" || pass != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer source.Close()
	destination := httptest.NewServer(registry.New())
	defer destination.Close()
	seed, err := name.ParseReference(strings.TrimPrefix(source.URL, "http://") + "/runtime:1")
	require.NoError(t, err)
	index, err := random.Index(32, 1, 2)
	require.NoError(t, err)
	require.NoError(t, remote.WriteIndex(seed, index, remote.WithAuth(&authn.Basic{Username: "reader", Password: password})))

	sourceURL, err := url.Parse(source.URL)
	require.NoError(t, err)
	transport := remote.DefaultTransport
	remote.DefaultTransport = mirrorTestTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		if clone.URL.Host == "private.example.com" {
			clone.URL.Host = sourceURL.Host
			clone.URL.Scheme = "http"
		}
		return transport.RoundTrip(clone)
	})
	t.Cleanup(func() { remote.DefaultTransport = transport })
	dockerConfig := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dockerConfig)
	auth := base64.StdEncoding.EncodeToString([]byte("reader:" + password))
	require.NoError(t, os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte(`{"auths":{"private.example.com":{"auth":"`+auth+`"}}}`), 0600))

	mgr, _, volumeManager, dir := setupTestManager(t)
	defer os.RemoveAll(dir)
	mgr.config.RegistryURL = destination.URL
	const id = "build-private-test"
	original := "FROM private.example.com/runtime:1\nRUN echo ready\n"
	require.NoError(t, writeBuildConfig(mgr.paths, id, &BuildConfig{Dockerfile: original, RegistryToken: "scoped-host-token"}))
	require.NoError(t, mgr.mirrorBaseImagesForBuild(context.Background(), id, CreateBuildRequest{Dockerfile: original}))
	config, err := readBuildConfig(mgr.paths, id)
	require.NoError(t, err)
	digest, err := index.Digest()
	require.NoError(t, err)
	target := strings.TrimPrefix(destination.URL, "http://") + "/private.example.com/runtime@" + digest.String()
	require.Equal(t, "FROM "+target+"\nRUN echo ready\n", config.Dockerfile)
	require.Equal(t, "scoped-host-token", config.RegistryToken)
	encoded, err := json.Marshal(config)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), password)
	ref, err := name.ParseReference(target)
	require.NoError(t, err)
	_, err = remote.Get(ref)
	require.NoError(t, err)

	sourcePath := filepath.Join(mgr.paths.BuildSourceDir(id), "source.tar.gz")
	require.NoError(t, os.MkdirAll(filepath.Dir(sourcePath), 0755))
	createTarball(t, sourcePath, map[string]string{"Dockerfile": original, "app.txt": "app"})
	captured := errors.New("captured source volume")
	volumeManager.createFromArchiveFunc = func(_ context.Context, _ volumes.CreateVolumeFromArchiveRequest, archive io.Reader) (*volumes.Volume, error) {
		data, err := io.ReadAll(archive)
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "source.tar.gz")
		require.NoError(t, os.WriteFile(path, data, 0600))
		got, err := ExtractDockerfileFromTarball(path)
		require.NoError(t, err)
		require.Equal(t, config.Dockerfile, got)
		return nil, captured
	}
	policy := DefaultBuildPolicy()
	_, err = mgr.executeBuild(context.Background(), id, CreateBuildRequest{Dockerfile: original}, &policy)
	require.ErrorIs(t, err, captured)
	stored, err := ExtractDockerfileFromTarball(sourcePath)
	require.NoError(t, err)
	require.Equal(t, original, stored)
}
