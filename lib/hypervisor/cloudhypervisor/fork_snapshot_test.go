package cloudhypervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewriteSnapshotConfigForFork(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.json")

	orig := map[string]any{
		"disks": []any{
			map[string]any{"path": "/src-data/guests/a/overlay.raw"},
			map[string]any{"path": "/src-data/images/docker.io/library/alpine/sha256abcdef/rootfs.erofs"},
		},
		"serial": map[string]any{"file": "/src/guests/a/logs/app.log"},
		"vsock":  map[string]any{"cid": float64(100), "socket": "/src/guests/a/vsock.sock"},
		"payload": map[string]any{
			"kernel": "/src-data/system/kernel/k1/x86_64/vmlinux",
			"initrd": "/src-data/system/initrd/x86_64/latest/initrd",
		},
		"metadata": map[string]any{
			"note": "keep-/src/guests/a-as-substring",
		},
		"net": []any{map[string]any{
			"tap":  "hype-old",
			"ip":   "10.0.0.10",
			"mac":  "02:00:00:00:00:01",
			"mask": "255.255.255.0",
		}},
	}
	data, err := json.Marshal(orig)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, data, 0644))

	err = rewriteSnapshotConfigForFork(configPath, hypervisor.ForkPrepareRequest{
		SourceDataDir: "/src-data/guests/a",
		TargetDataDir: "/dst-data/guests/b",
		VsockCID:      200,
		VsockSocket:   "/dst-data/guests/b/vsock.sock",
		SerialLogPath: "/dst-data/guests/b/logs/app.log",
		Network: &hypervisor.ForkNetworkConfig{
			TAPDevice: "hype-new",
			IP:        "10.0.0.20",
			MAC:       "02:00:00:00:00:02",
			Netmask:   "255.255.255.0",
		},
	})
	require.NoError(t, err)

	updatedData, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var updated map[string]any
	require.NoError(t, json.Unmarshal(updatedData, &updated))

	disks := updated["disks"].([]any)
	disk0 := disks[0].(map[string]any)
	assert.Equal(t, "/dst-data/guests/b/overlay.raw", disk0["path"])
	disk1 := disks[1].(map[string]any)
	assert.Equal(t, "/dst-data/images/docker.io/library/alpine/sha256abcdef/rootfs.erofs", disk1["path"])

	serial := updated["serial"].(map[string]any)
	assert.Equal(t, "/dst-data/guests/b/logs/app.log", serial["file"])

	vsock := updated["vsock"].(map[string]any)
	assert.Equal(t, float64(100), vsock["cid"])
	assert.Equal(t, "/dst-data/guests/b/vsock.sock", vsock["socket"])

	payload := updated["payload"].(map[string]any)
	assert.Equal(t, "/dst-data/system/kernel/k1/x86_64/vmlinux", payload["kernel"])
	assert.Equal(t, "/dst-data/system/initrd/x86_64/latest/initrd", payload["initrd"])

	netCfg := updated["net"].([]any)[0].(map[string]any)
	assert.Equal(t, "hype-new", netCfg["tap"])
	assert.Equal(t, "10.0.0.20", netCfg["ip"])
	assert.Equal(t, "02:00:00:00:00:02", netCfg["mac"])
	assert.Equal(t, "255.255.255.0", netCfg["mask"])

	metadata := updated["metadata"].(map[string]any)
	assert.Equal(t, "keep-/src/guests/a-as-substring", metadata["note"])
}
