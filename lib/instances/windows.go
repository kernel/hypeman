package instances

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/network"
)

func isWindowsPlatform(platform string) bool {
	return strings.EqualFold(strings.TrimSpace(platform), "windows/amd64")
}

func validateWindowsCreate(req CreateInstanceRequest, image *images.Image, caps hypervisor.Capabilities) error {
	if !images.IsWindowsImage(image) {
		return fmt.Errorf("%w: Windows instances require a Windows machine image", ErrInvalidRequest)
	}
	if !caps.SupportsUEFIBoot || !caps.SupportsTPM {
		return fmt.Errorf("%w: selected hypervisor must support UEFI boot and TPM devices", ErrInvalidRequest)
	}
	if image.Machine.VirtualSize <= 0 {
		return fmt.Errorf("%w: Windows image is missing its virtual disk size", ErrInvalidRequest)
	}
	if req.HotplugSize != 0 {
		return fmt.Errorf("%w: Windows instances do not yet support hotplug memory", ErrInvalidRequest)
	}
	if req.Size != 0 && req.Size < 4<<30 {
		return fmt.Errorf("%w: Windows 11 requires at least 4 GiB of memory", ErrInvalidRequest)
	}
	if req.Vcpus != 0 && req.Vcpus < 2 {
		return fmt.Errorf("%w: Windows 11 requires at least 2 vCPUs", ErrInvalidRequest)
	}
	if len(req.Volumes) != 0 || len(req.Devices) != 0 || req.GPU != nil {
		return fmt.Errorf("%w: Windows instances do not yet support volumes or device passthrough", ErrInvalidRequest)
	}
	if req.OverlaySize != 0 && req.OverlaySize != image.Machine.VirtualSize {
		return fmt.Errorf("%w: Windows instance disk size is fixed at %d bytes", ErrInvalidRequest, image.Machine.VirtualSize)
	}
	if len(req.Entrypoint) != 0 || len(req.Cmd) != 0 {
		return fmt.Errorf("%w: Windows machine images do not support entrypoint or command overrides", ErrInvalidRequest)
	}
	if len(req.Env) != 0 || req.HealthCheck != nil {
		return fmt.Errorf("%w: Windows instances do not yet support environment injection or health checks", ErrInvalidRequest)
	}
	if req.NetworkEgress != nil || len(req.Credentials) != 0 {
		return fmt.Errorf("%w: Windows instances do not yet support managed egress or credentials", ErrInvalidRequest)
	}
	return nil
}

func validateWindowsForkPolicy(stored *StoredMetadata) error {
	if stored != nil && isWindowsPlatform(stored.Platform) && stored.WindowsBitLockerPolicy != "disabled" {
		return fmt.Errorf("%w: Windows forks require a persona declared with %s=disabled", ErrNotSupported, images.MachineImageBitLockerLabel)
	}
	return nil
}

func (m *manager) ensureWindowsVsockCIDAvailable(ctx context.Context, stored *StoredMetadata) error {
	if stored == nil || !isWindowsPlatform(stored.Platform) {
		return nil
	}
	instances, err := m.listInstances(ctx)
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if instance.Id == stored.Id || instance.VsockCID != stored.VsockCID {
			continue
		}
		if instance.State == StateRunning || instance.State == StateInitializing {
			return fmt.Errorf("%w: Windows snapshot restore requires instance %s with the same captured vsock CID to be stopped", ErrInvalidState, instance.Id)
		}
	}
	return nil
}

func (m *manager) prepareWindowsForkIdentity(stored *StoredMetadata) error {
	if stored == nil || !isWindowsPlatform(stored.Platform) {
		return nil
	}
	if err := validateWindowsForkPolicy(stored); err != nil {
		return err
	}
	if err := os.RemoveAll(m.paths.InstanceTPMDir(stored.Id)); err != nil {
		return fmt.Errorf("clear forked Windows TPM state: %w", err)
	}
	if err := os.MkdirAll(m.paths.InstanceTPMDir(stored.Id), 0700); err != nil {
		return fmt.Errorf("create forked Windows TPM state: %w", err)
	}
	if err := os.Remove(m.paths.InstanceTPMSocket(stored.Id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove forked Windows TPM socket: %w", err)
	}
	stored.WindowsIdentityPending = true
	return nil
}

func rebindWindowsIdentity(ctx context.Context, stored *StoredMetadata) error {
	if stored == nil || !isWindowsPlatform(stored.Platform) || !stored.WindowsIdentityPending {
		return nil
	}
	dialer, err := hypervisor.NewVsockDialer(stored.HypervisorType, stored.VsockSocket, stored.VsockCID)
	if err != nil {
		return fmt.Errorf("create Windows identity dialer: %w", err)
	}
	rebindCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	machineID, err := guest.RebindInstanceIdentity(rebindCtx, dialer, stored.Id, 120*time.Second)
	if err != nil {
		return err
	}
	if machineID == "" {
		return fmt.Errorf("Windows guest agent returned an empty machine identity")
	}
	stored.WindowsIdentityPending = false
	return nil
}

func windowsFirmwareTemplates() (string, string, error) {
	code := os.Getenv("HYPEMAN_WINDOWS_OVMF_CODE")
	if code == "" {
		code = "/usr/share/OVMF/OVMF_CODE_4M.secboot.fd"
	}
	vars := os.Getenv("HYPEMAN_WINDOWS_OVMF_VARS")
	if vars == "" {
		vars = "/usr/share/OVMF/OVMF_VARS_4M.ms.fd"
	}
	for _, path := range []string{code, vars} {
		info, err := os.Stat(path)
		if err != nil {
			return "", "", fmt.Errorf("Windows firmware %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("Windows firmware %s is not a regular file", path)
		}
	}
	return code, vars, nil
}

func (m *manager) prepareWindowsInstance(inst *StoredMetadata, image *images.Image) error {
	source, err := images.GetMachineDiskPath(m.paths, image.Name, image.Digest, image.Machine)
	if err != nil {
		return err
	}
	if err := forkvm.CopyRegularFile(source, m.paths.InstanceWindowsDisk(inst.Id)); err != nil {
		return fmt.Errorf("clone Windows image: %w", err)
	}
	if err := os.Chmod(m.paths.InstanceWindowsDisk(inst.Id), 0600); err != nil {
		return fmt.Errorf("make Windows instance disk writable: %w", err)
	}

	code, vars, err := windowsFirmwareTemplates()
	if err != nil {
		return err
	}
	if err := forkvm.CopyRegularFile(code, m.paths.InstanceOVMFCode(inst.Id)); err != nil {
		return fmt.Errorf("copy OVMF code: %w", err)
	}
	if err := os.Chmod(m.paths.InstanceOVMFCode(inst.Id), 0444); err != nil {
		return fmt.Errorf("make OVMF code immutable: %w", err)
	}
	if err := forkvm.CopyRegularFile(vars, m.paths.InstanceOVMFVars(inst.Id)); err != nil {
		return fmt.Errorf("copy OVMF variables: %w", err)
	}
	if err := os.Chmod(m.paths.InstanceOVMFVars(inst.Id), 0600); err != nil {
		return fmt.Errorf("make OVMF variables writable: %w", err)
	}
	if err := os.MkdirAll(m.paths.InstanceTPMDir(inst.Id), 0700); err != nil {
		return fmt.Errorf("create TPM state directory: %w", err)
	}
	return nil
}

func (m *manager) buildWindowsHypervisorConfig(inst *Instance, image *images.Image, netConfig *network.NetworkConfig) (hypervisor.VMConfig, error) {
	if !images.IsWindowsImage(image) {
		return hypervisor.VMConfig{}, fmt.Errorf("image is not a Windows machine image")
	}
	if _, err := os.Stat(m.paths.InstanceWindowsDisk(inst.Id)); err != nil {
		return hypervisor.VMConfig{}, fmt.Errorf("stat Windows instance disk: %w", err)
	}

	var networks []hypervisor.NetworkConfig
	if netConfig != nil {
		networks = []hypervisor.NetworkConfig{{
			TAPDevice:   netConfig.TAPDevice,
			IP:          netConfig.IP,
			MAC:         netConfig.MAC,
			Netmask:     netConfig.Netmask,
			DownloadBps: inst.NetworkBandwidthDownload,
			UploadBps:   inst.NetworkBandwidthUpload,
		}}
	}

	ioBps := inst.DiskIOBps
	burstBps := ioBps * 4
	if ioBps <= 0 {
		burstBps = 0
	}
	return hypervisor.VMConfig{
		VCPUs:         inst.Vcpus,
		MemoryBytes:   inst.Size,
		Disks:         []hypervisor.DiskConfig{{Path: m.paths.InstanceWindowsDisk(inst.Id), Format: hypervisor.DiskFormatQCOW2, IOBps: ioBps, IOBurstBps: burstBps}},
		Networks:      networks,
		SerialLogPath: m.paths.InstanceAppLog(inst.Id),
		VsockCID:      inst.VsockCID,
		VsockSocket:   inst.VsockSocket,
		BootMode:      hypervisor.BootModeUEFI,
		Firmware: &hypervisor.FirmwareConfig{
			CodePath:   m.paths.InstanceOVMFCode(inst.Id),
			VarsPath:   m.paths.InstanceOVMFVars(inst.Id),
			SecureBoot: true,
		},
		TPM: &hypervisor.TPMConfig{
			SocketPath: m.paths.InstanceTPMSocket(inst.Id),
			StateDir:   m.paths.InstanceTPMDir(inst.Id),
		},
	}, nil
}
