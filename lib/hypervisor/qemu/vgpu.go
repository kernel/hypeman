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
)

type vgpuRuntimeManifest struct {
	Version        string `json:"version"`
	SourceCommit   string `json:"source_commit"`
	BinarySHA256   string `json:"binary_sha256"`
	FirmwareSHA256 string `json:"firmware_sha256"`
}

type vgpuSnapshotContract struct {
	Runtime       vgpuRuntimeManifest
	DriverVersion string
	VendorID      string
	DeviceID      string
	ProfileType   string
}

func vgpuRuntimeDir(p *paths.Paths) (string, error) {
	if p == nil || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", fmt.Errorf("vGPU snapshots require a managed Linux amd64 runtime")
	}
	return filepath.Join(p.DataDir(), "system", "binaries", "qemu", VGPUSnapshotVersion, "x86_64"), nil
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

func loadVGPURuntime(p *paths.Paths) (string, vgpuRuntimeManifest, error) {
	dir, err := vgpuRuntimeDir(p)
	if err != nil {
		return "", vgpuRuntimeManifest{}, err
	}
	var manifest vgpuRuntimeManifest
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "", manifest, fmt.Errorf("read managed QEMU runtime manifest: %w", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", manifest, fmt.Errorf("decode managed QEMU runtime manifest: %w", err)
	}
	if manifest.Version != VGPUSnapshotVersion || manifest.SourceCommit != vgpuSourceCommit {
		return "", manifest, fmt.Errorf("unsupported managed QEMU runtime manifest")
	}
	for name, expected := range map[string]string{"qemu-system-x86_64": manifest.BinarySHA256, "firmware": manifest.FirmwareSHA256} {
		var actual string
		var err error
		if name == "firmware" {
			actual, err = firmwareSHA256(filepath.Join(dir, name))
		} else {
			actual, err = fileSHA256(filepath.Join(dir, name))
		}
		if err != nil {
			return "", manifest, fmt.Errorf("verify managed runtime %s: %w", name, err)
		}
		if len(expected) != 64 || actual != expected {
			return "", manifest, fmt.Errorf("managed runtime %s checksum mismatch", name)
		}
	}
	binary := filepath.Join(dir, "qemu-system-x86_64")
	if err := validateExecutable(binary); err != nil {
		return "", manifest, fmt.Errorf("managed QEMU binary: %w", err)
	}
	return binary, manifest, nil
}

func (s *Starter) resolveVGPURuntime(p *paths.Paths) (string, error) {
	if s.profile.hypervisorType() != hypervisor.TypeQEMU {
		return "", fmt.Errorf("vGPU snapshot runtime requires standard QEMU")
	}
	binary, _, err := loadVGPURuntime(p)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	version, err := versionFromBinary(ctx, binary)
	if err != nil {
		return "", err
	}
	if version != vgpuUpstreamVersion {
		return "", fmt.Errorf("managed QEMU version %s does not match %s", version, vgpuUpstreamVersion)
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
	_, manifest, err := loadVGPURuntime(p)
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
	contract := &vgpuSnapshotContract{Runtime: manifest}
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
	dir, err := vgpuRuntimeDir(p)
	if err != nil {
		return nil, err
	}
	return []string{"-L", filepath.Join(dir, "firmware"), "-bios", filepath.Join(dir, "firmware", "bios-256k.bin")}, nil
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
