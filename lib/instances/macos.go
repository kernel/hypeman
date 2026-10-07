package instances

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
)

func prepareMacOSRequest(req *CreateInstanceRequest, img *images.Image, hv hypervisor.Type) error {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || hv != hypervisor.TypeVZ {
		return fmt.Errorf("%w: macOS guests require vz on Apple silicon", ErrInvalidRequest)
	}
	if img.Platform != "darwin/arm64" || img.SizeBytes == nil || *img.SizeBytes <= 0 {
		return fmt.Errorf("%w: incomplete macOS image", ErrImageNotReady)
	}
	if req.HotplugSize != 0 || req.OverlaySize != 0 || len(req.Volumes) != 0 || len(req.Devices) != 0 || req.GPU != nil || len(req.Env) != 0 || len(req.Entrypoint) != 0 || len(req.Cmd) != 0 || req.NetworkEgress != nil || len(req.Credentials) != 0 || req.AutoStandby != nil || req.HealthCheck != nil || req.RestartPolicy != nil || req.SnapshotPolicy != nil || req.DiskIOBps != 0 || req.NetworkBandwidthDownload != 0 || req.NetworkBandwidthUpload != 0 {
		return fmt.Errorf("%w: experimental macOS supports local disk clone, CPU/RAM, tags, expiration and NAT only; Linux commands/env/volumes, overlays, agents, policies and I/O shaping are unsupported", ErrInvalidRequest)
	}
	if req.Size == 0 {
		req.Size = int64(img.MacOS.Memory)
	}
	if req.Vcpus == 0 {
		req.Vcpus = int(img.MacOS.CPUs)
	}
	if req.Size < 4<<30 || req.Vcpus < 2 {
		return fmt.Errorf("%w: macOS requires at least 4 GiB and 2 vCPUs", ErrInvalidRequest)
	}
	req.OverlaySize = *img.SizeBytes // Reserve the writable boot disk, not a Linux overlay.
	req.SkipGuestAgent = true
	req.SkipKernelHeaders = true
	return nil
}
func (m *manager) prepareBootStorage(stored *StoredMetadata, img *images.Image) error {
	if stored.MacOS == nil {
		return m.createOverlayDisk(stored.Id, stored.OverlaySize)
	}
	root, err := images.GetDiskPath(m.paths, img.Name, img.Digest)
	if err != nil {
		return err
	}
	return cloneMacOSStorage(root, m.paths.InstanceOverlay(stored.Id), filepath.Join(stored.DataDir, "mac-aux.img"))
}
func (m *manager) macOSVMConfig(inst *Instance) hypervisor.VMConfig {
	c := hypervisor.VMConfig{BootMode: hypervisor.BootModeMacOS, VCPUs: inst.Vcpus, MemoryBytes: inst.Size, VsockCID: inst.VsockCID, VsockSocket: inst.VsockSocket,
		MacOS: &hypervisor.MacOSPlatform{HardwareModelData: inst.MacOS.HardwareModel, MachineIdentifierData: inst.MacOS.MachineIdentifier, AuxStoragePath: filepath.Join(inst.DataDir, "mac-aux.img")},
		Disks: []hypervisor.DiskConfig{{Path: m.paths.InstanceOverlay(inst.Id)}}}
	if inst.NetworkEnabled {
		c.Networks = []hypervisor.NetworkConfig{{MAC: inst.MAC}}
	}
	return c
}
func initialBootPhase(stored *StoredMetadata) phasetracking.Phase {
	if stored.MacOS != nil {
		return phasetracking.PhaseRunning
	}
	return phasetracking.PhaseInitializing
}
func (m *manager) checkMacOSIdentityAvailable(ctx context.Context, stored *StoredMetadata) error {
	entries, err := os.ReadDir(m.paths.GuestsDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == stored.Id {
			continue
		}
		meta, err := m.loadMetadata(entry.Name())
		if err != nil {
			return fmt.Errorf("check Mac identity admission: %w", err)
		}
		if meta.MacOS == nil || !bytes.Equal(meta.MacOS.MachineIdentifier, stored.MacOS.MachineIdentifier) {
			continue
		}
		state := m.deriveStateWithoutHydration(ctx, &meta.StoredMetadata).State
		if state.RequiresVMM() || state == StateUnknown {
			return fmt.Errorf("%w: macOS template identity is already in use by instance %s; stop it first", ErrInsufficientResources, meta.Id)
		}
	}
	return nil
}
func (m *manager) rejectMacOSOperation(id, operation string) error {
	meta, err := m.loadMetadata(id)
	if err != nil {
		return err
	}
	if meta.MacOS != nil {
		return fmt.Errorf("%w: %s is not implemented for experimental macOS instances", ErrInvalidRequest, operation)
	}
	return nil
}
