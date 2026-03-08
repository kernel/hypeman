package instances

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/require"
)

func TestEgressProxyRewritesHTTPSHeaders(t *testing.T) {
	requireKVMAccess(t)

	manager, _ := setupTestManager(t)
	ctx := context.Background()

	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, r.Header.Get("Authorization"))
	}))
	defer target.Close()

	imageRef := integrationTestImageRef(t, "docker.io/library/nginx:alpine")
	t.Logf("Pulling %s image...", imageRef)
	created, err := manager.imageManager.CreateImage(ctx, images.CreateImageRequest{Name: imageRef})
	require.NoError(t, err)

	for i := 0; i < 120; i++ {
		img, err := manager.imageManager.GetImage(ctx, created.Name)
		if err == nil && img.Status == images.StatusReady {
			break
		}
		time.Sleep(1 * time.Second)
	}
	img, err := manager.imageManager.GetImage(ctx, created.Name)
	require.NoError(t, err)
	require.Equal(t, images.StatusReady, img.Status)

	require.NoError(t, manager.systemManager.EnsureSystemFiles(ctx))
	require.NoError(t, manager.networkManager.Initialize(ctx, nil))

	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-egress-proxy",
		Image:          imageRef,
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		EgressProxy: &EgressProxyConfig{
			Enabled:     true,
			MockEnvVars: []string{"OUTBOUND_OPENAI_KEY"},
		},
		Env: map[string]string{
			"OUTBOUND_OPENAI_KEY": "real-openai-key-123",
		},
		Entrypoint: []string{"/bin/sh", "-lc"},
		Cmd:        []string{"sleep 3600"},
	})
	require.NoError(t, err)

	deleted := false
	t.Cleanup(func() {
		if !deleted {
			_ = manager.DeleteInstance(context.Background(), inst.Id)
		}
	})

	require.NoError(t, waitForVMReady(ctx, inst.SocketPath, 10*time.Second))
	require.NoError(t, waitForLogMessage(ctx, manager, inst.Id, "[guest-agent] listening", 45*time.Second))

	envOutput, envExitCode, err := execCommand(ctx, inst, "sh", "-lc", "printf '%s' \"$OUTBOUND_OPENAI_KEY\"")
	require.NoError(t, err)
	require.Equal(t, 0, envExitCode)
	require.Equal(t, "mock-OUTBOUND_OPENAI_KEY", envOutput)

	cmd := fmt.Sprintf(
		"NO_PROXY= no_proxy= curl -k -sS -H \"Authorization: Bearer $OUTBOUND_OPENAI_KEY\" %s",
		target.URL,
	)
	output, exitCode, err := execCommand(ctx, inst, "sh", "-lc", cmd)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "curl output: %s", output)
	require.Contains(t, output, "Bearer real-openai-key-123")
	require.NotContains(t, output, "mock_openai_key")

	require.NoError(t, manager.DeleteInstance(ctx, inst.Id))
	deleted = true
}
