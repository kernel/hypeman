// Package paths provides centralized path construction for hypeman data directory.
package paths

import (
	"errors"
	"path/filepath"
	"runtime"
)

// Paths provides typed path construction for the hypeman data directory.
type Paths struct {
	dataDir string
}

// New creates a new Paths instance for the given data directory.
func New(dataDir string) *Paths {
	return &Paths{dataDir: dataDir}
}

// ErrInvalidPathComponent is returned when a value cannot be safely joined as one path component.
var ErrInvalidPathComponent = errors.New("invalid path component")

// ValidatePathComponent rejects empty, nested, absolute, and traversing paths.
func ValidatePathComponent(value string) error {
	if value == "." || !filepath.IsLocal(value) || filepath.Base(value) != value {
		return ErrInvalidPathComponent
	}
	return nil
}

// DataDir returns the root data directory.
func (p *Paths) DataDir() string {
	return p.dataDir
}

// System path methods

// SystemKernel returns the path to a kernel file.
func (p *Paths) SystemKernel(version, arch string) string {
	return filepath.Join(p.dataDir, "system", "kernel", version, arch, "vmlinux")
}

// SystemInitrd returns the path to the latest initrd symlink.
func (p *Paths) SystemInitrd(arch string) string {
	return filepath.Join(p.dataDir, "system", "initrd", arch, "latest")
}

// SystemInitrdTimestamp returns the path to a specific timestamped initrd build.
func (p *Paths) SystemInitrdTimestamp(timestamp, arch string) string {
	return filepath.Join(p.dataDir, "system", "initrd", arch, timestamp, "initrd")
}

// SystemInitrdLatest returns the path to the latest symlink (same as SystemInitrd).
func (p *Paths) SystemInitrdLatest(arch string) string {
	return filepath.Join(p.dataDir, "system", "initrd", arch, "latest")
}

// SystemInitrdDir returns the directory for initrd builds for an architecture.
func (p *Paths) SystemInitrdDir(arch string) string {
	return filepath.Join(p.dataDir, "system", "initrd", arch)
}

// SystemOCICache returns the path to the OCI cache directory.
func (p *Paths) SystemOCICache() string {
	return filepath.Join(p.dataDir, "system", "oci-cache")
}

// SystemImageRetentionDir returns the path to persisted image retention state.
func (p *Paths) SystemImageRetentionDir() string {
	return filepath.Join(p.dataDir, "system", "image-retention")
}

// ImageRetentionRepositoryDir returns the directory for image retention state for one repository.
func (p *Paths) ImageRetentionRepositoryDir(repository string) string {
	return filepath.Join(p.SystemImageRetentionDir(), repository)
}

// ImageRetentionState returns the state file path for one retained image digest.
func (p *Paths) ImageRetentionState(repository, digestHex string) string {
	return filepath.Join(p.ImageRetentionRepositoryDir(repository), digestHex+".json")
}

// OCICacheBlobDir returns the path to the OCI cache blobs directory.
func (p *Paths) OCICacheBlobDir() string {
	return filepath.Join(p.SystemOCICache(), "blobs", "sha256")
}

// OCICacheBlob returns the path to a specific blob in the OCI cache.
func (p *Paths) OCICacheBlob(digestHex string) string {
	return filepath.Join(p.OCICacheBlobDir(), digestHex)
}

// OCICacheIndex returns the path to the OCI cache index.json.
func (p *Paths) OCICacheIndex() string {
	return filepath.Join(p.SystemOCICache(), "index.json")
}

// OCICacheLayout returns the path to the OCI cache oci-layout file.
func (p *Paths) OCICacheLayout() string {
	return filepath.Join(p.SystemOCICache(), "oci-layout")
}

// SystemBuild returns the path to a system build directory.
func (p *Paths) SystemBuild(ref string) string {
	return filepath.Join(p.dataDir, "system", "builds", ref)
}

// SystemBinary returns the path to a VMM binary.
func (p *Paths) SystemBinary(version, arch string) string {
	return filepath.Join(p.dataDir, "system", "binaries", version, arch, "cloud-hypervisor")
}

// FirecrackerBinary returns the path to a firecracker VMM binary.
func (p *Paths) FirecrackerBinary(version, arch string) string {
	return filepath.Join(p.dataDir, "system", "binaries", "firecracker", version, arch, "firecracker")
}

// UFFDDir returns the root directory for Firecracker UFFD pager state.
func (p *Paths) UFFDDir() string {
	return filepath.Join(p.dataDir, "uffd")
}

func (p *Paths) UFFDPagerDir(versionKey string) string {
	return filepath.Join(p.UFFDDir(), versionKey)
}

func (p *Paths) UFFDControlSocket(versionKey string) string {
	return filepath.Join(p.UFFDPagerDir(versionKey), "control.sock")
}

func (p *Paths) UFFDPagerPID(versionKey string) string {
	return filepath.Join(p.UFFDPagerDir(versionKey), "pager.pid")
}

func (p *Paths) UFFDPagerLog(versionKey string) string {
	return filepath.Join(p.UFFDPagerDir(versionKey), "pager.log")
}

func (p *Paths) UFFDSessionsDir(versionKey string) string {
	return filepath.Join(p.UFFDPagerDir(versionKey), "sessions")
}

// Image path methods

// ImageContentDir returns the directory for content-addressed image data.
func (p *Paths) ImageContentDir(digestHex string) string {
	return filepath.Join(p.dataDir, "images", "content", digestHex)
}

// ImageContentPath returns the path to a content-addressed rootfs disk file.
func (p *Paths) ImageContentPath(digestHex string) string {
	ext := "erofs"
	if runtime.GOOS == "darwin" {
		ext = "ext4"
	}
	return filepath.Join(p.ImageContentDir(digestHex), "rootfs."+ext)
}

// ImageContentMetadata returns the path to metadata for content-addressed image data.
func (p *Paths) ImageContentMetadata(digestHex string) string {
	return filepath.Join(p.ImageContentDir(digestHex), "metadata.json")
}

// ImageContentManifestModel returns the path to the persisted OCI manifest
// model (layer descriptors, config, platform) for content-addressed image data.
func (p *Paths) ImageContentManifestModel(digestHex string) string {
	return filepath.Join(p.ImageContentDir(digestHex), "manifest.json")
}

// ImageRepositoriesDir returns the root directory for repository tag references.
func (p *Paths) ImageRepositoriesDir() string {
	return filepath.Join(p.dataDir, "images", "repositories")
}

// ImageRepositoryTagSymlink returns the path to a tag reference in the new layout.
func (p *Paths) ImageRepositoryTagSymlink(repository, tag string) string {
	return filepath.Join(p.ImageRepositoriesDir(), repository, tag)
}

// ImageLayersDir returns the root directory of the per-layer artifact store.
// Layer artifacts are content-addressed by the compressed layer blob digest.
func (p *Paths) ImageLayersDir() string {
	return filepath.Join(p.dataDir, "images", "layers")
}

// ImageLayerDir returns the artifact directory for one layer digest.
func (p *Paths) ImageLayerDir(layerHex string) string {
	return filepath.Join(p.ImageLayersDir(), layerHex)
}

// ImageLayerArtifactForFormat returns the path to a materialized layer artifact.
func (p *Paths) ImageLayerArtifactForFormat(layerHex, format string) string {
	return filepath.Join(p.ImageLayerDir(layerHex), "layer."+format)
}

// ImageLayerRecordForFormat returns the path to the artifact record for one
// materialized layer and format.
func (p *Paths) ImageLayerRecordForFormat(layerHex, format string) string {
	return filepath.Join(p.ImageLayerDir(layerHex), "artifact."+format+".json")
}

// ImageDigestDir returns the directory for a specific image digest.
func (p *Paths) ImageDigestDir(repository, digestHex string) string {
	return filepath.Join(p.dataDir, "images", repository, digestHex)
}

// ImageDigestPath returns the path to the rootfs disk file for a digest.
// Uses .erofs on Linux (compressed) and .ext4 on Darwin (VZ kernel lacks erofs support).
func (p *Paths) ImageDigestPath(repository, digestHex string) string {
	ext := "erofs"
	if runtime.GOOS == "darwin" {
		ext = "ext4"
	}
	return filepath.Join(p.ImageDigestDir(repository, digestHex), "rootfs."+ext)
}

// ImageMetadata returns the path to metadata.json for a digest.
func (p *Paths) ImageMetadata(repository, digestHex string) string {
	return filepath.Join(p.ImageDigestDir(repository, digestHex), "metadata.json")
}

// ImageTagSymlink returns the path to a tag symlink.
func (p *Paths) ImageTagSymlink(repository, tag string) string {
	return filepath.Join(p.dataDir, "images", repository, tag)
}

// ImageRepositoryDir returns the directory for an image repository.
func (p *Paths) ImageRepositoryDir(repository string) string {
	return filepath.Join(p.dataDir, "images", repository)
}

// ImagesDir returns the root images directory.
func (p *Paths) ImagesDir() string {
	return filepath.Join(p.dataDir, "images")
}

// Instance path methods

// InstanceDir returns the directory for an instance.
func (p *Paths) InstanceDir(id string) string {
	return filepath.Join(p.dataDir, "guests", id)
}

// InstanceMetadata returns the path to instance metadata.json.
func (p *Paths) InstanceMetadata(id string) string {
	return filepath.Join(p.InstanceDir(id), "metadata.json")
}

// InstanceOverlay returns the path to instance overlay disk.
func (p *Paths) InstanceOverlay(id string) string {
	return filepath.Join(p.InstanceDir(id), "overlay.raw")
}

// InstanceConfigDisk returns the path to instance config disk.
func (p *Paths) InstanceConfigDisk(id string) string {
	return filepath.Join(p.InstanceDir(id), "config.ext4")
}

// InstanceVolumeOverlay returns the path to a volume's overlay disk for an instance.
func (p *Paths) InstanceVolumeOverlay(instanceID, volumeID string) string {
	return filepath.Join(p.InstanceDir(instanceID), "vol-overlays", volumeID+".raw")
}

// InstanceVolumeOverlaysDir returns the directory for volume overlays.
func (p *Paths) InstanceVolumeOverlaysDir(instanceID string) string {
	return filepath.Join(p.InstanceDir(instanceID), "vol-overlays")
}

// InstanceSocket returns the path to instance API socket.
// The socketName should be obtained from hypervisor.Type.SocketName() to ensure
// it stays within Unix socket path length limits (SUN_LEN ~108 bytes).
func (p *Paths) InstanceSocket(id string, socketName string) string {
	return filepath.Join(p.InstanceDir(id), socketName)
}

// InstanceVsockSocket returns the path to instance vsock socket.
func (p *Paths) InstanceVsockSocket(id string) string {
	return filepath.Join(p.InstanceDir(id), "vsock.sock")
}

// InstanceLogs returns the path to instance logs directory.
func (p *Paths) InstanceLogs(id string) string {
	return filepath.Join(p.InstanceDir(id), "logs")
}

// InstanceAppLog returns the path to instance application log (guest serial console).
func (p *Paths) InstanceAppLog(id string) string {
	return filepath.Join(p.InstanceLogs(id), "app.log")
}

// InstanceVMMLog returns the path to instance VMM log (Cloud Hypervisor stdout+stderr).
func (p *Paths) InstanceVMMLog(id string) string {
	return filepath.Join(p.InstanceLogs(id), "vmm.log")
}

// InstanceHypemanLog returns the path to instance hypeman operations log.
func (p *Paths) InstanceHypemanLog(id string) string {
	return filepath.Join(p.InstanceLogs(id), "hypeman.log")
}

// InstanceSWTPMLog returns the path to the instance software TPM log.
func (p *Paths) InstanceSWTPMLog(id string) string {
	return filepath.Join(p.InstanceLogs(id), "swtpm.log")
}

// InstanceSnapshots returns the path to instance snapshots directory.
func (p *Paths) InstanceSnapshots(id string) string {
	return filepath.Join(p.InstanceDir(id), "snapshots")
}

// InstanceSnapshotLatest returns the path to the latest snapshot directory.
func (p *Paths) InstanceSnapshotLatest(id string) string {
	return filepath.Join(p.InstanceSnapshots(id), "snapshot-latest")
}

// InstanceSnapshotBase returns the hidden retained snapshot base.
func (p *Paths) InstanceSnapshotBase(id string) string {
	return filepath.Join(p.InstanceSnapshots(id), "snapshot-base")
}

// InstanceSnapshotConfig returns the path to the snapshot config.json file.
// Cloud Hypervisor creates config.json in the snapshot directory.
func (p *Paths) InstanceSnapshotConfig(id string) string {
	return filepath.Join(p.InstanceSnapshotLatest(id), "config.json")
}

// InstanceSnapshotSchedule returns the path to the snapshot schedule config.
func (p *Paths) InstanceSnapshotSchedule(id string) string {
	return filepath.Join(p.SnapshotSchedulesDir(), id+".json")
}

// SnapshotSchedulesDir returns the root directory for instance snapshot schedules.
func (p *Paths) SnapshotSchedulesDir() string {
	return filepath.Join(p.dataDir, "snapshot-schedules")
}

// GuestsDir returns the root guests directory.
func (p *Paths) GuestsDir() string {
	return filepath.Join(p.dataDir, "guests")
}

// SnapshotStoreDir returns the root directory for centrally managed snapshots.
func (p *Paths) SnapshotStoreDir() string {
	return filepath.Join(p.dataDir, "snapshots")
}

// SnapshotDir returns the directory for a specific snapshot.
func (p *Paths) SnapshotDir(snapshotID string) string {
	return filepath.Join(p.SnapshotStoreDir(), snapshotID)
}

// SnapshotMetadata returns the path to snapshot metadata.json.
func (p *Paths) SnapshotMetadata(snapshotID string) string {
	return filepath.Join(p.SnapshotDir(snapshotID), "snapshot.json")
}

// SnapshotGuestDir returns the path to the copied guest payload for a snapshot.
func (p *Paths) SnapshotGuestDir(snapshotID string) string {
	return filepath.Join(p.SnapshotDir(snapshotID), "guest")
}

// Device path methods

// DevicesDir returns the root devices directory.
func (p *Paths) DevicesDir() string {
	return filepath.Join(p.dataDir, "devices")
}

// DeviceDir returns the directory for a device.
func (p *Paths) DeviceDir(id string) string {
	return filepath.Join(p.DevicesDir(), id)
}

// DeviceMetadata returns the path to device metadata.json.
func (p *Paths) DeviceMetadata(id string) string {
	return filepath.Join(p.DeviceDir(id), "metadata.json")
}

// VFHealthState returns the path to the persisted vGPU VF health file.
func (p *Paths) VFHealthState() string {
	return filepath.Join(p.dataDir, "gpu", "vf-health.json")
}

// Volume path methods

// VolumesDir returns the root volumes directory.
func (p *Paths) VolumesDir() string {
	return filepath.Join(p.dataDir, "volumes")
}

// VolumeDir returns the directory for a volume.
func (p *Paths) VolumeDir(id string) string {
	return filepath.Join(p.dataDir, "volumes", id)
}

// VolumeData returns the path to the volume data file.
func (p *Paths) VolumeData(id string) string {
	return filepath.Join(p.VolumeDir(id), "data.raw")
}

// VolumeMetadata returns the path to volume metadata.json.
func (p *Paths) VolumeMetadata(id string) string {
	return filepath.Join(p.VolumeDir(id), "metadata.json")
}

// Caddy path methods

// CaddyDir returns the caddy data directory.
func (p *Paths) CaddyDir() string {
	return filepath.Join(p.dataDir, "caddy")
}

// CaddyBinary returns the path to the caddy binary.
func (p *Paths) CaddyBinary(version, arch string) string {
	return filepath.Join(p.dataDir, "system", "binaries", "caddy", version, arch, "caddy")
}

// CaddyConfig returns the path to the caddy config file.
func (p *Paths) CaddyConfig() string {
	return filepath.Join(p.CaddyDir(), "config.json")
}

// CaddyPIDFile returns the path to the caddy PID file.
func (p *Paths) CaddyPIDFile() string {
	return filepath.Join(p.CaddyDir(), "caddy.pid")
}

// CaddyLogFile returns the path to the caddy log file.
func (p *Paths) CaddyLogFile() string {
	return filepath.Join(p.CaddyDir(), "caddy.log")
}

// CaddyDataDir returns the path to Caddy's data directory (for certs, etc.).
func (p *Paths) CaddyDataDir() string {
	return filepath.Join(p.CaddyDir(), "data")
}

// CaddyConfigDir returns the path to Caddy's config directory.
func (p *Paths) CaddyConfigDir() string {
	return filepath.Join(p.CaddyDir(), "config")
}

// Ingress path methods

// IngressesDir returns the root ingresses directory.
func (p *Paths) IngressesDir() string {
	return filepath.Join(p.dataDir, "ingresses")
}

// IngressMetadata returns the path to ingress metadata.json.
func (p *Paths) IngressMetadata(id string) string {
	return filepath.Join(p.IngressesDir(), id+".json")
}

// Builder path methods

// BuildersDir returns the root builders directory.
func (p *Paths) BuildersDir() string {
	return filepath.Join(p.dataDir, "builders")
}

// BuilderDir returns the directory for a specific builder.
func (p *Paths) BuilderDir(id string) string {
	return filepath.Join(p.BuildersDir(), id)
}

// BuilderMetadata returns the path to builder metadata.json.
func (p *Paths) BuilderMetadata(id string) string {
	return filepath.Join(p.BuilderDir(id), "metadata.json")
}

// Build path methods

// BuildsDir returns the root builds directory.
func (p *Paths) BuildsDir() string {
	return filepath.Join(p.dataDir, "builds")
}

// BuildDir returns the directory for a specific build.
func (p *Paths) BuildDir(id string) string {
	return filepath.Join(p.BuildsDir(), id)
}

// BuildMetadata returns the path to build metadata.json.
func (p *Paths) BuildMetadata(id string) string {
	return filepath.Join(p.BuildDir(id), "metadata.json")
}

// Push path methods

// PushesDir returns the root pushes directory.
func (p *Paths) PushesDir() string {
	return filepath.Join(p.dataDir, "pushes")
}

// PushDir returns the directory for a specific push.
func (p *Paths) PushDir(id string) string {
	return filepath.Join(p.PushesDir(), id)
}

// PushMetadata returns the path to push metadata.json.
func (p *Paths) PushMetadata(id string) string {
	return filepath.Join(p.PushDir(id), "metadata.json")
}

// BuildLogs returns the path to build logs directory.
func (p *Paths) BuildLogs(id string) string {
	return filepath.Join(p.BuildDir(id), "logs")
}

// BuildLog returns the path to the main build log file.
func (p *Paths) BuildLog(id string) string {
	return filepath.Join(p.BuildLogs(id), "build.log")
}

// BuildSourceDir returns the path to the source directory for a build.
func (p *Paths) BuildSourceDir(id string) string {
	return filepath.Join(p.BuildDir(id), "source")
}

// BuildConfig returns the path to the build config file (passed to builder VM).
func (p *Paths) BuildConfig(id string) string {
	return filepath.Join(p.BuildDir(id), "config.json")
}
