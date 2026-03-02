package qemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// PrepareFork prepares QEMU fork state by rewriting snapshot VM config when a
// snapshot path is provided. For stopped forks (no snapshot), this is a no-op.
func (s *Starter) PrepareFork(ctx context.Context, req hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	_ = ctx
	if req.SnapshotConfigPath == "" {
		return hypervisor.ForkPrepareResult{}, nil
	}

	snapshotDir := filepath.Dir(req.SnapshotConfigPath)
	cfg, err := loadVMConfig(snapshotDir)
	if err != nil {
		// The generic path points to CH's config.json; for QEMU, require qemu-config.json.
		expectedPath := filepath.Join(snapshotDir, vmConfigFile)
		if _, statErr := os.Stat(expectedPath); statErr != nil {
			return hypervisor.ForkPrepareResult{}, fmt.Errorf("load qemu snapshot config %q: %w", expectedPath, err)
		}
		return hypervisor.ForkPrepareResult{}, fmt.Errorf("load qemu snapshot config: %w", err)
	}

	if req.SourceDataDir != "" && req.TargetDataDir != "" && req.SourceDataDir != req.TargetDataDir {
		cfg = rewriteQEMUConfigPaths(cfg, req.SourceDataDir, req.TargetDataDir)
	}

	if req.VsockCID > 0 {
		cfg.VsockCID = req.VsockCID
	}
	if req.VsockSocket != "" {
		cfg.VsockSocket = req.VsockSocket
	}
	if req.SerialLogPath != "" {
		cfg.SerialLogPath = req.SerialLogPath
	}

	if req.Network != nil {
		for i := range cfg.Networks {
			if req.Network.TAPDevice != "" {
				cfg.Networks[i].TAPDevice = req.Network.TAPDevice
			}
			if req.Network.MAC != "" {
				cfg.Networks[i].MAC = req.Network.MAC
			}
			if req.Network.IP != "" {
				cfg.Networks[i].IP = req.Network.IP
			}
			if req.Network.Netmask != "" {
				cfg.Networks[i].Netmask = req.Network.Netmask
			}
		}
	}

	if err := saveVMConfig(snapshotDir, cfg); err != nil {
		return hypervisor.ForkPrepareResult{}, fmt.Errorf("write qemu snapshot config: %w", err)
	}
	return hypervisor.ForkPrepareResult{
		VsockCIDUpdated: req.VsockCID > 0,
	}, nil
}

func rewriteQEMUConfigPaths(cfg hypervisor.VMConfig, sourceDir, targetDir string) hypervisor.VMConfig {
	replace := func(value string) string {
		if value == sourceDir || strings.HasPrefix(value, sourceDir+"/") {
			return targetDir + strings.TrimPrefix(value, sourceDir)
		}
		return value
	}

	for i := range cfg.Disks {
		cfg.Disks[i].Path = replace(cfg.Disks[i].Path)
	}

	cfg.SerialLogPath = replace(cfg.SerialLogPath)
	cfg.VsockSocket = replace(cfg.VsockSocket)
	cfg.KernelPath = replace(cfg.KernelPath)
	cfg.InitrdPath = replace(cfg.InitrdPath)

	return cfg
}
