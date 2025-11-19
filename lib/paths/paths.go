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
//	    builds/{ref}/
//	  images/
//	    {repository}/{digest}/
//	      rootfs.ext4
//	      metadata.json
//	    {repository}/{tag} -> {digest} (symlink)
//	  guests/
//	    {id}/
//	      metadata.json
//	      overlay.raw
//	      config.ext4
//	      ch.sock
//	      logs/
//	      snapshots/
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

// GuestsDir returns the root guests directory.
func (p *Paths) GuestsDir() string {
	return filepath.Join(p.dataDir, "guests")
}

