//go:build darwin

// Package vz implements the hypervisor.Hypervisor interface for
// Apple's Virtualization.framework on macOS.
package vz

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"

	"github.com/Code-Hex/vz/v3"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
)

func init() {
	hypervisor.RegisterSocketName(hypervisor.TypeVZ, "vz.sock")
	hypervisor.RegisterVsockDialerFactory(hypervisor.TypeVZ, NewVsockDialer)
}

// Starter implements hypervisor.VMStarter for Virtualization.framework.
type Starter struct{}

// NewStarter creates a new vz starter.
func NewStarter() *Starter {
	return &Starter{}
}

// Verify Starter implements the interface
var _ hypervisor.VMStarter = (*Starter)(nil)

// SocketName returns the socket filename for vz.
func (s *Starter) SocketName() string {
	return "vz.sock"
}

// GetBinaryPath returns empty - vz uses system Virtualization.framework.
func (s *Starter) GetBinaryPath(p *paths.Paths, version string) (string, error) {
	return "", nil
}

// GetVersion returns the macOS version as the "hypervisor version".
func (s *Starter) GetVersion(p *paths.Paths) (string, error) {
	// Return a version indicating vz availability
	return "vz-macos", nil
}

// StartVM creates and starts a VM. Returns PID 0 since vz runs in-process.
func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)

	// vz uses hvc0 for serial console
	kernelCommandLine := config.KernelArgs
	if kernelCommandLine == "" {
		kernelCommandLine = "console=hvc0 root=/dev/vda"
	} else {
		kernelCommandLine = strings.ReplaceAll(kernelCommandLine, "console=ttyS0", "console=hvc0")
	}

	bootLoader, err := vz.NewLinuxBootLoader(
		config.KernelPath,
		vz.WithCommandLine(kernelCommandLine),
		vz.WithInitrd(config.InitrdPath),
	)
	if err != nil {
		return 0, nil, fmt.Errorf("create boot loader: %w", err)
	}

	vcpus := computeCPUCount(config.VCPUs)
	memoryBytes := computeMemorySize(uint64(config.MemoryBytes))

	log.DebugContext(ctx, "vz VM config",
		"vcpus", vcpus,
		"memory_bytes", memoryBytes,
		"kernel", config.KernelPath,
		"initrd", config.InitrdPath)

	vmConfig, err := vz.NewVirtualMachineConfiguration(bootLoader, vcpus, memoryBytes)
	if err != nil {
		return 0, nil, fmt.Errorf("create vm configuration: %w", err)
	}

	if err := s.configureSerialConsole(vmConfig, config.SerialLogPath); err != nil {
		return 0, nil, fmt.Errorf("configure serial: %w", err)
	}

	if err := s.configureNetwork(vmConfig, config.Networks); err != nil {
		return 0, nil, fmt.Errorf("configure network: %w", err)
	}

	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return 0, nil, fmt.Errorf("create entropy device: %w", err)
	}
	vmConfig.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	if err := s.configureStorage(vmConfig, config.Disks); err != nil {
		return 0, nil, fmt.Errorf("configure storage: %w", err)
	}

	vsockConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return 0, nil, fmt.Errorf("create vsock device: %w", err)
	}
	vmConfig.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})

	if balloonConfig, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration(); err == nil {
		vmConfig.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloonConfig})
	}

	if validated, err := vmConfig.Validate(); !validated || err != nil {
		return 0, nil, fmt.Errorf("invalid vm configuration: %w", err)
	}

	vm, err := vz.NewVirtualMachine(vmConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("create virtual machine: %w", err)
	}

	if err := vm.Start(); err != nil {
		return 0, nil, fmt.Errorf("start vm: %w", err)
	}

	log.InfoContext(ctx, "vz VM started", "vcpus", vcpus, "memory_mb", memoryBytes/1024/1024)

	return 0, &Hypervisor{vm: vm, vmConfig: vmConfig}, nil
}

// RestoreVM restores a VM from a snapshot (macOS 14+ ARM64 only).
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	return 0, nil, fmt.Errorf("vz RestoreVM requires VMConfig; use RestoreVMWithConfig instead")
}

// RestoreVMWithConfig restores a VM from a snapshot (macOS 14+ ARM64 only).
func (s *Starter) RestoreVMWithConfig(ctx context.Context, p *paths.Paths, config hypervisor.VMConfig, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)

	kernelCommandLine := config.KernelArgs
	if kernelCommandLine == "" {
		kernelCommandLine = "console=hvc0 root=/dev/vda"
	}

	bootLoader, err := vz.NewLinuxBootLoader(
		config.KernelPath,
		vz.WithCommandLine(kernelCommandLine),
		vz.WithInitrd(config.InitrdPath),
	)
	if err != nil {
		return 0, nil, fmt.Errorf("create boot loader: %w", err)
	}

	vcpus := computeCPUCount(config.VCPUs)
	memoryBytes := computeMemorySize(uint64(config.MemoryBytes))

	vmConfig, err := vz.NewVirtualMachineConfiguration(bootLoader, vcpus, memoryBytes)
	if err != nil {
		return 0, nil, fmt.Errorf("create vm configuration: %w", err)
	}

	if err := s.configureSerialConsole(vmConfig, config.SerialLogPath); err != nil {
		return 0, nil, fmt.Errorf("configure serial: %w", err)
	}
	if err := s.configureNetwork(vmConfig, config.Networks); err != nil {
		return 0, nil, fmt.Errorf("configure network: %w", err)
	}

	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return 0, nil, fmt.Errorf("create entropy device: %w", err)
	}
	vmConfig.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	if err := s.configureStorage(vmConfig, config.Disks); err != nil {
		return 0, nil, fmt.Errorf("configure storage: %w", err)
	}

	vsockConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return 0, nil, fmt.Errorf("create vsock device: %w", err)
	}
	vmConfig.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})

	if validated, err := vmConfig.Validate(); !validated || err != nil {
		return 0, nil, fmt.Errorf("invalid vm configuration: %w", err)
	}

	if valid, err := vmConfig.ValidateSaveRestoreSupport(); err != nil || !valid {
		return 0, nil, fmt.Errorf("snapshot restore not supported (requires macOS 14+ ARM64)")
	}

	vm, err := vz.NewVirtualMachine(vmConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("create virtual machine: %w", err)
	}

	log.InfoContext(ctx, "restoring vz VM from snapshot", "path", snapshotPath)
	if err := vm.RestoreMachineStateFromURL(snapshotPath); err != nil {
		return 0, nil, fmt.Errorf("restore from snapshot: %w", err)
	}

	log.InfoContext(ctx, "vz VM restored", "vcpus", vcpus, "memory_mb", memoryBytes/1024/1024)

	return 0, &Hypervisor{vm: vm, vmConfig: vmConfig}, nil
}

func (s *Starter) configureSerialConsole(vmConfig *vz.VirtualMachineConfiguration, logPath string) error {
	var serialAttachment *vz.FileHandleSerialPortAttachment

	nullRead, err := os.OpenFile("/dev/null", os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null for reading: %w", err)
	}

	if logPath != "" {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			nullRead.Close()
			return fmt.Errorf("open serial log file: %w", err)
		}
		serialAttachment, err = vz.NewFileHandleSerialPortAttachment(nullRead, file)
		if err != nil {
			nullRead.Close()
			file.Close()
			return fmt.Errorf("create serial attachment: %w", err)
		}
	} else {
		nullWrite, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
		if err != nil {
			nullRead.Close()
			return fmt.Errorf("open /dev/null for writing: %w", err)
		}
		serialAttachment, err = vz.NewFileHandleSerialPortAttachment(nullRead, nullWrite)
		if err != nil {
			nullRead.Close()
			nullWrite.Close()
			return fmt.Errorf("create serial attachment: %w", err)
		}
	}

	consoleConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialAttachment)
	if err != nil {
		return fmt.Errorf("create console config: %w", err)
	}
	vmConfig.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{
		consoleConfig,
	})

	return nil
}

func (s *Starter) configureNetwork(vmConfig *vz.VirtualMachineConfiguration, networks []hypervisor.NetworkConfig) error {
	if len(networks) == 0 {
		return s.addNATNetwork(vmConfig, "")
	}
	for _, netConfig := range networks {
		if err := s.addNATNetwork(vmConfig, netConfig.MAC); err != nil {
			return err
		}
	}
	return nil
}

func (s *Starter) addNATNetwork(vmConfig *vz.VirtualMachineConfiguration, macAddr string) error {
	natAttachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return fmt.Errorf("create NAT attachment: %w", err)
	}

	networkConfig, err := vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
	if err != nil {
		return fmt.Errorf("create network config: %w", err)
	}

	var mac *vz.MACAddress
	if macAddr != "" {
		hwAddr, parseErr := net.ParseMAC(macAddr)
		if parseErr == nil {
			mac, err = vz.NewMACAddress(hwAddr)
		}
		if parseErr != nil || err != nil {
			mac, err = vz.NewRandomLocallyAdministeredMACAddress()
			if err != nil {
				return fmt.Errorf("generate MAC address: %w", err)
			}
		}
	} else {
		mac, err = vz.NewRandomLocallyAdministeredMACAddress()
		if err != nil {
			return fmt.Errorf("generate MAC address: %w", err)
		}
	}
	networkConfig.SetMACAddress(mac)

	vmConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{
		networkConfig,
	})

	return nil
}

func (s *Starter) configureStorage(vmConfig *vz.VirtualMachineConfiguration, disks []hypervisor.DiskConfig) error {
	var storageDevices []vz.StorageDeviceConfiguration

	for _, disk := range disks {
		if _, err := os.Stat(disk.Path); os.IsNotExist(err) {
			return fmt.Errorf("disk image not found: %s", disk.Path)
		}

		if strings.HasSuffix(disk.Path, ".qcow2") {
			return fmt.Errorf("qcow2 not supported by vz, use raw format: %s", disk.Path)
		}

		attachment, err := vz.NewDiskImageStorageDeviceAttachment(disk.Path, disk.Readonly)
		if err != nil {
			return fmt.Errorf("create disk attachment for %s: %w", disk.Path, err)
		}

		blockConfig, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
		if err != nil {
			return fmt.Errorf("create block device config: %w", err)
		}

		storageDevices = append(storageDevices, blockConfig)
	}

	if len(storageDevices) > 0 {
		vmConfig.SetStorageDevicesVirtualMachineConfiguration(storageDevices)
	}

	return nil
}

func computeCPUCount(requested int) uint {
	virtualCPUCount := uint(requested)
	if virtualCPUCount == 0 {
		virtualCPUCount = uint(runtime.NumCPU() - 1)
		if virtualCPUCount < 1 {
			virtualCPUCount = 1
		}
	}

	maxAllowed := vz.VirtualMachineConfigurationMaximumAllowedCPUCount()
	minAllowed := vz.VirtualMachineConfigurationMinimumAllowedCPUCount()

	if virtualCPUCount > maxAllowed {
		virtualCPUCount = maxAllowed
	}
	if virtualCPUCount < minAllowed {
		virtualCPUCount = minAllowed
	}

	return virtualCPUCount
}

func computeMemorySize(requested uint64) uint64 {
	if requested == 0 {
		requested = 2 * 1024 * 1024 * 1024 // 2GB default
	}

	maxAllowed := vz.VirtualMachineConfigurationMaximumAllowedMemorySize()
	minAllowed := vz.VirtualMachineConfigurationMinimumAllowedMemorySize()

	if requested > maxAllowed {
		requested = maxAllowed
	}
	if requested < minAllowed {
		requested = minAllowed
	}

	return requested
}
