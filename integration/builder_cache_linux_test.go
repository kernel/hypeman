//go:build linux

package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

func requireBuilderIntegrationHost(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("builder integration test requires root on Linux")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("builder integration test requires /dev/kvm")
	}
}

func builderIntegrationDataDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func builderIntegrationPlatformConfig(t *testing.T) (config.NetworkConfig, hypervisor.Type) {
	t.Helper()
	return newParallelTestNetworkConfig(t), hypervisor.TypeCloudHypervisor
}

func builderIntegrationDockerSocket(t *testing.T) string {
	t.Helper()
	return "/var/run/docker.sock"
}

func prepareBuilderIntegrationRegistryAccess(t *testing.T, bridge string) {
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
