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
		"disks":  []any{map[string]any{"path": "/src/guests/a/overlay.raw"}},
		"serial": map[string]any{"file": "/src/guests/a/logs/app.log"},
		"vsock":  map[string]any{"cid": float64(100), "socket": "/src/guests/a/vsock.sock"},
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
		SourceDataDir: "/src/guests/a",
		TargetDataDir: "/dst/guests/b",
		VsockCID:      200,
		VsockSocket:   "/dst/guests/b/vsock.sock",
		SerialLogPath: "/dst/guests/b/logs/app.log",
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
	assert.Equal(t, "/dst/guests/b/overlay.raw", disk0["path"])

	serial := updated["serial"].(map[string]any)
	assert.Equal(t, "/dst/guests/b/logs/app.log", serial["file"])

	vsock := updated["vsock"].(map[string]any)
	assert.Equal(t, float64(100), vsock["cid"])
	assert.Equal(t, "/dst/guests/b/vsock.sock", vsock["socket"])

	netCfg := updated["net"].([]any)[0].(map[string]any)
	assert.Equal(t, "hype-new", netCfg["tap"])
	assert.Equal(t, "10.0.0.20", netCfg["ip"])
	assert.Equal(t, "02:00:00:00:00:02", netCfg["mac"])
	assert.Equal(t, "255.255.255.0", netCfg["mask"])

	metadata := updated["metadata"].(map[string]any)
	assert.Equal(t, "keep-/src/guests/a-as-substring", metadata["note"])
}
