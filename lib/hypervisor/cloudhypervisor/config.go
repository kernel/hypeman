package cloudhypervisor

import (
	"path/filepath"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/vmm"
)

// serialSocketPath returns the unix socket path that Cloud Hypervisor
// connects to for serial output. The socket lives next to the serial log
// file in the instance directory.
//
// We route serial through a hypeman-owned socket reader (see serial.go)
// rather than letting CH open the file directly, because CH's File-mode
// serial opens without O_APPEND. Combined with copytruncate-style log
// rotation that leaves CH's fd offset stale, the next write lands past
// EOF and creates a sparse hole of NUL bytes from byte 0 onward.
func serialSocketPath(logPath string) string {
	if logPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(logPath), "serial.sock")
}

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

	// Serial console configuration. We route serial through a unix socket
	// that hypeman listens on (see startSerialReader) instead of letting
	// CH write to the file directly — see serialSocketPath for rationale.
	serial := vmm.ConsoleConfig{
		Mode:   vmm.ConsoleConfigModeSocket,
		Socket: ptr(serialSocketPath(cfg.SerialLogPath)),
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

	var balloon *vmm.BalloonConfig
	if cfg.GuestMemory.EnableBalloon {
		balloon = &vmm.BalloonConfig{
			Size: 0,
		}
		if cfg.GuestMemory.DeflateOnOOM {
			balloon.DeflateOnOom = ptr(true)
		}
		if cfg.GuestMemory.FreePageReporting {
			balloon.FreePageReporting = ptr(true)
		}
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
		Balloon: balloon,
	}
}
