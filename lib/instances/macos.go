package instances

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/network"
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
	boot, err := images.GetBootStorage(m.paths, img.Name, img.Digest)
	if err != nil {
		return err
	}
	return cloneMacOSStorage(boot.Disk, boot.Aux, m.paths.InstanceOverlay(stored.Id), filepath.Join(stored.DataDir, "mac-aux.img"))
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
		if errors.Is(err, ErrNotFound) {
			// A create in progress or a leftover directory has no instance to admit against.
			continue
		}
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

// vmnet stores octets without leading zeroes. A lease is observed addressing,
// not a static-IP assignment or a guest-readiness signal.
func macOSGuestIP(mac string) string {
	wanted, err := net.ParseMAC(mac)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return ""
	}
	return macOSLeaseIP(string(data), wanted)
}
func macOSLeaseIP(data string, wanted net.HardwareAddr) string {
	for _, block := range strings.Split(data, "}") {
		var addr, ip string
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			switch k {
			case "hw_address":
				_, addr, _ = strings.Cut(v, ",")
			case "ip_address":
				ip = v
			}
		}
		var padded []string
		for _, octet := range strings.Split(addr, ":") {
			if len(octet) == 1 {
				octet = "0" + octet
			}
			padded = append(padded, octet)
		}
		raw, err := hex.DecodeString(strings.Join(padded, ""))
		if err == nil && string(raw) == string(wanted) && net.ParseIP(ip) != nil {
			return ip
		}
	}
	return ""
}

// macOSNetworkConfig pins a networked macOS guest to its preserved MAC and returns
// its network config. It returns nil for Linux guests and networkless macOS guests.
func macOSNetworkConfig(stored *StoredMetadata) *network.NetworkConfig {
	if stored.MacOS == nil || !stored.NetworkEnabled {
		return nil
	}
	stored.MAC = stored.MacOS.MAC
	return &network.NetworkConfig{MAC: stored.MacOS.MAC}
}
