package qemu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func managedVGPUFixture(t *testing.T) (*paths.Paths, string, vgpuRuntimeManifest) {
	t.Helper()
	p := paths.New(t.TempDir())
	dir, err := vgpuRuntimeDir(p)
	if err != nil {
		t.Skip(err)
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "qemu-system-x86_64"), []byte("#!/bin/sh\necho 'QEMU emulator version 11.1.3'\n"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "firmware"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "firmware", "bios-256k.bin"), []byte("firmware"), 0o644))
	binaryHash, err := fileSHA256(filepath.Join(dir, "qemu-system-x86_64"))
	require.NoError(t, err)
	firmwareHash, err := firmwareSHA256(filepath.Join(dir, "firmware"))
	require.NoError(t, err)
	manifest := vgpuRuntimeManifest{Version: VGPUSnapshotVersion, SourceCommit: vgpuSourceCommit, BinarySHA256: binaryHash, FirmwareSHA256: firmwareHash}
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644))
	return p, dir, manifest
}

func TestManagedVGPURuntimeIsExplicitAndChecksumPinned(t *testing.T) {
	p, dir, _ := managedVGPUFixture(t)
	starter := NewStarter()
	version, err := starter.ResolveVersion(p, VGPUSnapshotVersion)
	require.NoError(t, err)
	require.Equal(t, VGPUSnapshotVersion, version)
	binary, err := starter.GetBinaryPath(p, version)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "qemu-system-x86_64"), binary)
	_, err = NewMicroVMStarter().ResolveVersion(p, version)
	require.ErrorContains(t, err, "standard QEMU")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "firmware", "bios-256k.bin"), []byte("different firmware"), 0o644))
	_, err = starter.ResolveVersion(p, version)
	require.ErrorContains(t, err, "checksum mismatch")
}

func TestManagedVGPURuntimeVerificationIsReusedUntilFilesChange(t *testing.T) {
	p, dir, manifest := managedVGPUFixture(t)
	binary := filepath.Join(dir, "qemu-system-x86_64")
	probes := filepath.Join(t.TempDir(), "probes")
	script := "#!/bin/sh\necho probe >> " + probes + "\necho 'QEMU emulator version 11.1.3'\n"
	require.NoError(t, os.WriteFile(binary, []byte(script), 0o755))
	var err error
	manifest.BinarySHA256, err = fileSHA256(binary)
	require.NoError(t, err)
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644))
	verifications := func() int {
		content, err := os.ReadFile(probes)
		require.NoError(t, err)
		return strings.Count(string(content), "probe")
	}

	for range 3 {
		_, err = loadVGPURuntime(p)
		require.NoError(t, err)
	}
	require.Equal(t, 1, verifications(), "an unchanged installation is verified once")

	// Rewriting a file with the same size and modification time still
	// forces the checksums to be recomputed.
	info, err := os.Stat(binary)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	tampered := []byte(strings.Repeat("x", int(info.Size())))
	require.NoError(t, os.WriteFile(binary, tampered, 0o755))
	require.NoError(t, os.Chtimes(binary, info.ModTime(), info.ModTime()))
	_, err = loadVGPURuntime(p)
	require.ErrorContains(t, err, "binary checksum mismatch")
}

func TestManagedVGPURuntimeRejectsUnexpectedUpstreamVersion(t *testing.T) {
	p, dir, manifest := managedVGPUFixture(t)
	binary := filepath.Join(dir, "qemu-system-x86_64")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\necho 'QEMU emulator version 8.2.2'\n"), 0o755))
	var err error
	manifest.BinarySHA256, err = fileSHA256(binary)
	require.NoError(t, err)
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644))
	_, err = NewStarter().ResolveVersion(p, VGPUSnapshotVersion)
	require.ErrorContains(t, err, "does not match")
}

func TestManagedVGPURuntimeMissingDoesNotUseSystemQEMU(t *testing.T) {
	_, err := NewStarter().ResolveVersion(paths.New(t.TempDir()), VGPUSnapshotVersion)
	require.Error(t, err)
}

func TestVGPUSnapshotContractRejectsCompatibilityChanges(t *testing.T) {
	_, _, runtime := managedVGPUFixture(t)
	base := vgpuSnapshotContract{Runtime: runtime, DriverVersion: "driver-a", VendorID: "0x10de", DeviceID: "device-a"}
	require.NoError(t, validateVGPUSnapshotContract(&base, &base))
	for _, change := range []func(*vgpuSnapshotContract){
		func(v *vgpuSnapshotContract) { v.Runtime.BinarySHA256 = strings.Repeat("0", 64) },
		func(v *vgpuSnapshotContract) { v.Runtime.FirmwareSHA256 = strings.Repeat("0", 64) },
		func(v *vgpuSnapshotContract) { v.DriverVersion = "driver-b" },
		func(v *vgpuSnapshotContract) { v.DeviceID = "device-b" },
		func(v *vgpuSnapshotContract) { v.ProfileType = "profile-b" },
	} {
		other := base
		change(&other)
		require.Error(t, validateVGPUSnapshotContract(&base, &other))
	}
	require.Error(t, validateVGPUSnapshotContract(nil, &base))
}

func TestVGPUForkClearsSourceDeviceAndBindsFreshIdentity(t *testing.T) {
	dir := t.TempDir()
	oldDevice := "/sys/bus/pci/devices/0000:01:00.1"
	newDevice := "/sys/bus/pci/devices/0000:01:00.2"
	saved := savedVMConfig{VMConfig: hypervisor.VMConfig{VGPUSnapshot: true, InstanceID: "source", VGPUDevicePath: oldDevice, VsockCID: 40}, MachineType: vgpuMachineType, QEMUVersion: VGPUSnapshotVersion}
	require.NoError(t, saveVMConfig(dir, saved))
	starter := NewStarter()
	_, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{SnapshotConfigPath: filepath.Join(dir, "config.json"), SourceDataDir: "/source", TargetDataDir: "/child", InstanceID: "child", VsockCID: 41})
	require.NoError(t, err)
	fork, err := loadVMConfig(dir)
	require.NoError(t, err)
	require.Empty(t, fork.VGPUDevicePath)
	require.Equal(t, "child", fork.InstanceID)
	require.EqualValues(t, 41, fork.VsockCID)
	_, err = starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{SnapshotConfigPath: filepath.Join(dir, "config.json"), VGPUDevicePath: newDevice})
	require.NoError(t, err)
	fork, err = loadVMConfig(dir)
	require.NoError(t, err)
	require.Equal(t, newDevice, fork.VGPUDevicePath)
	require.NotEqual(t, vgpuInstanceUUID("source"), vgpuInstanceUUID(fork.InstanceID))
}

func TestRemoveSnapshotMemoryLeavesForkLinksIntact(t *testing.T) {
	dir := t.TempDir()
	memory := filepath.Join(dir, "memory")
	forkLink := filepath.Join(dir, "fork-memory")
	require.NoError(t, os.WriteFile(memory, []byte("template memory"), 0o644))
	require.NoError(t, os.Link(memory, forkLink))

	require.NoError(t, removeSnapshotMemory(memory))
	require.NoFileExists(t, memory)
	content, err := os.ReadFile(forkLink)
	require.NoError(t, err)
	require.Equal(t, "template memory", string(content))
	require.NoError(t, removeSnapshotMemory(memory), "a missing file is not an error")
}

func TestVGPUMigrationNeverSelfAnnounces(t *testing.T) {
	// An announce round arriving while the guest agent takes the restored
	// interface down deadlocks the guest's network stack.
	var parameters map[string]any
	for _, command := range vgpuMigrationCommands() {
		if command.Execute == "migrate-set-parameters" {
			parameters = command.Args.(map[string]any)
		}
	}
	require.Equal(t, 0, parameters["announce-rounds"])
}

func TestVGPUArgumentsAreOptIn(t *testing.T) {
	cfg := hypervisor.VMConfig{VGPUSnapshot: true, InstanceID: "child", VGPUDevicePath: "/sys/bus/pci/devices/0000:01:00.1", MemoryBytes: 6 << 30, VCPUs: 4}
	args := strings.Join(buildArgs(cfg, vgpuMachineType), " ")
	require.Contains(t, args, "pc-q35-8.2,accel=kvm,smbios-entry-point-type=32,memory-backend=pc.ram")
	require.Contains(t, args, "-object memory-backend-ram,id=pc.ram,size=6144M,prealloc=on,prealloc-threads=16,x-use-canonical-path-for-ramblock-id=off")
	require.Contains(t, args, "vmgenid,guid=auto")
	require.Contains(t, args, "enable-migration=on")
	cfg.VGPUSnapshot = false
	args = strings.Join(buildArgs(cfg, MachineTypeQ35), " ")
	require.NotContains(t, args, "enable-migration")
	require.NotContains(t, args, "memory-backend")
	require.NotContains(t, args, "vmgenid")
}
