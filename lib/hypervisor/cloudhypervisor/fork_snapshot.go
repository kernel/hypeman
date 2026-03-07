package cloudhypervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// rewriteSnapshotConfigForFork rewrites Cloud Hypervisor snapshot config.json for a forked instance.
func rewriteSnapshotConfigForFork(configPath string, req hypervisor.ForkPrepareRequest) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read snapshot config: %w", err)
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("unmarshal snapshot config: %w", err)
	}

	if req.SourceDataDir != "" && req.TargetDataDir != "" && req.SourceDataDir != req.TargetDataDir {
		configAny := rewriteStringValues(config, func(s string) string {
			if s == req.SourceDataDir || strings.HasPrefix(s, req.SourceDataDir+"/") {
				return req.TargetDataDir + strings.TrimPrefix(s, req.SourceDataDir)
			}
			return s
		})
		config = configAny.(map[string]any)
	}

	updateVsockConfig(config, req.VsockCID, req.VsockSocket)
	if req.SerialLogPath != "" {
		updateSerialConfig(config, req.SerialLogPath)
	}
	if req.Network != nil {
		updateNetworkConfig(config, req.Network)
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

func updateVsockConfig(config map[string]any, cid int64, socketPath string) {
	_ = cid // Keep snapshot CID stable for CH restores; only rewrite socket path.
	vsock, ok := config["vsock"].(map[string]any)
	if !ok || vsock == nil {
		return
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

func updateNetworkConfig(config map[string]any, netCfg *hypervisor.ForkNetworkConfig) {
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
