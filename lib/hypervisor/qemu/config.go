package qemu

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernel/hypeman/lib/hypervisor"
)

// BuildArgs converts hypervisor.VMConfig to command-line arguments for standard
// QEMU. Backend starters use buildArgs with their private machine profile.
func BuildArgs(cfg hypervisor.VMConfig) []string {
	return buildArgs(cfg, standardMachineType())
}

func buildArgs(cfg hypervisor.VMConfig, machine MachineType) []string {
	args := make([]string, 0, 64)
	microvm := machine == MachineTypeMicroVM

	// Machine type with KVM acceleration (arch-specific when omitted).
	args = append(args, "-machine", string(machine)+",accel=kvm")
	if microvm {
		// Do not allow a host qemu.conf to add devices outside microvm's
		// documented eight virtio-mmio-device limit.
		args = append(args, "-no-user-config")
	}

	// CPU configuration
	args = append(args, "-cpu", "host")
	args = append(args, "-smp", strconv.Itoa(cfg.VCPUs))

	// Memory configuration
	memMB := cfg.MemoryBytes / (1024 * 1024)
	args = append(args, "-m", fmt.Sprintf("%dM", memMB))

	if cfg.GuestMemory.EnableBalloon {
		balloonOpts := []string{virtioDevice(microvm, "virtio-balloon")}
		// deflate-on-oom lets the guest reclaim ballooned pages under memory
		// pressure instead of invoking its OOM killer.
		if cfg.GuestMemory.DeflateOnOOM {
			balloonOpts = append(balloonOpts, "deflate-on-oom=on")
		}
		if cfg.GuestMemory.FreePageReporting {
			balloonOpts = append(balloonOpts, "free-page-reporting=on")
		}
		if cfg.GuestMemory.FreePageHinting {
			balloonOpts = append(balloonOpts, "free-page-hint=on")
		}
		args = append(args, "-device", strings.Join(balloonOpts, ","))
	}

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
			// Disable host-side file locking for shared readonly bases so multiple
			// VMs can boot concurrently from the same image without lock contention.
			driveOpts += ",readonly=on,file.locking=off"
		}
		if disk.IOBps > 0 {
			driveOpts += fmt.Sprintf(",throttling.bps-total=%d", disk.IOBps)
			if disk.IOBurstBps > 0 && disk.IOBurstBps > disk.IOBps {
				driveOpts += fmt.Sprintf(",throttling.bps-total-max=%d", disk.IOBurstBps)
			}
		}
		args = append(args, "-drive", driveOpts)
		args = append(args, "-device", fmt.Sprintf("%s,drive=drive%d", virtioDevice(microvm, "virtio-blk"), i))
	}

	// Network configuration
	for i, net := range cfg.Networks {
		netdevOpts := fmt.Sprintf("tap,id=net%d,ifname=%s,script=no,downscript=no", i, net.TAPDevice)
		args = append(args, "-netdev", netdevOpts)

		deviceOpts := fmt.Sprintf("%s,netdev=net%d,mac=%s", virtioDevice(microvm, "virtio-net"), i, net.MAC)
		args = append(args, "-device", deviceOpts)
	}

	// Vsock configuration
	if cfg.VsockCID > 0 {
		args = append(args, "-device", fmt.Sprintf("%s,guest-cid=%d", virtioDevice(microvm, "vhost-vsock"), cfg.VsockCID))
	}

	// Whole-device PCI passthrough (vGPU attaches via VGPUDevicePath below)
	for _, devicePath := range cfg.PCIDevices {
		var deviceArg string
		if strings.HasPrefix(devicePath, "/sys/bus/pci/devices/") {
			// Full sysfs path for regular PCI device - extract the PCI address
			// Using filepath.Base is more robust than manual string splitting
			pciAddr := filepath.Base(strings.TrimSuffix(devicePath, "/"))
			deviceArg = fmt.Sprintf("vfio-pci,host=%s", pciAddr)
		} else {
			// Raw PCI address (e.g., "0000:82:00.4")
			deviceArg = fmt.Sprintf("vfio-pci,host=%s", devicePath)
		}
		args = append(args, "-device", deviceArg)
	}

	if cfg.VGPUDevicePath != "" {
		args = append(args, "-device", fmt.Sprintf("vfio-pci,sysfsdev=%s", cfg.VGPUDevicePath))
	}

	// Serial console output to file. Use a chardev with append=on so QEMU
	// opens the file with O_APPEND. Without it, QEMU writes at its internal
	// fd offset; if the file is externally truncated (e.g. log rotation via
	// copytruncate) subsequent writes leave a sparse hole of NUL bytes from
	// byte 0 to the stale offset, which downstream log readers will pick up.
	if cfg.SerialLogPath != "" {
		args = append(args,
			"-chardev", fmt.Sprintf("file,id=serial0,path=%s,append=on", cfg.SerialLogPath),
			"-serial", "chardev:serial0",
		)
	} else {
		args = append(args, "-serial", "stdio")
	}

	// No graphics
	args = append(args, "-nographic")

	// Disable default devices we don't need
	args = append(args, "-nodefaults")

	return args
}

// virtioDevice returns the QEMU device model for a virtio transport. Standard
// q35/virt guests attach virtio devices over PCI (for example, virtio-blk-pci).
// The microvm board has no PCI bus, so it uses virtio-mmio models whose QEMU
// names end in -device (for example, virtio-blk-device).
func virtioDevice(microvm bool, name string) string {
	if microvm {
		return name + "-device"
	}
	return name + "-pci"
}
