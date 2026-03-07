//go:build darwin

package vz

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// PrepareFork prepares VZ snapshot state for forked instances.
// For stopped forks (no snapshot), this is a no-op.
func (s *Starter) PrepareFork(ctx context.Context, req hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	_ = ctx
	if req.SnapshotConfigPath == "" {
		return hypervisor.ForkPrepareResult{}, nil
	}

	if err := rewriteSnapshotManifestForFork(req.SnapshotConfigPath, req); err != nil {
		return hypervisor.ForkPrepareResult{}, err
	}
	return hypervisor.ForkPrepareResult{
		// VZ vsock dialing is socket-path based; CID rewrites are not required.
		VsockCIDUpdated: false,
	}, nil
}

func rewriteSnapshotManifestForFork(manifestPath string, req hypervisor.ForkPrepareRequest) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read snapshot manifest: %w", err)
	}

	var manifest shimconfig.SnapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("unmarshal snapshot manifest: %w", err)
	}

	if manifest.Hypervisor != "" && manifest.Hypervisor != string(hypervisor.TypeVZ) {
		return fmt.Errorf("snapshot hypervisor mismatch: expected vz, got %s", manifest.Hypervisor)
	}
	if manifest.Hypervisor == "" {
		manifest.Hypervisor = string(hypervisor.TypeVZ)
	}
	if manifest.MachineStateFile == "" {
		manifest.MachineStateFile = shimconfig.SnapshotMachineStateFile
	}

	if req.SourceDataDir != "" && req.TargetDataDir != "" && req.SourceDataDir != req.TargetDataDir {
		manifest.ShimConfig = rewriteShimConfigPaths(manifest.ShimConfig, req.SourceDataDir, req.TargetDataDir)
	}

	if req.VsockSocket != "" {
		manifest.ShimConfig.VsockSocket = req.VsockSocket
	}
	if req.SerialLogPath != "" {
		manifest.ShimConfig.SerialLogPath = req.SerialLogPath
	}

	// VZ machine-state restore requires device configuration compatibility.
	// Rewriting network identity fields in the serialized config can cause
	// restore to fail with "invalid argument", so keep NIC identity unchanged.

	// Runtime-only field; restore path is provided by the caller.
	manifest.ShimConfig.RestoreMachineStatePath = ""

	updated, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal snapshot manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, updated, 0644); err != nil {
		return fmt.Errorf("write snapshot manifest: %w", err)
	}
	return nil
}

func rewriteShimConfigPaths(cfg shimconfig.ShimConfig, sourceDir, targetDir string) shimconfig.ShimConfig {
	replace := func(value string) string {
		if value == sourceDir || strings.HasPrefix(value, sourceDir+"/") {
			return targetDir + strings.TrimPrefix(value, sourceDir)
		}
		return value
	}

	cfg.SerialLogPath = replace(cfg.SerialLogPath)
	cfg.KernelPath = replace(cfg.KernelPath)
	cfg.InitrdPath = replace(cfg.InitrdPath)
	cfg.ControlSocket = replace(cfg.ControlSocket)
	cfg.VsockSocket = replace(cfg.VsockSocket)
	cfg.LogPath = replace(cfg.LogPath)
	cfg.RestoreMachineStatePath = replace(cfg.RestoreMachineStatePath)

	for i := range cfg.Disks {
		cfg.Disks[i].Path = replace(cfg.Disks[i].Path)
	}

	return cfg
}
