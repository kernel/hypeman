//go:build darwin

package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// createVM creates and configures a vz.VirtualMachine from ShimConfig.
func createVM(config shimconfig.ShimConfig) (*vz.VirtualMachine, *vz.VirtualMachineConfiguration, error) {
	// Prepare kernel command line (vz uses hvc0 for serial console)
	kernelArgs := config.KernelArgs
	if kernelArgs == "" {
		kernelArgs = "console=hvc0 root=/dev/vda"
	} else {
		kernelArgs = strings.ReplaceAll(kernelArgs, "console=ttyS0", "console=hvc0")
	}

	bootLoader, err := vz.NewLinuxBootLoader(
		config.KernelPath,
		vz.WithCommandLine(kernelArgs),
		vz.WithInitrd(config.InitrdPath),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create boot loader: %w", err)
	}

	vcpus := computeCPUCount(config.VCPUs)
	memoryBytes := computeMemorySize(uint64(config.MemoryBytes))

	slog.Debug("VM config", "vcpus", vcpus, "memory_bytes", memoryBytes, "kernel", config.KernelPath, "initrd", config.InitrdPath)

	vmConfig, err := vz.NewVirtualMachineConfiguration(bootLoader, vcpus, memoryBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("create vm configuration: %w", err)
	}

	if err := configureSerialConsole(vmConfig, config.SerialLogPath); err != nil {
		return nil, nil, fmt.Errorf("configure serial: %w", err)
	}

	if err := configureNetwork(vmConfig, config.Networks); err != nil {
		return nil, nil, fmt.Errorf("configure network: %w", err)
	}

	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, nil, fmt.Errorf("create entropy device: %w", err)
	}
	vmConfig.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	if err := configureStorage(vmConfig, config.Disks); err != nil {
		return nil, nil, fmt.Errorf("configure storage: %w", err)
	}

	vsockConfig, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, nil, fmt.Errorf("create vsock device: %w", err)
	}
	vmConfig.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsockConfig})

	if balloonConfig, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration(); err == nil {
		vmConfig.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloonConfig})
	}

	if validated, err := vmConfig.Validate(); !validated || err != nil {
		return nil, nil, fmt.Errorf("invalid vm configuration: %w", err)
	}

	vm, err := vz.NewVirtualMachine(vmConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("create virtual machine: %w", err)
	}

	return vm, vmConfig, nil
}

func configureSerialConsole(vmConfig *vz.VirtualMachineConfiguration, logPath string) error {
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

func configureNetwork(vmConfig *vz.VirtualMachineConfiguration, networks []shimconfig.NetworkConfig) error {
	if len(networks) == 0 {
		// No networks configured (NetworkEnabled=false) — do not attach any NIC.
		return nil
	}

	var devices []*vz.VirtioNetworkDeviceConfiguration
	for _, netConfig := range networks {
		dev, err := createNATNetworkDevice(netConfig.MAC)
		if err != nil {
			return err
		}
		devices = append(devices, dev)
	}
	vmConfig.SetNetworkDevicesVirtualMachineConfiguration(devices)
	return nil
}

func createNATNetworkDevice(macAddr string) (*vz.VirtioNetworkDeviceConfiguration, error) {
	natAttachment, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("create NAT attachment: %w", err)
	}

	networkConfig, err := vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
	if err != nil {
		return nil, fmt.Errorf("create network config: %w", err)
	}

	mac, err := assignMACAddress(macAddr)
	if err != nil {
		return nil, err
	}
	networkConfig.SetMACAddress(mac)

	return networkConfig, nil
}

func assignMACAddress(macAddr string) (*vz.MACAddress, error) {
	if macAddr == "" {
		mac, err := vz.NewRandomLocallyAdministeredMACAddress()
		if err != nil {
			return nil, fmt.Errorf("generate MAC address: %w", err)
		}
		slog.Info("generated random MAC address", "mac", mac.String())
		return mac, nil
	}

	hwAddr, err := net.ParseMAC(macAddr)
	if err != nil {
		slog.Warn("failed to parse MAC address, generating random", "mac", macAddr, "error", err)
		mac, err := vz.NewRandomLocallyAdministeredMACAddress()
		if err != nil {
			return nil, fmt.Errorf("generate MAC address: %w", err)
		}
		return mac, nil
	}

	mac, err := vz.NewMACAddress(hwAddr)
	if err != nil {
		slog.Warn("failed to create MAC from parsed address, generating random", "mac", macAddr, "error", err)
		mac, err := vz.NewRandomLocallyAdministeredMACAddress()
		if err != nil {
			return nil, fmt.Errorf("generate MAC address: %w", err)
		}
		return mac, nil
	}

	slog.Info("using specified MAC address", "mac", macAddr)
	return mac, nil
}

func configureStorage(vmConfig *vz.VirtualMachineConfiguration, disks []shimconfig.DiskConfig) error {
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
		requested = 2 * 1024 * 1024 * 1024 // 2GB safety default (caller normally provides this)
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
