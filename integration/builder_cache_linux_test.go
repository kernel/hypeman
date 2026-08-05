package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/builders"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/registry"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuilderPersistentCacheReuse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if os.Geteuid() != 0 {
		t.Skip("builder integration test requires root")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("builder integration test requires /dev/kvm")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	p := paths.New(t.TempDir())
	cfg := &config.Config{
		DataDir: p.DataDir(),
		Network: newParallelTestNetworkConfig(t),
	}
	gateway, err := network.DeriveGateway(cfg.Network.SubnetCIDR)
	require.NoError(t, err)
	cfg.Network.SubnetGateway = gateway

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	volumeManager := volumes.NewManager(p, 0, nil)
	networkManager := network.NewManager(p, cfg, nil)
	require.NoError(t, networkManager.Initialize(ctx, nil))
	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))
	instanceManager := instances.NewManager(
		p,
		imageManager,
		systemManager,
		networkManager,
		devices.NewManager(p),
		volumeManager,
		instances.ResourceLimits{MaxOverlaySize: 100 << 30},
		"",
		instances.SnapshotPolicy{},
		nil,
		nil,
	)
	t.Cleanup(func() {
		all, listErr := instanceManager.ListInstances(context.Background(), nil)
		if listErr != nil {
			t.Logf("list instances during cleanup: %v", listErr)
			return
		}
		for _, instance := range all {
			if deleteErr := instanceManager.DeleteInstance(context.Background(), instance.Id); deleteErr != nil {
				t.Logf("delete instance %s during cleanup: %v", instance.Id, deleteErr)
			}
		}
	})

	registryURL := startBuildRegistry(t, gateway, p, imageManager)

	builderManager, err := builders.NewManager(
		p,
		builders.Config{DefaultDiskSizeGb: 4},
		volumeManager,
		instanceManager,
		nil,
		nil,
	)
	require.NoError(t, err)
	require.NoError(t, builderManager.Start(ctx))

	buildManager, err := builds.NewManager(
		p,
		builds.Config{
			MaxConcurrentBuilds: 1,
			RegistryURL:         registryURL,
			RegistryInsecure:    true,
			RegistrySecret:      "builder-cache-integration-test",
			DefaultTimeout:      600,
		},
		instanceManager,
		volumeManager,
		builderManager,
		imageManager,
		nil,
		nil,
		nil,
	)
	require.NoError(t, err)
	builderManager.SetBuildActivityChecker(buildManager.BuilderHasBuilds)
	require.NoError(t, buildManager.Start(ctx))
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		all, listErr := imageManager.ListImages(ctx)
		require.NoError(collect, listErr)
		ready := false
		for _, image := range all {
			if strings.Contains(image.Name, "/internal/builder") {
				ready = image.Status == images.StatusReady
			}
		}
		require.True(collect, ready)
	}, 5*time.Minute, time.Second)
	// ensureBuilderImage records the ready image in the build manager after conversion.
	time.Sleep(100 * time.Millisecond)

	builder, err := builderManager.CreateBuilder(ctx, builders.CreateBuilderRequest{DiskSizeGb: 4})
	require.NoError(t, err)

	dockerfile := `FROM alpine:3.18
ARG CACHE_BUSTER
RUN --mount=type=cache,target=/cache sh -c 'if [ -f /cache/sentinel ]; then echo BUILDER_CACHE_HIT; else echo BUILDER_CACHE_MISS; touch /cache/sentinel; fi; echo "$CACHE_BUSTER" > /cache-buster'
`
	source := sourceArchive(t, dockerfile)
	first := runBuilderBuild(t, ctx, buildManager, builder.ID, dockerfile, source, "first")
	firstLogs, err := buildManager.GetBuildLogs(ctx, first.ID)
	require.NoError(t, err)
	require.Contains(t, string(firstLogs), "BUILDER_CACHE_MISS")

	second := runBuilderBuild(t, ctx, buildManager, builder.ID, dockerfile, source, "second")
	secondLogs, err := buildManager.GetBuildLogs(ctx, second.ID)
	require.NoError(t, err)
	require.Contains(t, string(secondLogs), "BUILDER_CACHE_HIT")
	require.NotNil(t, first.BuilderInstanceID)
	require.NotNil(t, second.BuilderInstanceID)
	require.NotEqual(t, *first.BuilderInstanceID, *second.BuilderInstanceID)
}

func startBuildRegistry(t *testing.T, gateway string, p *paths.Paths, imageManager images.Manager) string {
	t.Helper()
	reg, err := registry.New(p, imageManager)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	server := &http.Server{Handler: reg.Handler()}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			t.Logf("registry server: %v", serveErr)
		}
	}()
	t.Cleanup(func() { _ = server.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	return net.JoinHostPort(gateway, strconv.Itoa(port))
}

func runBuilderBuild(t *testing.T, ctx context.Context, manager builds.Manager, builderID, dockerfile string, source []byte, cacheBuster string) *builds.Build {
	t.Helper()
	build, err := manager.CreateBuild(ctx, builds.CreateBuildRequest{
		Dockerfile: dockerfile,
		BuilderID:  builderID,
		BuildArgs:  map[string]string{"CACHE_BUSTER": cacheBuster},
	}, source)
	require.NoError(t, err)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		current, getErr := manager.GetBuild(ctx, build.ID)
		require.NoError(collect, getErr)
		require.Contains(collect, []string{builds.StatusReady, builds.StatusFailed}, current.Status)
	}, 10*time.Minute, time.Second)

	result, err := manager.GetBuild(ctx, build.ID)
	require.NoError(t, err)
	if result.Status != builds.StatusReady {
		logs, _ := manager.GetBuildLogs(ctx, build.ID)
		t.Fatalf("build %s failed: %v\n%s", build.ID, result.Error, logs)
	}
	return result
}

func sourceArchive(t *testing.T, dockerfile string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	contents := []byte(dockerfile)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Mode: 0644,
		Size: int64(len(contents)),
	}))
	_, err := tw.Write(contents)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return out.Bytes()
}
