package qemu

import (
	"fmt"
	"runtime"
	"strconv"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// BuildArgs converts hypervisor.VMConfig to QEMU command-line arguments.
func BuildArgs(cfg hypervisor.VMConfig) []string {
	args := make([]string, 0, 64)

	// Machine type with KVM acceleration (arch-specific)
	args = append(args, "-machine", machineType())

	// CPU configuration
	args = append(args, "-cpu", "host")
	args = append(args, "-smp", strconv.Itoa(cfg.VCPUs))

	// Memory configuration
	memMB := cfg.MemoryBytes / (1024 * 1024)
	args = append(args, "-m", fmt.Sprintf("%dM", memMB))

	// Kernel and initrd
	if cfg.KernelPath != "" {
		args = append(args, "-kernel", cfg.KernelPath)
	}
	if cfg.InitrdPath != "" {
		args = append(args, "-initrd", cfg.InitrdPath)
	}
	if cfg.KernelArgs != "" {
		args = append(args, "-append", cfg.KernelArgs)
	}

	// Disk configuration
	for i, disk := range cfg.Disks {
		driveOpts := fmt.Sprintf("file=%s,format=raw,if=none,id=drive%d", disk.Path, i)
		if disk.Readonly {
			driveOpts += ",readonly=on"
		}
		if disk.IOBps > 0 {
			driveOpts += fmt.Sprintf(",throttling.bps-total=%d", disk.IOBps)
			if disk.IOBurstBps > 0 && disk.IOBurstBps > disk.IOBps {
				driveOpts += fmt.Sprintf(",throttling.bps-total-max=%d", disk.IOBurstBps)
			}
		}
		args = append(args, "-drive", driveOpts)
		args = append(args, "-device", fmt.Sprintf("virtio-blk-pci,drive=drive%d", i))
	}

	// Network configuration
	for i, net := range cfg.Networks {
		netdevOpts := fmt.Sprintf("tap,id=net%d,ifname=%s,script=no,downscript=no", i, net.TAPDevice)
		args = append(args, "-netdev", netdevOpts)

		deviceOpts := fmt.Sprintf("virtio-net-pci,netdev=net%d,mac=%s", i, net.MAC)
		args = append(args, "-device", deviceOpts)
	}

	// Vsock configuration
	if cfg.VsockCID > 0 {
		args = append(args, "-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", cfg.VsockCID))
	}

	// PCI device passthrough (GPU, etc.)
	for _, pciAddr := range cfg.PCIDevices {
		args = append(args, "-device", fmt.Sprintf("vfio-pci,host=%s", pciAddr))
	}

	// Serial console output to file
	if cfg.SerialLogPath != "" {
		args = append(args, "-serial", fmt.Sprintf("file:%s", cfg.SerialLogPath))
	} else {
		args = append(args, "-serial", "stdio")
	}

	// No graphics
	args = append(args, "-nographic")

	// Disable default devices we don't need
	args = append(args, "-nodefaults")

	return args
}

// machineType returns the QEMU machine type for the host architecture.
func machineType() string {
	switch runtime.GOARCH {
	case "arm64":
		return "virt,accel=kvm"
	default:
		// x86_64 and others use q35
		return "q35,accel=kvm"
	}
}
