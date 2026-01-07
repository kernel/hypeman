package cloudhypervisor

import (
	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/vmm"
)

// ToVMConfig converts hypervisor.VMConfig to Cloud Hypervisor's vmm.VmConfig.
func ToVMConfig(cfg hypervisor.VMConfig) vmm.VmConfig {
	// Payload configuration (kernel + initramfs)
	payload := vmm.PayloadConfig{
		Kernel:    ptr(cfg.KernelPath),
		Cmdline:   ptr(cfg.KernelArgs),
		Initramfs: ptr(cfg.InitrdPath),
	}

	// CPU configuration
	cpus := vmm.CpusConfig{
		BootVcpus: cfg.VCPUs,
		MaxVcpus:  cfg.VCPUs,
	}

	// Add topology if provided
	if cfg.Topology != nil {
		cpus.Topology = &vmm.CpuTopology{
			ThreadsPerCore: ptr(cfg.Topology.ThreadsPerCore),
			CoresPerDie:    ptr(cfg.Topology.CoresPerDie),
			DiesPerPackage: ptr(cfg.Topology.DiesPerPackage),
			Packages:       ptr(cfg.Topology.Packages),
		}
	}

	// Memory configuration
	memory := vmm.MemoryConfig{
		Size: cfg.MemoryBytes,
	}
	if cfg.HotplugBytes > 0 {
		memory.HotplugSize = &cfg.HotplugBytes
		memory.HotplugMethod = ptr("VirtioMem")
	}

	// Disk configuration
	disks := make([]vmm.DiskConfig, 0, len(cfg.Disks))
	for _, d := range cfg.Disks {
		disk := vmm.DiskConfig{
			Path: ptr(d.Path),
		}
		if d.Readonly {
			disk.Readonly = ptr(true)
		}
		if d.IOBps > 0 {
			// Token bucket: Size is refilled every RefillTime ms
			// Rate = Size / RefillTime * 1000 = Size bytes/sec (when RefillTime = 1000)
			burstBps := d.IOBurstBps
			if burstBps <= 0 {
				burstBps = d.IOBps
			}
			disk.RateLimiterConfig = &vmm.RateLimiterConfig{
				Bandwidth: &vmm.TokenBucket{
					Size:         d.IOBps,                 // sustained rate (bytes/sec with 1s refill)
					RefillTime:   1000,                    // refill over 1 second
					OneTimeBurst: ptr(burstBps - d.IOBps), // extra burst capacity
				},
			}
		}
		disks = append(disks, disk)
	}

	// Serial console configuration
	serial := vmm.ConsoleConfig{
		Mode: vmm.ConsoleConfigMode("File"),
		File: ptr(cfg.SerialLogPath),
	}

	// Console off (we use serial)
	console := vmm.ConsoleConfig{
		Mode: vmm.ConsoleConfigMode("Off"),
	}

	// Network configuration
	var nets *[]vmm.NetConfig
	if len(cfg.Networks) > 0 {
		netConfigs := make([]vmm.NetConfig, 0, len(cfg.Networks))
		for _, n := range cfg.Networks {
			netConfigs = append(netConfigs, vmm.NetConfig{
				Tap:  ptr(n.TAPDevice),
				Ip:   ptr(n.IP),
				Mac:  ptr(n.MAC),
				Mask: ptr(n.Netmask),
			})
		}
		nets = &netConfigs
	}

	// Vsock configuration
	var vsock *vmm.VsockConfig
	if cfg.VsockCID > 0 {
		vsock = &vmm.VsockConfig{
			Cid:    cfg.VsockCID,
			Socket: cfg.VsockSocket,
		}
	}

	// Device passthrough configuration
	var devices *[]vmm.DeviceConfig
	if len(cfg.PCIDevices) > 0 {
		deviceConfigs := make([]vmm.DeviceConfig, 0, len(cfg.PCIDevices))
		for _, path := range cfg.PCIDevices {
			deviceConfigs = append(deviceConfigs, vmm.DeviceConfig{
				Path: path,
			})
		}
		devices = &deviceConfigs
	}

	return vmm.VmConfig{
		Payload: payload,
		Cpus:    &cpus,
		Memory:  &memory,
		Disks:   &disks,
		Serial:  &serial,
		Console: &console,
		Net:     nets,
		Vsock:   vsock,
		Devices: devices,
	}
}
