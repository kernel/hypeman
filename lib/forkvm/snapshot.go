package forkvm

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// SnapshotNetworkConfig contains network identity fields in CH snapshot config.json.
type SnapshotNetworkConfig struct {
	TAPDevice string
	IP        string
	MAC       string
	Netmask   string
}

// SnapshotRewriteOptions controls identity/path rewrites for CH snapshot config.json.
type SnapshotRewriteOptions struct {
	SourceDataDir string
	TargetDataDir string

	VsockCID    *int64
	VsockSocket string

	SerialLogPath string
	Network       *SnapshotNetworkConfig
}

// RewriteSnapshotConfig rewrites cloud-hypervisor snapshot config.json for a forked instance.
func RewriteSnapshotConfig(configPath string, opts SnapshotRewriteOptions) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read snapshot config: %w", err)
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("unmarshal snapshot config: %w", err)
	}

	if opts.SourceDataDir != "" && opts.TargetDataDir != "" && opts.SourceDataDir != opts.TargetDataDir {
		configAny := rewriteStringValues(config, func(s string) string {
			return strings.ReplaceAll(s, opts.SourceDataDir, opts.TargetDataDir)
		})
		config = configAny.(map[string]any)
	}

	if opts.VsockCID != nil || opts.VsockSocket != "" {
		updateVsockConfig(config, opts.VsockCID, opts.VsockSocket)
	}
	if opts.SerialLogPath != "" {
		updateSerialConfig(config, opts.SerialLogPath)
	}
	if opts.Network != nil {
		updateNetworkConfig(config, *opts.Network)
	}

	updated, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot config: %w", err)
	}

	if err := os.WriteFile(configPath, updated, 0644); err != nil {
		return fmt.Errorf("write snapshot config: %w", err)
	}

	return nil
}

func rewriteStringValues(value any, mapper func(string) string) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			out[k] = rewriteStringValues(child, mapper)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, child := range v {
			out = append(out, rewriteStringValues(child, mapper))
		}
		return out
	case string:
		return mapper(v)
	default:
		return value
	}
}

func updateVsockConfig(config map[string]any, cid *int64, socketPath string) {
	vsock, ok := config["vsock"].(map[string]any)
	if !ok || vsock == nil {
		return
	}
	if cid != nil {
		vsock["cid"] = *cid
	}
	if socketPath != "" {
		vsock["socket"] = socketPath
	}
}

func updateSerialConfig(config map[string]any, logPath string) {
	serial, ok := config["serial"].(map[string]any)
	if !ok || serial == nil {
		return
	}
	serial["file"] = logPath
}

func updateNetworkConfig(config map[string]any, netCfg SnapshotNetworkConfig) {
	nets, ok := config["net"].([]any)
	if !ok {
		return
	}
	for _, netAny := range nets {
		netMap, ok := netAny.(map[string]any)
		if !ok || netMap == nil {
			continue
		}
		if netCfg.TAPDevice != "" {
			netMap["tap"] = netCfg.TAPDevice
		}
		if netCfg.IP != "" {
			netMap["ip"] = netCfg.IP
		}
		if netCfg.MAC != "" {
			netMap["mac"] = netCfg.MAC
		}
		if netCfg.Netmask != "" {
			netMap["mask"] = netCfg.Netmask
		}
	}
}
