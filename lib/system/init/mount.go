package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// mountEssentials mounts additional filesystems needed for boot.
// Note: /proc, /sys, /dev are already mounted by the init.sh wrapper script
// before the Go binary runs (the Go runtime needs them during initialization).
// This function mounts:
// - /dev/pts (pseudo-terminals)
// - /dev/shm (shared memory)
// - /sys/fs/cgroup (cgroup2 for container runtimes like runc)
func mountEssentials(log *Logger) error {
	// Create mount points for pts and shm (proc/sys/dev already exist from wrapper)
	for _, dir := range []string{"/dev/pts", "/dev/shm"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	// Mount devpts for PTY support (needed for guest-agent and interactive shells)
	if err := syscall.Mount("devpts", "/dev/pts", "devpts", 0, ""); err != nil {
		return fmt.Errorf("mount /dev/pts: %w", err)
	}

	// Set permissions on /dev/shm
	if err := os.Chmod("/dev/shm", 01777); err != nil {
		return fmt.Errorf("chmod /dev/shm: %w", err)
	}

	// Mount cgroup2 for container runtimes (runc/BuildKit require cgroups)
	// This is safe because VMs are already isolated by the hypervisor, and
	// cgroup v2 has better security than v1 (no release_agent escape vector)
	if err := os.MkdirAll("/sys/fs/cgroup", 0755); err != nil {
		return fmt.Errorf("mkdir /sys/fs/cgroup: %w", err)
	}
	if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
		// Non-fatal: some kernels may not have cgroup2 support
		log.Info("mount", "cgroup2 mount failed (non-fatal): "+err.Error())
	} else {
		log.Info("mount", "mounted cgroup2")
	}

	log.Info("mount", "mounted devpts/shm")

	// Set up serial console now that /dev is mounted
	// hvc0 for Virtualization.framework (vz) on macOS
	// ttyAMA0 for ARM64 PL011 UART (cloud-hypervisor)
	// ttyS0 for x86_64 (QEMU, cloud-hypervisor)
	consoles := []string{"/dev/hvc0", "/dev/ttyAMA0", "/dev/ttyS0"}
	for _, console := range consoles {
		if _, err := os.Stat(console); err == nil {
			log.SetConsole(console)
			redirectToConsole(console)
			log.Info("mount", "using console "+console)
			break
		}
	}

	log.Info("mount", "console setup complete")

	return nil
}

// waitForDevice polls for a block device to become available.
// It polls every 10ms until the device exists or the timeout is reached.
func waitForDevice(device string, timeout time.Duration) error {
	const pollInterval = 10 * time.Millisecond
	deadline := time.Now().Add(timeout)

	for {
		if _, err := os.Stat(device); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for device %s", device)
		}
		time.Sleep(pollInterval)
	}
}

// setupOverlay sets up the overlay filesystem:
// - /dev/vda: readonly rootfs (ext4)
// - /dev/vdb: writable overlay disk (ext4)
// - /overlay/newroot: merged overlay filesystem
func setupOverlay(log *Logger) error {
	// Wait for block devices to be ready (polls every 10ms, 2s timeout)
	// This typically completes in 10-50ms instead of always waiting 500ms
	if err := waitForDevice("/dev/vda", 2*time.Second); err != nil {
		return fmt.Errorf("wait for rootfs device: %w", err)
	}
	if err := waitForDevice("/dev/vdb", 2*time.Second); err != nil {
		return fmt.Errorf("wait for overlay device: %w", err)
	}

	// Create mount points
	for _, dir := range []string{"/lower", "/overlay"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	// Mount readonly rootfs from /dev/vda (ext4 filesystem)
	if err := mount("/dev/vda", "/lower", "ext4", "ro"); err != nil {
		return fmt.Errorf("mount rootfs: %w", err)
	}
	log.Info("overlay", "mounted rootfs from /dev/vda")

	// Mount writable overlay disk from /dev/vdb
	if err := mount("/dev/vdb", "/overlay", "ext4", ""); err != nil {
		return fmt.Errorf("mount overlay disk: %w", err)
	}

	// Create overlay directories
	for _, dir := range []string{"/overlay/upper", "/overlay/work", "/overlay/newroot"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	log.Info("overlay", "mounted overlay disk from /dev/vdb")

	// Create overlay filesystem
	if err := mountOverlay("/lower", "/overlay/upper", "/overlay/work", "/overlay/newroot"); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	log.Info("overlay", "created overlay filesystem")

	return nil
}

// bindMountsToNewRoot bind-mounts essential filesystems to the new root.
// Uses bind mounts instead of move so that the original /dev remains populated
// for processes running in the initrd namespace.
func bindMountsToNewRoot(log *Logger) error {
	newroot := "/overlay/newroot"

	// Create mount points in new root
	for _, dir := range []string{"proc", "sys", "sys/fs/cgroup", "dev", "dev/pts"} {
		if err := os.MkdirAll(newroot+"/"+dir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	// Bind mount filesystems
	mounts := []struct{ src, dst string }{
		{"/proc", newroot + "/proc"},
		{"/sys", newroot + "/sys"},
		{"/sys/fs/cgroup", newroot + "/sys/fs/cgroup"},
		{"/dev", newroot + "/dev"},
		{"/dev/pts", newroot + "/dev/pts"},
	}

	for _, m := range mounts {
		if err := bindMount(m.src, m.dst); err != nil {
			return fmt.Errorf("bind mount %s: %w", m.src, err)
		}
	}

	log.Info("bind", "bound mounts to new root")

	// Set up /dev symlinks for process substitution inside the container
	symlinks := []struct{ target, link string }{
		{"/proc/self/fd", newroot + "/dev/fd"},
		{"/proc/self/fd/0", newroot + "/dev/stdin"},
		{"/proc/self/fd/1", newroot + "/dev/stdout"},
		{"/proc/self/fd/2", newroot + "/dev/stderr"},
	}

	for _, s := range symlinks {
		os.Remove(s.link) // Remove if exists
		os.Symlink(s.target, s.link)
	}

	return nil
}

// mount executes a mount command
func mount(source, target, fstype, options string) error {
	args := []string{"-t", fstype}
	if options != "" {
		args = append(args, "-o", options)
	}
	args = append(args, source, target)

	cmd := exec.Command("/bin/mount", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", err, output)
	}
	return nil
}

// mountOverlay creates an overlay filesystem
func mountOverlay(lower, upper, work, target string) error {
	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	cmd := exec.Command("/bin/mount", "-t", "overlay", "-o", options, "overlay", target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", err, output)
	}
	return nil
}

// bindMount performs a bind mount
func bindMount(source, target string) error {
	cmd := exec.Command("/bin/mount", "--bind", source, target)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s", err, output)
	}
	return nil
}

// redirectToConsole redirects stdout/stderr to the serial console
func redirectToConsole(device string) {
	f, err := os.OpenFile(device, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	os.Stdout = f
	os.Stderr = f
}

// copyGuestAgent copies the guest-agent binary to the target location in the new root.
// It skips copying if:
// - skipGuestAgent config option is true
// - The destination file already exists (lazy copy optimization)
func copyGuestAgent(log *Logger, skipGuestAgent bool) error {
	const (
		src = "/usr/local/bin/guest-agent"
		dst = "/overlay/newroot/opt/hypeman/guest-agent"
	)

	// Check for skip via config
	if skipGuestAgent {
		log.Info("agent", "skipping guest-agent copy (skip_guest_agent=true)")
		return nil
	}

	// Check if destination already exists (lazy copy - skip if already present)
	if _, err := os.Stat(dst); err == nil {
		log.Info("agent", "guest-agent already exists, skipping copy")
		return nil
	}

	// Create target directory
	if err := os.MkdirAll("/overlay/newroot/opt/hypeman", 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	// Read source binary
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}

	// Write to destination
	if err := os.WriteFile(dst, data, 0755); err != nil {
		return fmt.Errorf("write destination: %w", err)
	}

	log.Info("agent", "copied guest-agent to /opt/hypeman/")
	return nil
}

