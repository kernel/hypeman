package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	// Paths in the overlay filesystem
	newrootLibModules = "/overlay/newroot/lib/modules"
	newrootUsrSrc     = "/overlay/newroot/usr/src"
	headersTarball    = "/kernel-headers.tar.gz"
)

// setupKernelHeaders installs kernel headers and cleans up mismatched headers from the guest image.
// This enables DKMS to build out-of-tree kernel modules (e.g., NVIDIA vGPU drivers).
func setupKernelHeaders(log *Logger) error {
	// Get running kernel version
	var uname syscall.Utsname
	if err := syscall.Uname(&uname); err != nil {
		return fmt.Errorf("uname: %w", err)
	}
	runningKernel := int8ArrayToString(uname.Release[:])
	log.Info("headers", "running kernel: "+runningKernel)

	// Check if headers tarball exists in initrd
	if _, err := os.Stat(headersTarball); os.IsNotExist(err) {
		log.Info("headers", "no kernel headers tarball found, skipping")
		return nil
	}

	// Clean up mismatched kernel modules directories
	if err := cleanupMismatchedModules(log, runningKernel); err != nil {
		log.Info("headers", "warning: failed to cleanup mismatched modules: "+err.Error())
		// Non-fatal, continue
	}

	// Clean up mismatched kernel headers directories
	if err := cleanupMismatchedHeaders(log, runningKernel); err != nil {
		log.Info("headers", "warning: failed to cleanup mismatched headers: "+err.Error())
		// Non-fatal, continue
	}

	// Create target directories
	headersDir := filepath.Join(newrootUsrSrc, "linux-headers-"+runningKernel)
	modulesDir := filepath.Join(newrootLibModules, runningKernel)

	if err := os.MkdirAll(headersDir, 0755); err != nil {
		return fmt.Errorf("mkdir headers dir: %w", err)
	}
	if err := os.MkdirAll(modulesDir, 0755); err != nil {
		return fmt.Errorf("mkdir modules dir: %w", err)
	}

	// Extract headers tarball
	if err := extractTarGz(headersTarball, headersDir); err != nil {
		return fmt.Errorf("extract headers: %w", err)
	}
	log.Info("headers", "extracted kernel headers to "+headersDir)

	// Create build symlink
	buildLink := filepath.Join(modulesDir, "build")
	os.Remove(buildLink) // Remove if exists
	// Use absolute path for symlink target (will be correct after chroot)
	symlinkTarget := "/usr/src/linux-headers-" + runningKernel
	if err := os.Symlink(symlinkTarget, buildLink); err != nil {
		return fmt.Errorf("create build symlink: %w", err)
	}
	log.Info("headers", "created build symlink")

	return nil
}

// cleanupMismatchedModules removes /lib/modules/* directories that don't match the running kernel
func cleanupMismatchedModules(log *Logger, runningKernel string) error {
	entries, err := os.ReadDir(newrootLibModules)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No modules directory, nothing to clean
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if entry.Name() != runningKernel {
			path := filepath.Join(newrootLibModules, entry.Name())
			log.Info("headers", "removing mismatched modules: "+entry.Name())
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
		}
	}

	return nil
}

// cleanupMismatchedHeaders removes /usr/src/linux-headers-* directories that don't match the running kernel
func cleanupMismatchedHeaders(log *Logger, runningKernel string) error {
	entries, err := os.ReadDir(newrootUsrSrc)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No usr/src directory, nothing to clean
		}
		return err
	}

	expectedName := "linux-headers-" + runningKernel

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Remove any linux-headers-* directory that doesn't match
		if strings.HasPrefix(entry.Name(), "linux-headers-") && entry.Name() != expectedName {
			path := filepath.Join(newrootUsrSrc, entry.Name())
			log.Info("headers", "removing mismatched headers: "+entry.Name())
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
		}
	}

	return nil
}

// extractTarGz extracts a .tar.gz file to a destination directory
func extractTarGz(tarball, destDir string) error {
	// Use tar command since it's available in Alpine
	cmd := exec.Command("/bin/tar", "-xzf", tarball, "-C", destDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %s: %s", err, output)
	}
	return nil
}

// int8ArrayToString converts a null-terminated int8 array (from syscall) to a Go string
func int8ArrayToString(arr []int8) string {
	var buf []byte
	for _, b := range arr {
		if b == 0 {
			break
		}
		buf = append(buf, byte(b))
	}
	return string(buf)
}
