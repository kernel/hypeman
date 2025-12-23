package qemu

import (
	"testing"

	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
)

func TestBuildArgs_Basic(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       2,
		MemoryBytes: 1024 * 1024 * 1024, // 1GB
		KernelPath:  "/path/to/vmlinux",
		InitrdPath:  "/path/to/initrd",
		KernelArgs:  "console=ttyS0",
	}

	args := BuildArgs(cfg)

	// Check machine type (arch-dependent)
	assert.Contains(t, args, "-machine")
	assert.Contains(t, args, machineType())

	// Check CPU
	assert.Contains(t, args, "-cpu")
	assert.Contains(t, args, "host")
	assert.Contains(t, args, "-smp")
	assert.Contains(t, args, "2")

	// Check memory
	assert.Contains(t, args, "-m")
	assert.Contains(t, args, "1024M")

	// Check kernel
	assert.Contains(t, args, "-kernel")
	assert.Contains(t, args, "/path/to/vmlinux")

	// Check initrd
	assert.Contains(t, args, "-initrd")
	assert.Contains(t, args, "/path/to/initrd")

	// Check kernel args
	assert.Contains(t, args, "-append")
	assert.Contains(t, args, "console=ttyS0")

	// Check nographic
	assert.Contains(t, args, "-nographic")
}

func TestBuildArgs_Disks(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       1,
		MemoryBytes: 512 * 1024 * 1024,
		Disks: []hypervisor.DiskConfig{
			{Path: "/path/to/rootfs.ext4", Readonly: false},
			{Path: "/path/to/data.ext4", Readonly: true},
		},
	}

	args := BuildArgs(cfg)

	// Check first disk (writable)
	assert.Contains(t, args, "-drive")
	foundDrive0 := false
	foundDrive1 := false
	for _, arg := range args {
		if arg == "file=/path/to/rootfs.ext4,format=raw,if=none,id=drive0" {
			foundDrive0 = true
		}
		if arg == "file=/path/to/data.ext4,format=raw,if=none,id=drive1,readonly=on" {
			foundDrive1 = true
		}
	}
	assert.True(t, foundDrive0, "Expected writable drive0")
	assert.True(t, foundDrive1, "Expected readonly drive1")

	// Check virtio-blk devices
	assert.Contains(t, args, "virtio-blk-pci,drive=drive0")
	assert.Contains(t, args, "virtio-blk-pci,drive=drive1")
}

func TestBuildArgs_Network(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       1,
		MemoryBytes: 512 * 1024 * 1024,
		Networks: []hypervisor.NetworkConfig{
			{
				TAPDevice: "tap0",
				MAC:       "02:00:00:ab:cd:ef",
				IP:        "192.168.1.10",
				Netmask:   "255.255.255.0",
			},
		},
	}

	args := BuildArgs(cfg)

	// Check netdev
	foundNetdev := false
	for _, arg := range args {
		if arg == "tap,id=net0,ifname=tap0,script=no,downscript=no" {
			foundNetdev = true
		}
	}
	assert.True(t, foundNetdev, "Expected tap netdev")

	// Check virtio-net device with MAC
	assert.Contains(t, args, "virtio-net-pci,netdev=net0,mac=02:00:00:ab:cd:ef")
}

func TestBuildArgs_Vsock(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       1,
		MemoryBytes: 512 * 1024 * 1024,
		VsockCID:    123,
	}

	args := BuildArgs(cfg)

	assert.Contains(t, args, "-device")
	assert.Contains(t, args, "vhost-vsock-pci,guest-cid=123")
}

func TestBuildArgs_PCIPassthrough(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       1,
		MemoryBytes: 512 * 1024 * 1024,
		PCIDevices:  []string{"0000:01:00.0", "0000:02:00.0"},
	}

	args := BuildArgs(cfg)

	assert.Contains(t, args, "vfio-pci,host=0000:01:00.0")
	assert.Contains(t, args, "vfio-pci,host=0000:02:00.0")
}

func TestBuildArgs_SerialLog(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:         1,
		MemoryBytes:   512 * 1024 * 1024,
		SerialLogPath: "/var/log/app.log",
	}

	args := BuildArgs(cfg)

	assert.Contains(t, args, "-serial")
	assert.Contains(t, args, "file:/var/log/app.log")
}

func TestBuildArgs_NoSerialLog(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       1,
		MemoryBytes: 512 * 1024 * 1024,
	}

	args := BuildArgs(cfg)

	assert.Contains(t, args, "-serial")
	assert.Contains(t, args, "stdio")
}
