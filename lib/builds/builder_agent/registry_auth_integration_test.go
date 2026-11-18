package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/kernel/hypeman/lib/builds/registryauth"
	"github.com/stretchr/testify/require"
)

func TestBuildKitUsesBorrowedRegistryConfig(t *testing.T) {
	if os.Getenv("HYPEMAN_BUILD_AUTH_DOCKER_TEST") != "1" {
		t.Skip("set HYPEMAN_BUILD_AUTH_DOCKER_TEST=1 with Docker available")
	}
	credential := registryauth.Credential{Username: "AWS", Password: "synthetic-registry-password"}
	handler := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		if user != credential.Username || password != credential.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	ref, err := name.ParseReference(host + "/private/base:latest")
	require.NoError(t, err)
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{Architecture: runtime.GOARCH, OS: "linux"})
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img, remote.WithAuth(authn.FromConfig(authn.AuthConfig{Username: credential.Username, Password: credential.Password}))))
	contextDir, authDir := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM "+ref.Name()+"\n"), 0600))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tag := "hypeman-registry-auth-test:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	build := func() ([]byte, error) {
		cmd := exec.CommandContext(ctx, "docker", "build", "--progress=plain", "--no-cache", "--pull", "-t", tag, contextDir)
		cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+authDir)
		return cmd.CombinedOutput()
	}
	_, err = build()
	require.Error(t, err, "private base must reject unauthenticated builds")
	config, err := json.Marshal(map[string]any{"auths": registryAuthEntries("local.example:4973", "local-token", map[string]registryauth.Credential{host: credential})})
	require.NoError(t, err)
	require.NoError(t, writeRegistryConfig(authDir, config))
	defer exec.Command("docker", "image", "rm", "-f", tag).Run()
	output, err := build()
	require.NoError(t, err, "%s", output)
	require.NotContains(t, string(output), credential.Password)
	require.NotContains(t, string(output), credential.Auth())
	inspect, err := exec.CommandContext(ctx, "docker", "image", "inspect", tag).Output()
	require.NoError(t, err)
	require.NotContains(t, string(inspect), credential.Password)
	require.NotContains(t, string(inspect), credential.Auth())
}
