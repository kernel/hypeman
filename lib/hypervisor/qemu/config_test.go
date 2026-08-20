package qemu

import (
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
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
	machine := standardMachineType()
	assert.Contains(t, args, "-machine")
	assert.Contains(t, args, string(machine)+",accel=kvm")

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
		if arg == "file=/path/to/data.ext4,format=raw,if=none,id=drive1,readonly=on,file.locking=off" {
			foundDrive1 = true
		}
	}
	assert.True(t, foundDrive0, "Expected writable drive0")
	assert.True(t, foundDrive1, "Expected readonly drive1")

	// Check virtio-blk devices
	assert.Contains(t, args, "virtio-blk-pci,drive=drive0")
	assert.Contains(t, args, "virtio-blk-pci,drive=drive1")
}

func TestBuildArgs_UEFISecureBootTPMAndQCOW2(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       2,
		MemoryBytes: 1024 * 1024 * 1024,
		BootMode:    hypervisor.BootModeUEFI,
		Firmware: &hypervisor.FirmwareConfig{
			CodePath:   "/firmware/OVMF_CODE.fd",
			VarsPath:   "/instance/OVMF_VARS.fd",
			SecureBoot: true,
		},
		TPM: &hypervisor.TPMConfig{
			SocketPath: "/instance/swtpm.sock",
			StateDir:   "/instance/tpm",
		},
		Disks: []hypervisor.DiskConfig{{Path: "/instance/windows.qcow2", Format: hypervisor.DiskFormatQCOW2}},
	}

	args := buildArgs(cfg, MachineTypeQ35)
	assert.Contains(t, args, "q35,accel=kvm,smm=on")
	assert.Contains(t, args, "if=pflash,format=raw,unit=0,file=/firmware/OVMF_CODE.fd,readonly=on")
	assert.Contains(t, args, "if=pflash,format=raw,unit=1,file=/instance/OVMF_VARS.fd")
	assert.Contains(t, args, "driver=cfi.pflash01,property=secure,value=on")
	assert.Contains(t, args, "file=/instance/windows.qcow2,format=qcow2,if=none,id=drive0")
	assert.Contains(t, args, "socket,id=chrtpm,path=/instance/swtpm.sock")
	assert.Contains(t, args, "emulator,id=tpm0,chardev=chrtpm")
	assert.Contains(t, args, "tpm-crb,tpmdev=tpm0")
	assert.NotContains(t, args, "-kernel")
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

	assert.Contains(t, args, "-chardev")
	assert.Contains(t, args, "file,id=serial0,path=/var/log/app.log,append=on")
	assert.Contains(t, args, "-serial")
	assert.Contains(t, args, "chardev:serial0")
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

func TestBuildArgs_MicroVM(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:         1,
		MemoryBytes:   512 * 1024 * 1024,
		Disks:         []hypervisor.DiskConfig{{Path: "/rootfs"}},
		Networks:      []hypervisor.NetworkConfig{{TAPDevice: "tap0", MAC: "02:00:00:ab:cd:ef"}},
		VsockCID:      123,
		SerialLogPath: "/var/log/app.log",
		GuestMemory:   hypervisor.GuestMemoryConfig{EnableBalloon: true},
	}

	args := buildArgs(cfg, MachineTypeMicroVM)
	assert.Contains(t, args, "microvm,accel=kvm")
	assert.Contains(t, args, "-no-user-config")
	assert.Contains(t, args, "virtio-blk-device,drive=drive0")
	assert.Contains(t, args, "virtio-net-device,netdev=net0,mac=02:00:00:ab:cd:ef")
	assert.Contains(t, args, "vhost-vsock-device,guest-cid=123")
	assert.Contains(t, args, "virtio-balloon-device")
	assert.Contains(t, args, "chardev:serial0")
	for _, arg := range args {
		assert.NotContains(t, arg, "-pci", "microvm cannot use PCI transport")
	}
}

func TestProfilesValidateFirmwareAndDiskFormats(t *testing.T) {
	uefi := hypervisor.VMConfig{
		BootMode: hypervisor.BootModeUEFI,
		Firmware: &hypervisor.FirmwareConfig{CodePath: "/code", VarsPath: "/vars"},
		Disks:    []hypervisor.DiskConfig{{Path: "/disk", Format: hypervisor.DiskFormatQCOW2}},
	}
	if standardMachineType() == MachineTypeQ35 {
		assert.NoError(t, StandardProfile{}.validateConfig(uefi))
	} else {
		assert.ErrorContains(t, StandardProfile{}.validateConfig(uefi), "supported only by qemu/q35 on amd64")
	}
	assert.ErrorContains(t, MicroVMProfile{}.validateConfig(uefi), "does not support uefi boot")
}

func TestBuildArgs_GuestMemoryBalloon(t *testing.T) {
	cfg := hypervisor.VMConfig{
		VCPUs:       1,
		MemoryBytes: 512 * 1024 * 1024,
		GuestMemory: hypervisor.GuestMemoryConfig{
			EnableBalloon:     true,
			DeflateOnOOM:      true,
			FreePageReporting: true,
			FreePageHinting:   true,
		},
	}

	args := BuildArgs(cfg)
	assert.Contains(t, args, "-device")
	assert.Contains(t, args, "virtio-balloon-pci,deflate-on-oom=on,free-page-reporting=on,free-page-hint=on")
}
