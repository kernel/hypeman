//go:build darwin

// Package vz implements the hypervisor.Hypervisor interface for
// Apple's Virtualization.framework on macOS via the vz-shim subprocess.
package vz

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
)

func init() {
	hypervisor.RegisterSocketName(hypervisor.TypeVZ, "vz.sock")
	hypervisor.RegisterVsockDialerFactory(hypervisor.TypeVZ, NewVsockDialer)
	hypervisor.RegisterClientFactory(hypervisor.TypeVZ, func(socketPath string) (hypervisor.Hypervisor, error) {
		return NewClient(socketPath)
	})
}

// ShimConfig is the configuration passed to the vz-shim process.
type ShimConfig struct {
	VCPUs            int             `json:"vcpus"`
	MemoryBytes      int64           `json:"memory_bytes"`
	Disks            []DiskConfig    `json:"disks"`
	Networks         []NetworkConfig `json:"networks"`
	SerialLogPath    string          `json:"serial_log_path"`
	KernelPath       string          `json:"kernel_path"`
	InitrdPath       string          `json:"initrd_path"`
	KernelArgs       string          `json:"kernel_args"`
	ControlSocket    string          `json:"control_socket"`
	VsockSocket      string          `json:"vsock_socket"`
	LogPath          string          `json:"log_path"`
	RestoreStatePath string          `json:"restore_state_path,omitempty"`
}

// DiskConfig for shim.
type DiskConfig struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

// NetworkConfig for shim.
type NetworkConfig struct {
	MAC string `json:"mac"`
}

// Starter implements hypervisor.VMStarter for Virtualization.framework.
type Starter struct{}

// NewStarter creates a new vz starter.
func NewStarter() *Starter {
	return &Starter{}
}

// Verify Starter implements the interface
var _ hypervisor.VMStarter = (*Starter)(nil)

// SocketName returns the socket filename for vz.
func (s *Starter) SocketName() string {
	return "vz.sock"
}

// GetBinaryPath returns empty - vz uses system Virtualization.framework.
func (s *Starter) GetBinaryPath(p *paths.Paths, version string) (string, error) {
	return "", nil
}

// GetVersion returns the macOS version as the "hypervisor version".
func (s *Starter) GetVersion(p *paths.Paths) (string, error) {
	// Return a version indicating vz availability
	return "vz-macos", nil
}

// StartVM spawns a vz-shim subprocess to host the VM.
// Returns the shim PID and a client to control the VM.
func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)

	// Derive socket paths from the control socket path
	instanceDir := filepath.Dir(socketPath)
	controlSocket := socketPath
	vsockSocket := filepath.Join(instanceDir, "vz.vsock")
	logPath := filepath.Join(instanceDir, "logs", "vz-shim.log")

	// Build shim config
	shimConfig := ShimConfig{
		VCPUs:         config.VCPUs,
		MemoryBytes:   config.MemoryBytes,
		SerialLogPath: config.SerialLogPath,
		KernelPath:    config.KernelPath,
		InitrdPath:    config.InitrdPath,
		KernelArgs:    config.KernelArgs,
		ControlSocket: controlSocket,
		VsockSocket:   vsockSocket,
		LogPath:       logPath,
	}

	// Convert disks
	for _, disk := range config.Disks {
		shimConfig.Disks = append(shimConfig.Disks, DiskConfig{
			Path:     disk.Path,
			Readonly: disk.Readonly,
		})
	}

	// Convert networks
	for _, net := range config.Networks {
		shimConfig.Networks = append(shimConfig.Networks, NetworkConfig{
			MAC: net.MAC,
		})
	}

	configJSON, err := json.Marshal(shimConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal shim config: %w", err)
	}

	log.DebugContext(ctx, "spawning vz-shim", "config", string(configJSON))

	// Find the vz-shim binary (same directory as hypeman or in PATH)
	shimPath, err := s.findShimBinary()
	if err != nil {
		return 0, nil, fmt.Errorf("find vz-shim binary: %w", err)
	}

	// Spawn the shim process
	cmd := exec.CommandContext(ctx, shimPath, "-config", string(configJSON))
	cmd.Stdout = nil // Shim logs to file
	cmd.Stderr = nil
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("start vz-shim: %w", err)
	}

	pid := cmd.Process.Pid
	log.InfoContext(ctx, "vz-shim started", "pid", pid, "control_socket", controlSocket)

	// Wait for the control socket to be ready
	client, err := s.waitForShim(ctx, controlSocket, 30*time.Second)
	if err != nil {
		// Kill the shim if we can't connect
		cmd.Process.Kill()
		return 0, nil, fmt.Errorf("connect to vz-shim: %w", err)
	}

	// Release the process so it's not killed when cmd goes out of scope
	cmd.Process.Release()

	return pid, client, nil
}

// findShimBinary locates the vz-shim binary.
func (s *Starter) findShimBinary() (string, error) {
	// First, check next to the current executable
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		shimPath := filepath.Join(exeDir, "vz-shim")
		if _, err := os.Stat(shimPath); err == nil {
			return shimPath, nil
		}
		// Also check parent's tmp dir (for air hot-reload development)
		// When running ./tmp/main, check ./tmp/vz-shim
		if filepath.Base(exeDir) == "tmp" {
			shimPath = filepath.Join(exeDir, "vz-shim")
			if _, err := os.Stat(shimPath); err == nil {
				return shimPath, nil
			}
		}
	}

	// Check in PATH
	shimPath, err := exec.LookPath("vz-shim")
	if err == nil {
		return shimPath, nil
	}

	// Check common locations
	commonPaths := []string{
		"/usr/local/bin/vz-shim",
		filepath.Join(os.Getenv("HOME"), "bin", "vz-shim"),
	}
	for _, p := range commonPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("vz-shim binary not found")
}

// waitForShim waits for the shim's control socket to be ready.
func (s *Starter) waitForShim(ctx context.Context, socketPath string, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		client, err := NewClient(socketPath)
		if err == nil {
			return client, nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return nil, fmt.Errorf("timeout waiting for shim socket: %s", socketPath)
}

// vzMetadata is the subset of instance metadata needed for restore.
type vzMetadata struct {
	Image         string `json:"Image"`
	VCPUs         int    `json:"Vcpus"`
	Size          int64  `json:"Size"` // memory in bytes
	KernelVersion string `json:"KernelVersion"`
	MAC           string `json:"MAC"`
}

// RestoreVM restores a VM from a snapshot.
// Unlike Cloud Hypervisor, vz snapshots only contain CPU/memory state, not VM config.
// We load the VM config from metadata.json in the instance directory.
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)

	// Derive paths from socketPath (which is in the instance directory)
	instanceDir := filepath.Dir(socketPath)
	controlSocket := socketPath
	vsockSocket := filepath.Join(instanceDir, "vz.vsock")
	logPath := filepath.Join(instanceDir, "logs", "vz-shim.log")

	// The snapshot file is inside snapshotPath directory
	restoreStatePath := filepath.Join(snapshotPath, "vz-state")
	if _, err := os.Stat(restoreStatePath); err != nil {
		return 0, nil, fmt.Errorf("snapshot file not found: %s", restoreStatePath)
	}

	// Load metadata to get VM config
	metadataPath := filepath.Join(instanceDir, "metadata.json")
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return 0, nil, fmt.Errorf("read metadata: %w", err)
	}

	var metadata vzMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return 0, nil, fmt.Errorf("parse metadata: %w", err)
	}

	// Build disk list - order matters for vz (vda, vdb, vdc...)
	var disks []DiskConfig

	// 1. Get rootfs disk path from image info
	// Parse image name to get digest path
	// The Image field is like "alpine:3.20" - we need to find the digest
	// For now, scan the images directory for this image
	imagesDir := p.ImagesDir()
	imageRootfs, err := findImageRootfs(imagesDir, metadata.Image)
	if err != nil {
		return 0, nil, fmt.Errorf("find image rootfs: %w", err)
	}
	disks = append(disks, DiskConfig{Path: imageRootfs, Readonly: true})

	// 2. Overlay disk
	overlayDisk := filepath.Join(instanceDir, "overlay.raw")
	if _, err := os.Stat(overlayDisk); err == nil {
		disks = append(disks, DiskConfig{Path: overlayDisk, Readonly: false})
	}

	// 3. Config disk
	configDisk := filepath.Join(instanceDir, "config.ext4")
	if _, err := os.Stat(configDisk); err == nil {
		disks = append(disks, DiskConfig{Path: configDisk, Readonly: true})
	}

	// Get kernel and initrd paths
	arch := system.GetArch()
	kernelPath := p.SystemKernel(metadata.KernelVersion, arch)

	// Resolve the initrd symlink to get the same path as original start
	// This is critical for vz snapshot restore which requires identical paths
	initrdLatest := p.SystemInitrdLatest(arch)
	initrdTimestamp, err := os.Readlink(initrdLatest)
	if err != nil {
		return 0, nil, fmt.Errorf("read initrd symlink: %w", err)
	}
	initrdPath := p.SystemInitrdTimestamp(initrdTimestamp, arch)

	// Build shim config
	shimConfig := ShimConfig{
		VCPUs:            metadata.VCPUs,
		MemoryBytes:      metadata.Size,
		Disks:            disks,
		Networks:         []NetworkConfig{{MAC: metadata.MAC}},
		SerialLogPath:    filepath.Join(instanceDir, "logs", "app.log"),
		KernelPath:       kernelPath,
		InitrdPath:       initrdPath,
		KernelArgs:       "console=hvc0",
		ControlSocket:    controlSocket,
		VsockSocket:      vsockSocket,
		LogPath:          logPath,
		RestoreStatePath: restoreStatePath,
	}

	configJSON, err := json.Marshal(shimConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal shim config: %w", err)
	}

	log.DebugContext(ctx, "spawning vz-shim for restore", "config", string(configJSON), "snapshot", restoreStatePath)

	// Find the vz-shim binary
	shimPath, err := s.findShimBinary()
	if err != nil {
		return 0, nil, fmt.Errorf("find vz-shim binary: %w", err)
	}

	// Spawn the shim process
	cmd := exec.CommandContext(ctx, shimPath, "-config", string(configJSON))
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("start vz-shim: %w", err)
	}

	pid := cmd.Process.Pid
	log.InfoContext(ctx, "vz-shim started for restore", "pid", pid, "control_socket", controlSocket, "snapshot", restoreStatePath)

	// Wait for the control socket to be ready
	client, err := s.waitForShim(ctx, controlSocket, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		return 0, nil, fmt.Errorf("connect to vz-shim: %w", err)
	}

	// Release the process so it's not killed when cmd goes out of scope
	cmd.Process.Release()

	return pid, client, nil
}

// findImageRootfs locates the rootfs.ext4 for an image by name.
// This scans the images directory structure to find the latest version.
func findImageRootfs(imagesDir, imageName string) (string, error) {
	// Images are stored as: {imagesDir}/{registry}/{repo}/{digest}/rootfs.ext4
	// For "alpine:3.20" -> docker.io/library/alpine/{sha256:xxx}/rootfs.ext4

	// Normalize image name to path components
	// Simple approach: walk the images directory looking for matching image
	var foundPath string
	err := filepath.Walk(imagesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if info.Name() == "rootfs.ext4" {
			// Check if parent directory structure matches image name
			// For now, just find any rootfs.ext4 that might match
			dir := filepath.Dir(path)
			// The path structure is like: .../alpine/{digest}/rootfs.ext4
			if containsImageName(dir, imageName) {
				foundPath = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if foundPath == "" {
		return "", fmt.Errorf("rootfs not found for image: %s", imageName)
	}
	return foundPath, nil
}

// containsImageName checks if a path contains the image name components.
func containsImageName(path, imageName string) bool {
	// Extract just the image name without tag (e.g., "alpine" from "alpine:3.20")
	parts := filepath.SplitList(imageName)
	name := imageName
	if idx := len(name) - 1; idx >= 0 {
		for i := len(name) - 1; i >= 0; i-- {
			if name[i] == ':' {
				name = name[:i]
				break
			}
		}
	}
	_ = parts
	return filepath.Base(filepath.Dir(filepath.Dir(path))) == name ||
		filepath.Base(filepath.Dir(path)) == name
}
