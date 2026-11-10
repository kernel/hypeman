package qemu

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/digitalocean/go-qemu/qmp"
	"github.com/google/uuid"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
)

// VGPUSnapshotVersion selects the separately installed, snapshot-capable runtime.
const VGPUSnapshotVersion = "11.1.3-vgpu.1"

const (
	vgpuUpstreamVersion             = "11.1.3"
	vgpuSourceCommit                = "005c61c0ef6f6ae615283440b4bb7be51b2c6ddd"
	vgpuMachineType     MachineType = "pc-q35-8.2"

	// vgpuRAMBackendID is q35's default RAM block id, so the explicit backend
	// keeps the name an implicit one would give guest RAM in the stream.
	vgpuRAMBackendID = "pc.ram"
	// vgpuPreallocThreads matches QEMU's own upper bound for populating RAM.
	vgpuPreallocThreads = 16

	vgpuRuntimeBinaryName   = "qemu-system-x86_64"
	vgpuRuntimeFirmwareDir  = "firmware"
	vgpuRuntimeManifestName = "manifest.json"
)

type vgpuRuntimeManifest struct {
	Version        string `json:"version"`
	SourceCommit   string `json:"source_commit"`
	BinarySHA256   string `json:"binary_sha256"`
	FirmwareSHA256 string `json:"firmware_sha256"`
}

// vgpuRuntime is a managed QEMU installation whose contents matched its manifest.
type vgpuRuntime struct {
	binary   string
	firmware string
	manifest vgpuRuntimeManifest
}

// vgpuSnapshotContract is the host state a vGPU snapshot can only be loaded on.
type vgpuSnapshotContract struct {
	Runtime       vgpuRuntimeManifest
	DriverVersion string
	VendorID      string
	DeviceID      string
	ProfileType   string
}

// vgpuRuntimeCache holds the last verified runtime per installation
// directory. Hashing the binary and firmware costs tens of milliseconds and
// every start and restore resolves the runtime several times, so a
// verification is reused while each file's size and modification time are
// unchanged. Installations are immutable once placed; a replaced file is
// hashed again.
var vgpuRuntimeCache struct {
	sync.Mutex
	entries map[string]vgpuRuntimeCacheEntry
}

type vgpuRuntimeCacheEntry struct {
	fingerprint string
	runtime     vgpuRuntime
}

func vgpuRuntimeDir(p *paths.Paths) (string, error) {
	if p == nil || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("vGPU snapshots require a managed Linux amd64 runtime")
	}
	return p.QEMURuntimeDir(VGPUSnapshotVersion, "x86_64"), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// firmwareSHA256 hashes every firmware file with its name and length, in
// name order, so a renamed, added or removed blob changes the digest.
func firmwareSHA256(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("empty runtime firmware directory")
	}
	h := sha256.New()
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("runtime firmware must contain regular files")
		}
		h.Write([]byte(entry.Name() + "\x00"))
		if err := binary.Write(h, binary.LittleEndian, uint64(info.Size())); err != nil {
			return "", err
		}
		f, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// vgpuRuntimeFingerprint summarizes the installed files without reading them.
func vgpuRuntimeFingerprint(dir string) (string, error) {
	var b strings.Builder
	add := func(path string) error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s\x00%d\x00%d\x00%o\n", path, info.Size(), info.ModTime().UnixNano(), info.Mode())
		return nil
	}
	for _, name := range []string{vgpuRuntimeManifestName, vgpuRuntimeBinaryName} {
		if err := add(filepath.Join(dir, name)); err != nil {
			return "", err
		}
	}
	firmware := filepath.Join(dir, vgpuRuntimeFirmwareDir)
	entries, err := os.ReadDir(firmware)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if err := add(filepath.Join(firmware, entry.Name())); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

func loadVGPURuntime(p *paths.Paths) (vgpuRuntime, error) {
	dir, err := vgpuRuntimeDir(p)
	if err != nil {
		return vgpuRuntime{}, err
	}
	fingerprint, err := vgpuRuntimeFingerprint(dir)
	if err != nil {
		return vgpuRuntime{}, fmt.Errorf("inspect managed QEMU runtime: %w", err)
	}
	vgpuRuntimeCache.Lock()
	defer vgpuRuntimeCache.Unlock()
	if entry, ok := vgpuRuntimeCache.entries[dir]; ok && entry.fingerprint == fingerprint {
		return entry.runtime, nil
	}
	rt, err := verifyVGPURuntime(dir)
	if err != nil {
		delete(vgpuRuntimeCache.entries, dir)
		return vgpuRuntime{}, err
	}
	if vgpuRuntimeCache.entries == nil {
		vgpuRuntimeCache.entries = make(map[string]vgpuRuntimeCacheEntry)
	}
	vgpuRuntimeCache.entries[dir] = vgpuRuntimeCacheEntry{fingerprint: fingerprint, runtime: rt}
	return rt, nil
}

// verifyVGPURuntime checks the installation against its manifest and confirms
// the binary reports the pinned upstream version.
func verifyVGPURuntime(dir string) (vgpuRuntime, error) {
	rt := vgpuRuntime{
		binary:   filepath.Join(dir, vgpuRuntimeBinaryName),
		firmware: filepath.Join(dir, vgpuRuntimeFirmwareDir),
	}
	data, err := os.ReadFile(filepath.Join(dir, vgpuRuntimeManifestName))
	if err != nil {
		return vgpuRuntime{}, fmt.Errorf("read managed QEMU runtime manifest: %w", err)
	}
	if err := json.Unmarshal(data, &rt.manifest); err != nil {
		return vgpuRuntime{}, fmt.Errorf("decode managed QEMU runtime manifest: %w", err)
	}
	if rt.manifest.Version != VGPUSnapshotVersion || rt.manifest.SourceCommit != vgpuSourceCommit {
		return vgpuRuntime{}, fmt.Errorf("unsupported managed QEMU runtime manifest")
	}
	binarySum, err := fileSHA256(rt.binary)
	if err != nil {
		return vgpuRuntime{}, fmt.Errorf("verify managed runtime binary: %w", err)
	}
	if binarySum != rt.manifest.BinarySHA256 {
		return vgpuRuntime{}, fmt.Errorf("managed runtime binary checksum mismatch")
	}
	firmwareSum, err := firmwareSHA256(rt.firmware)
	if err != nil {
		return vgpuRuntime{}, fmt.Errorf("verify managed runtime firmware: %w", err)
	}
	if firmwareSum != rt.manifest.FirmwareSHA256 {
		return vgpuRuntime{}, fmt.Errorf("managed runtime firmware checksum mismatch")
	}
	if err := validateExecutable(rt.binary); err != nil {
		return vgpuRuntime{}, fmt.Errorf("managed QEMU binary: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	version, err := versionFromBinary(ctx, rt.binary)
	if err != nil {
		return vgpuRuntime{}, err
	}
	if version != vgpuUpstreamVersion {
		return vgpuRuntime{}, fmt.Errorf("managed QEMU version %s does not match %s", version, vgpuUpstreamVersion)
	}
	return rt, nil
}

func (s *Starter) resolveVGPURuntime(p *paths.Paths) (string, error) {
	if s.profile.hypervisorType() != hypervisor.TypeQEMU {
		return "", fmt.Errorf("vGPU snapshot runtime requires standard QEMU")
	}
	if _, err := loadVGPURuntime(p); err != nil {
		return "", err
	}
	return VGPUSnapshotVersion, nil
}

func newVGPUSnapshotContract(p *paths.Paths, config hypervisor.VMConfig) (*vgpuSnapshotContract, error) {
	if config.VGPUDevicePath == "" || len(config.PCIDevices) != 0 || config.EffectiveBootMode() != hypervisor.BootModeDirect || config.InstanceID == "" {
		return nil, fmt.Errorf("vGPU snapshots require an identified direct-boot guest with one vendor VFIO vGPU and no other PCI assignments")
	}
	if !strings.HasPrefix(config.VGPUDevicePath, "/sys/bus/pci/devices/") {
		return nil, fmt.Errorf("vGPU snapshots require vendor VFIO")
	}
	rt, err := loadVGPURuntime(p)
	if err != nil {
		return nil, err
	}
	read := func(path string) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", fmt.Errorf("empty compatibility field %s", path)
		}
		return value, nil
	}
	contract := &vgpuSnapshotContract{Runtime: rt.manifest}
	if contract.DriverVersion, err = read("/sys/module/nvidia/version"); err != nil {
		return nil, fmt.Errorf("read NVIDIA host driver: %w", err)
	}
	if contract.VendorID, err = read(filepath.Join(config.VGPUDevicePath, "vendor")); err != nil {
		return nil, err
	}
	if contract.DeviceID, err = read(filepath.Join(config.VGPUDevicePath, "device")); err != nil {
		return nil, err
	}
	if contract.ProfileType, err = read(filepath.Join(config.VGPUDevicePath, "nvidia", "current_vgpu_type")); err != nil {
		return nil, err
	}
	if contract.ProfileType == "0" {
		return nil, fmt.Errorf("vGPU snapshot requires an allocated profile")
	}
	if contract.VendorID != "0x10de" {
		return nil, fmt.Errorf("vGPU snapshots require an NVIDIA device")
	}
	return contract, nil
}

func validateVGPUSnapshotContract(saved, current *vgpuSnapshotContract) error {
	if saved == nil || current == nil || *saved != *current {
		return fmt.Errorf("vGPU snapshot runtime, firmware or host driver/device changed; rebuild the template")
	}
	return nil
}

func vgpuRuntimeArgs(p *paths.Paths) ([]string, error) {
	rt, err := loadVGPURuntime(p)
	if err != nil {
		return nil, err
	}
	return []string{"-L", rt.firmware, "-bios", filepath.Join(rt.firmware, "bios-256k.bin")}, nil
}

// removeSnapshotMemory unlinks a previous snapshot's memory file. QEMU writes
// mapped RAM into an existing file in place, and forks hardlink the previous
// snapshot's memory, so a new snapshot must start from a new inode.
func removeSnapshotMemory(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove previous snapshot memory: %w", err)
	}
	return nil
}

func vgpuInstanceUUID(instanceID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(instanceID)).String()
}

func (c *Client) configureVGPUMigration() error {
	for _, command := range []qmp.Command{
		{Execute: "migrate-set-capabilities", Args: map[string]any{"capabilities": []map[string]any{{"capability": "mapped-ram", "state": true}, {"capability": "multifd", "state": true}}}},
		{Execute: "migrate-set-parameters", Args: map[string]any{"multifd-channels": 4, "max-bandwidth": int64(1 << 30), "downtime-limit": 10000}},
	} {
		if _, err := c.Run(command); err != nil {
			return fmt.Errorf("configure GPU migration: %w", err)
		}
	}
	return nil
}
