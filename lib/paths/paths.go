// Package paths provides centralized path construction for hypeman data directory.
//
// Directory Structure:
//
//	{dataDir}/
//	  system/
//	    kernel/{version}/{arch}/vmlinux
//	    initrd/{arch}/{timestamp}/initrd
//	    initrd/{arch}/latest -> {timestamp}
//	    binaries/{version}/{arch}/cloud-hypervisor
//	    oci-cache/
//	      oci-layout
//	      index.json
//	      blobs/sha256/{digestHex}
//	    builds/{ref}/
//	  images/
//	    {repository}/{digest}/
//	      rootfs.ext4
//	      metadata.json
//	    {repository}/{tag} -> {digest} (symlink)
//	  volumes/
//	    {id}/
//	      data.raw
//	      metadata.json
//	  guests/
//	    {id}/
//	      metadata.json
//	      overlay.raw
//	      config.ext4
//	      ch.sock
//	      vsock.sock
//	      logs/
//	      vol-overlays/
//	        {volumeID}.raw
//	      snapshots/
//	        snapshot-latest/
//	          config.json
package paths

import "path/filepath"

// Paths provides typed path construction for the hypeman data directory.
type Paths struct {
	dataDir string
}

// New creates a new Paths instance for the given data directory.
func New(dataDir string) *Paths {
	return &Paths{dataDir: dataDir}
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

// Image path methods

// ImageDigestDir returns the directory for a specific image digest.
func (p *Paths) ImageDigestDir(repository, digestHex string) string {
	return filepath.Join(p.dataDir, "images", repository, digestHex)
}

// ImageDigestPath returns the path to the rootfs disk file for a digest.
func (p *Paths) ImageDigestPath(repository, digestHex string) string {
	return filepath.Join(p.ImageDigestDir(repository, digestHex), "rootfs.ext4")
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
func (p *Paths) InstanceSocket(id string) string {
	return filepath.Join(p.InstanceDir(id), "ch.sock")
}

// InstanceVsockSocket returns the path to instance vsock socket.
func (p *Paths) InstanceVsockSocket(id string) string {
	return filepath.Join(p.InstanceDir(id), "vsock.sock")
}

// InstanceLogs returns the path to instance logs directory.
func (p *Paths) InstanceLogs(id string) string {
	return filepath.Join(p.InstanceDir(id), "logs")
}

// InstanceConsoleLog returns the path to instance console log file.
func (p *Paths) InstanceConsoleLog(id string) string {
	return filepath.Join(p.InstanceLogs(id), "console.log")
}

// InstanceSnapshots returns the path to instance snapshots directory.
func (p *Paths) InstanceSnapshots(id string) string {
	return filepath.Join(p.InstanceDir(id), "snapshots")
}

// InstanceSnapshotLatest returns the path to the latest snapshot directory.
func (p *Paths) InstanceSnapshotLatest(id string) string {
	return filepath.Join(p.InstanceSnapshots(id), "snapshot-latest")
}

// InstanceSnapshotConfig returns the path to the snapshot config.json file.
// Cloud Hypervisor creates config.json in the snapshot directory.
func (p *Paths) InstanceSnapshotConfig(id string) string {
	return filepath.Join(p.InstanceSnapshotLatest(id), "config.json")
}

// GuestsDir returns the root guests directory.
func (p *Paths) GuestsDir() string {
	return filepath.Join(p.dataDir, "guests")
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

// Envoy path methods

// EnvoyDir returns the envoy data directory.
func (p *Paths) EnvoyDir() string {
	return filepath.Join(p.dataDir, "envoy")
}

// EnvoyBinary returns the path to the envoy binary.
func (p *Paths) EnvoyBinary(version, arch string) string {
	return filepath.Join(p.dataDir, "system", "binaries", "envoy", version, arch, "envoy")
}

// EnvoyConfig returns the path to the envoy config file.
func (p *Paths) EnvoyConfig() string {
	return filepath.Join(p.EnvoyDir(), "config.yaml")
}

// EnvoyPIDFile returns the path to the envoy PID file.
func (p *Paths) EnvoyPIDFile() string {
	return filepath.Join(p.EnvoyDir(), "envoy.pid")
}

// EnvoyLogFile returns the path to the envoy log file.
func (p *Paths) EnvoyLogFile() string {
	return filepath.Join(p.EnvoyDir(), "envoy.log")
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
