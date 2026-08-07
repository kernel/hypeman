//go:build darwin

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/hypervisor"
)

func requireBuilderIntegrationHost(t *testing.T) {
	t.Helper()
	if os.Getenv("HYPEMAN_RUN_BUILDER_INTEGRATION_TEST") != "1" {
		t.Skip("set HYPEMAN_RUN_BUILDER_INTEGRATION_TEST=1 to run the VZ builder integration test")
	}
	if runtime.GOARCH != "arm64" {
		t.Skip("VZ builder integration test requires Apple Silicon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("builder integration test requires Docker")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("builder integration test requires a running Docker daemon")
	}
}

func builderIntegrationDataDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hb-")
	if err != nil {
		t.Fatalf("create short builder integration data directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func builderIntegrationPlatformConfig(t *testing.T) (config.NetworkConfig, hypervisor.Type) {
	t.Helper()
	// Deliberately supply the Linux-shaped defaults from issue #358. VZ must
	// still use its platform-effective shared NAT for the builder VM and registry.
	return config.NetworkConfig{
		BridgeName:    "vmbr0",
		SubnetCIDR:    "10.100.0.0/16",
		SubnetGateway: "10.100.0.1",
		DNSServer:     "8.8.8.8",
	}, hypervisor.TypeVZ
}

func builderIntegrationDockerSocket(t *testing.T) string {
	t.Helper()
	if socket := unixDockerSocket(os.Getenv("DOCKER_HOST")); socket != "" {
		return socket
	}
	if output, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output(); err == nil {
		if socket := unixDockerSocket(strings.TrimSpace(string(output))); socket != "" {
			return socket
		}
	}
	home, _ := os.UserHomeDir()
	for _, candidate := range []string{
		"/var/run/docker.sock",
		filepath.Join(home, ".colima", "default", "docker.sock"),
		filepath.Join(home, ".docker", "run", "docker.sock"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Fatal("builder integration test requires a local Docker Unix socket")
	return ""
}

func unixDockerSocket(host string) string {
	if strings.HasPrefix(host, "unix://") {
		return strings.TrimPrefix(host, "unix://")
	}
	if strings.HasPrefix(host, "/") {
		return host
	}
	return ""
}

func prepareBuilderIntegrationRegistryAccess(t *testing.T, bridge string) {
	t.Helper()
	// VZ's shared NAT can reach host listeners through its gateway without a
	// host firewall rule managed by Hypeman.
}
