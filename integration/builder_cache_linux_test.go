package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	t.Chdir("..")

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
	allowHostRegistryTraffic(t, cfg.Network.BridgeName)
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

	registryURL, registryCA := startBuildRegistry(t, gateway, p, imageManager)

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
			RegistryCACert:      registryCA,
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
	// ensureBuilderImage records the ready image on its next 500ms poll.
	time.Sleep(600 * time.Millisecond)

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

func startBuildRegistry(t *testing.T, gateway string, p *paths.Paths, imageManager images.Manager) (string, string) {
	t.Helper()
	reg, err := registry.New(p, imageManager)
	require.NoError(t, err)
	certPEM, keyPEM := registryCertificate(t, net.ParseIP(gateway))
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	require.NoError(t, err)
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	server := &http.Server{Handler: reg.Handler()}
	go func() {
		if serveErr := server.Serve(tlsListener); serveErr != nil && serveErr != http.ErrServerClosed {
			t.Logf("registry server: %v", serveErr)
		}
	}()
	t.Cleanup(func() { _ = server.Close() })
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	return net.JoinHostPort(gateway, port), string(certPEM)
}

func allowHostRegistryTraffic(t *testing.T, bridge string) {
	t.Helper()
	if exec.Command("nft", "list", "table", "inet", "kernel_firewall").Run() != nil {
		return
	}
	comment := "hypeman-builder-cache-test-" + bridge
	output, err := exec.Command("nft", "insert", "rule", "inet", "kernel_firewall", "input",
		"iifname", bridge, "accept", "comment", comment).CombinedOutput()
	require.NoErrorf(t, err, "allow registry traffic: %s", output)
	t.Cleanup(func() {
		output, err := exec.Command("nft", "-a", "list", "chain", "inet", "kernel_firewall", "input").Output()
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(output), "\n") {
			if !strings.Contains(line, comment) {
				continue
			}
			fields := strings.Fields(line)
			_ = exec.Command("nft", "delete", "rule", "inet", "kernel_firewall", "input", "handle", fields[len(fields)-1]).Run()
		}
	})
}

func registryCertificate(t *testing.T, ip net.IP) ([]byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: ip.String()},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
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
