package images

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/paths"
)

const (
	MachineImageVersionLabel    = "io.hypeman.machine-image.version"
	MachineImageKindLabel       = "io.hypeman.machine-image.kind"
	MachineImageDiskPathLabel   = "io.hypeman.machine-image.disk-path"
	MachineImageDiskFormatLabel = "io.hypeman.machine-image.disk-format"
	MachineImageBaseLabel       = "io.hypeman.machine-image.base"
	MachineImageTPMLabel        = "io.hypeman.machine-image.tpm"
	MachineImageSecureBootLabel = "io.hypeman.machine-image.secure-boot"

	MachineImageVersion = "1"
)

type MachineImageKind string

const (
	MachineImageWindowsBase  MachineImageKind = "windows-base"
	MachineImageWindowsImage MachineImageKind = "windows-image"
)

// MachineImage describes a bootable disk artifact. The OCI manifest remains
// the distribution envelope; this metadata controls materialization.
type MachineImage struct {
	Kind        MachineImageKind `json:"kind"`
	DiskPath    string           `json:"disk_path"`
	DiskFormat  string           `json:"disk_format"`
	Base        string           `json:"base,omitempty"`
	TPM         string           `json:"tpm"`
	SecureBoot  string           `json:"secure_boot"`
	VirtualSize int64            `json:"virtual_size"`
}

func parseMachineImage(meta *containerMetadata) (*MachineImage, error) {
	version := strings.TrimSpace(meta.Labels[MachineImageVersionLabel])
	if version == "" {
		if strings.EqualFold(meta.OS, "windows") {
			return nil, fmt.Errorf("ordinary Windows container images are not bootable; missing %s", MachineImageVersionLabel)
		}
		return nil, nil
	}
	if version != MachineImageVersion {
		return nil, fmt.Errorf("unsupported machine image version %q", version)
	}
	if !strings.EqualFold(meta.OS, "windows") || meta.Architecture != "amd64" {
		return nil, fmt.Errorf("machine image requires platform windows/amd64")
	}

	machine := &MachineImage{
		Kind:       MachineImageKind(strings.TrimSpace(meta.Labels[MachineImageKindLabel])),
		DiskPath:   strings.TrimSpace(meta.Labels[MachineImageDiskPathLabel]),
		DiskFormat: strings.TrimSpace(meta.Labels[MachineImageDiskFormatLabel]),
		Base:       strings.TrimSpace(meta.Labels[MachineImageBaseLabel]),
		TPM:        strings.TrimSpace(meta.Labels[MachineImageTPMLabel]),
		SecureBoot: strings.TrimSpace(meta.Labels[MachineImageSecureBootLabel]),
	}
	if machine.DiskPath == "" || filepath.IsAbs(machine.DiskPath) || !filepath.IsLocal(machine.DiskPath) {
		return nil, fmt.Errorf("machine image disk path must be a local relative path")
	}
	if machine.TPM != "2.0" {
		return nil, fmt.Errorf("machine image requires TPM 2.0")
	}
	if machine.SecureBoot != "required" {
		return nil, fmt.Errorf("machine image must require Secure Boot")
	}

	switch machine.Kind {
	case MachineImageWindowsBase:
		switch machine.DiskFormat {
		case "raw", "qcow2", "vhd", "vhdx":
		default:
			return nil, fmt.Errorf("unsupported Windows base disk format %q", machine.DiskFormat)
		}
		if machine.Base != "" {
			return nil, fmt.Errorf("Windows base image cannot reference another base")
		}
	case MachineImageWindowsImage:
		if machine.DiskFormat != "qcow2" {
			return nil, fmt.Errorf("Windows image disk format must be qcow2")
		}
		base, err := ParseNormalizedRef(machine.Base)
		if err != nil || !base.IsDigest() {
			return nil, fmt.Errorf("Windows image base must be a digest-pinned OCI reference")
		}
	default:
		return nil, fmt.Errorf("unsupported machine image kind %q", machine.Kind)
	}
	return machine, nil
}

func machineArtifactDisk(root string, machine *MachineImage) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve machine artifact root: %w", err)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(machine.DiskPath)))
	if err != nil {
		return "", fmt.Errorf("resolve machine image disk: %w", err)
	}
	rel, err := filepath.Rel(resolvedRoot, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("machine image disk path escapes artifact root")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat machine image disk: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("machine image disk is not a regular file")
	}
	return path, nil
}

type qemuImageInfo struct {
	Format            string `json:"format"`
	VirtualSize       int64  `json:"virtual-size"`
	BackingFilename   string `json:"backing-filename"`
	BackingFileFormat string `json:"backing-filename-format"`
	FormatSpecific    struct {
		Type string                     `json:"type"`
		Data map[string]json.RawMessage `json:"data"`
	} `json:"format-specific"`
}

func inspectQEMUImage(path, format string) (qemuImageInfo, error) {
	output, err := exec.Command("qemu-img", "info", "--output=json", "-f", format, path).CombinedOutput()
	if err != nil {
		return qemuImageInfo{}, fmt.Errorf("inspect machine disk: %w: %s", err, output)
	}
	var info qemuImageInfo
	if err := json.Unmarshal(output, &info); err != nil {
		return qemuImageInfo{}, fmt.Errorf("decode qemu-img info: %w", err)
	}
	return info, nil
}

func validateMachineSource(info qemuImageInfo, allowBacking bool) error {
	if !allowBacking && info.BackingFilename != "" {
		return fmt.Errorf("machine image source must not reference a backing file")
	}
	for _, feature := range []string{"data-file", "data-file-raw", "encrypt", "encryption", "encrypt-format"} {
		if _, ok := info.FormatSpecific.Data[feature]; ok {
			return fmt.Errorf("machine image source uses unsupported %s feature", feature)
		}
	}
	return nil
}

func qemuDiskFormat(format string) string {
	if format == "vhd" {
		return "vpc"
	}
	return format
}

func (m *manager) materializeMachineImage(root string, machine *MachineImage, destination string) (int64, error) {
	source, err := machineArtifactDisk(root, machine)
	if err != nil {
		return 0, err
	}
	sourceFormat := qemuDiskFormat(machine.DiskFormat)
	sourceInfo, err := inspectQEMUImage(source, sourceFormat)
	if err != nil {
		return 0, err
	}
	if sourceInfo.Format != sourceFormat {
		return 0, fmt.Errorf("machine image source format is %s, expected %s", sourceInfo.Format, sourceFormat)
	}
	if err := validateMachineSource(sourceInfo, machine.Kind == MachineImageWindowsImage); err != nil {
		return 0, err
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return 0, fmt.Errorf("create machine image directory: %w", err)
	}
	_ = os.Remove(destination)
	removeOnError := true
	defer func() {
		if removeOnError {
			_ = os.Remove(destination)
		}
	}()

	switch machine.Kind {
	case MachineImageWindowsBase:
		if machine.DiskFormat == "raw" {
			err = forkvm.CopyRegularFile(source, destination)
		} else {
			output, convertErr := exec.Command("qemu-img", "convert", "-f", sourceFormat, "-O", "raw", source, destination).CombinedOutput()
			if convertErr != nil {
				err = fmt.Errorf("convert Windows base to raw: %w: %s", convertErr, output)
			}
		}
		if err != nil {
			return 0, fmt.Errorf("materialize Windows base: %w", err)
		}
		info, err := inspectQEMUImage(destination, "raw")
		if err != nil {
			return 0, err
		}
		if info.Format != "raw" {
			return 0, fmt.Errorf("Windows base disk must be raw, got %s", info.Format)
		}
		machine.VirtualSize = info.VirtualSize
	case MachineImageWindowsImage:
		if err := forkvm.CopyRegularFile(source, destination); err != nil {
			return 0, fmt.Errorf("materialize Windows image: %w", err)
		}
		if err := os.Chmod(destination, 0600); err != nil {
			return 0, fmt.Errorf("make Windows image writable for validation: %w", err)
		}
		basePath, err := m.resolveMachineBase(machine.Base)
		if err != nil {
			return 0, err
		}
		output, err := exec.Command("qemu-img", "rebase", "-u", "-f", "qcow2", "-F", "raw", "-b", basePath, destination).CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("set image backing file: %w: %s", err, output)
		}
		info, err := inspectQEMUImage(destination, "qcow2")
		if err != nil {
			return 0, err
		}
		if err := validateMachineSource(info, true); err != nil {
			return 0, err
		}
		if info.Format != "qcow2" || info.BackingFileFormat != "raw" || info.BackingFilename != basePath {
			return 0, fmt.Errorf("invalid Windows image disk backing configuration")
		}
		baseInfo, err := inspectQEMUImage(basePath, "raw")
		if err != nil {
			return 0, err
		}
		if info.VirtualSize != baseInfo.VirtualSize {
			return 0, fmt.Errorf("image virtual size %d does not match base %d", info.VirtualSize, baseInfo.VirtualSize)
		}
		machine.VirtualSize = info.VirtualSize
	}

	if err := os.Chmod(destination, 0444); err != nil {
		return 0, fmt.Errorf("make machine disk immutable: %w", err)
	}
	stat, err := os.Stat(destination)
	if err != nil {
		return 0, fmt.Errorf("stat materialized machine disk: %w", err)
	}
	removeOnError = false
	return stat.Size(), nil
}

func (m *manager) resolveMachineBase(reference string) (string, error) {
	ref, err := ParseNormalizedRef(reference)
	if err != nil || !ref.IsDigest() {
		return "", fmt.Errorf("parse machine base reference")
	}
	meta, err := readMetadata(m.paths, ref.Repository(), ref.DigestHex())
	if err != nil {
		return "", fmt.Errorf("get machine base %s: %w", reference, err)
	}
	if meta.Status != StatusReady || meta.Machine == nil || meta.Machine.Kind != MachineImageWindowsBase {
		return "", fmt.Errorf("machine base %s is not a ready Windows base", reference)
	}
	return machineDiskPath(m.paths, ref.Repository(), ref.DigestHex(), MachineImageWindowsBase), nil
}

func machineDiskPath(p *paths.Paths, repository, digestHex string, kind MachineImageKind) string {
	return machineDiskPathInDir(resolveImageLayout(p, repository, digestHex).dir, kind)
}

func machineDiskPathInDir(dir string, kind MachineImageKind) string {
	name := "base.raw"
	if kind == MachineImageWindowsImage {
		name = "image.qcow2"
	}
	return filepath.Join(dir, name)
}

func (m *manager) recordMachineDependency(ref *ResolvedRef, machine *MachineImage, buildID string) error {
	m.createMu.Lock()
	defer m.createMu.Unlock()

	meta, err := readMetadata(m.paths, ref.Repository(), ref.DigestHex())
	if err != nil || meta.BuildID != buildID {
		return errStaleBuild
	}
	meta.Machine = machine
	if err := writeMetadata(m.paths, ref.Repository(), ref.DigestHex(), meta); err != nil {
		return fmt.Errorf("record machine image dependency: %w", err)
	}
	return nil
}

func (m *manager) ensureNoMachineDependents(repository, digestHex string) error {
	metas, err := listAllMetadata(m.paths)
	if err != nil {
		return err
	}
	for _, meta := range metas {
		if meta.Status == StatusFailed || meta.Machine == nil || meta.Machine.Kind != MachineImageWindowsImage {
			continue
		}
		base, err := ParseNormalizedRef(meta.Machine.Base)
		if err == nil && base.Repository() == repository && base.DigestHex() == digestHex {
			return fmt.Errorf("cannot delete Windows base while image %s depends on it", meta.Name)
		}
	}
	return nil
}

// GetMachineDiskPath returns the materialized disk for a machine image.
func GetMachineDiskPath(p *paths.Paths, imageName, digest string, machine *MachineImage) (string, error) {
	if machine == nil {
		return "", fmt.Errorf("image is not a machine image")
	}
	ref, err := ParseNormalizedRef(imageName)
	if err != nil {
		return "", fmt.Errorf("parse image name: %w", err)
	}
	return machineDiskPath(p, ref.Repository(), strings.TrimPrefix(digest, "sha256:"), machine.Kind), nil
}

// IsWindowsImage reports whether an image is directly launchable as a Windows desktop.
func IsWindowsImage(image *Image) bool {
	return image != nil && image.Machine != nil && image.Machine.Kind == MachineImageWindowsImage
}
