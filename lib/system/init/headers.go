package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	headersWorkerArg      = "--headers-worker"
	headersWorkerGuestArg = "--headers-worker-guest"

	headersStatusPending = "pending"
	headersStatusRunning = "running"
	headersStatusReady   = "ready"
	headersStatusFailed  = "failed"
)

type kernelHeadersPaths struct {
	libModulesDir string
	usrSrcDir     string
	tarballPath   string
	statusPath    string
}

var (
	initrdKernelHeadersPaths = kernelHeadersPaths{
		libModulesDir: "/overlay/newroot/lib/modules",
		usrSrcDir:     "/overlay/newroot/usr/src",
		tarballPath:   "/kernel-headers.tar.gz",
		statusPath:    "/overlay/newroot/run/hypeman/kernel-headers.status",
	}
	guestKernelHeadersPaths = kernelHeadersPaths{
		libModulesDir: "/lib/modules",
		usrSrcDir:     "/usr/src",
		tarballPath:   "/opt/hypeman/kernel-headers.tar.gz",
		statusPath:    "/run/hypeman/kernel-headers.status",
	}
)

func startKernelHeadersWorkerAsync(log *Logger) {
	if err := writeKernelHeadersStatus(initrdKernelHeadersPaths.statusPath, headersStatusPending); err != nil {
		log.Info("hypeman-init:headers", "warning: failed to write status file: "+err.Error())
	}

	cmd := exec.Command("/proc/self/exe", headersWorkerArg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Error("hypeman-init:headers", "failed to start async headers worker", err)
		_ = writeKernelHeadersStatus(initrdKernelHeadersPaths.statusPath, headersStatusFailed)
		log.Info("hypeman-init:headers", formatHeadersFailedSentinel(err))
		return
	}

	log.Info("hypeman-init:headers", fmt.Sprintf("started async headers worker (pid %d)", cmd.Process.Pid))
}

func runKernelHeadersWorker(log *Logger, paths kernelHeadersPaths) {
	log.Info("hypeman-init:headers", formatHeadersSentinel("START"))
	if err := writeKernelHeadersStatus(paths.statusPath, headersStatusRunning); err != nil {
		log.Info("hypeman-init:headers", "warning: failed to write status file: "+err.Error())
	}
	if err := lowerKernelHeadersWorkerPriority(log); err != nil {
		log.Info("hypeman-init:headers", "warning: failed to lower worker priority: "+err.Error())
	}

	if err := setupKernelHeaders(log, paths); err != nil {
		log.Error("hypeman-init:headers", "kernel headers setup failed", err)
		_ = writeKernelHeadersStatus(paths.statusPath, headersStatusFailed)
		log.Info("hypeman-init:headers", formatHeadersFailedSentinel(err))
		os.Exit(1)
	}

	if err := writeKernelHeadersStatus(paths.statusPath, headersStatusReady); err != nil {
		log.Info("hypeman-init:headers", "warning: failed to write status file: "+err.Error())
	}
	log.Info("hypeman-init:headers", formatHeadersSentinel("READY"))
	os.Exit(0)
}

func lowerKernelHeadersWorkerPriority(log *Logger) error {
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10); err != nil {
		return err
	}

	ionicePath, err := exec.LookPath("ionice")
	if err != nil {
		return nil
	}

	cmd := exec.Command(ionicePath, "-c", "3", "-p", strconv.Itoa(os.Getpid()))
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Info("hypeman-init:headers", fmt.Sprintf("warning: ionice best-effort failed: %v: %s", err, strings.TrimSpace(string(output))))
	}
	return nil
}

func writeKernelHeadersStatus(statusPath, status string) error {
	if err := os.MkdirAll(filepath.Dir(statusPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(statusPath, []byte(status+"\n"), 0644)
}

func formatHeadersSentinel(state string) string {
	return fmt.Sprintf("HYPEMAN-HEADERS-%s ts=%s", state, time.Now().UTC().Format(time.RFC3339Nano))
}

func formatHeadersFailedSentinel(err error) string {
	return fmt.Sprintf("HYPEMAN-HEADERS-FAILED ts=%s error=%q", time.Now().UTC().Format(time.RFC3339Nano), err.Error())
}

// setupKernelHeaders installs kernel headers and cleans up mismatched headers from the guest image.
// This enables DKMS to build out-of-tree kernel modules (e.g., NVIDIA vGPU drivers).
func setupKernelHeaders(log *Logger, paths kernelHeadersPaths) error {
	// Get running kernel version
	var uname syscall.Utsname
	if err := syscall.Uname(&uname); err != nil {
		return fmt.Errorf("uname: %w", err)
	}
	runningKernel := int8ArrayToString(uname.Release[:])
	log.Info("hypeman-init:headers", "running kernel: "+runningKernel)

	ready, err := kernelHeadersAlreadyInstalled(runningKernel, paths)
	if err != nil {
		return fmt.Errorf("check fast path: %w", err)
	}
	if ready {
		log.Info("hypeman-init:headers", "kernel headers already installed, skipping extraction")
		return nil
	}

	// Check if headers tarball exists in initrd
	if _, err := os.Stat(paths.tarballPath); os.IsNotExist(err) {
		return fmt.Errorf("kernel headers tarball not found at %s", paths.tarballPath)
	} else if err != nil {
		return fmt.Errorf("stat tarball: %w", err)
	}

	// Clean up mismatched kernel modules directories
	if err := cleanupMismatchedModules(log, runningKernel, paths.libModulesDir); err != nil {
		log.Info("hypeman-init:headers", "warning: failed to cleanup mismatched modules: "+err.Error())
		// Non-fatal, continue
	}

	// Clean up mismatched kernel headers directories
	if err := cleanupMismatchedHeaders(log, runningKernel, paths.usrSrcDir); err != nil {
		log.Info("hypeman-init:headers", "warning: failed to cleanup mismatched headers: "+err.Error())
		// Non-fatal, continue
	}

	// Create target directories
	headersDir := filepath.Join(paths.usrSrcDir, "linux-headers-"+runningKernel)
	modulesDir := filepath.Join(paths.libModulesDir, runningKernel)

	if err := os.MkdirAll(headersDir, 0755); err != nil {
		return fmt.Errorf("mkdir headers dir: %w", err)
	}
	if err := os.MkdirAll(modulesDir, 0755); err != nil {
		return fmt.Errorf("mkdir modules dir: %w", err)
	}

	// Extract headers tarball
	if err := extractTarGz(paths.tarballPath, headersDir); err != nil {
		return fmt.Errorf("extract headers: %w", err)
	}
	log.Info("hypeman-init:headers", "extracted kernel headers to "+headersDir)

	// Create build symlink
	buildLink := filepath.Join(modulesDir, "build")
	os.Remove(buildLink) // Remove if exists
	// Use absolute path for symlink target (will be correct after chroot)
	symlinkTarget := "/usr/src/linux-headers-" + runningKernel
	if err := os.Symlink(symlinkTarget, buildLink); err != nil {
		return fmt.Errorf("create build symlink: %w", err)
	}
	log.Info("hypeman-init:headers", "created build symlink")

	return nil
}

func kernelHeadersAlreadyInstalled(runningKernel string, paths kernelHeadersPaths) (bool, error) {
	headersDir := filepath.Join(paths.usrSrcDir, "linux-headers-"+runningKernel)
	headersInfo, err := os.Stat(headersDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !headersInfo.IsDir() {
		return false, nil
	}

	buildLink := filepath.Join(paths.libModulesDir, runningKernel, "build")
	target, err := os.Readlink(buildLink)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return target == "/usr/src/linux-headers-"+runningKernel, nil
}

// cleanupMismatchedModules removes /lib/modules/* directories that don't match the running kernel
func cleanupMismatchedModules(log *Logger, runningKernel, modulesDir string) error {
	entries, err := os.ReadDir(modulesDir)
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
			path := filepath.Join(modulesDir, entry.Name())
			log.Info("hypeman-init:headers", "removing mismatched modules: "+entry.Name())
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
		}
	}

	return nil
}

// cleanupMismatchedHeaders removes /usr/src/linux-headers-* directories that don't match the running kernel
func cleanupMismatchedHeaders(log *Logger, runningKernel, usrSrcDir string) error {
	entries, err := os.ReadDir(usrSrcDir)
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
			path := filepath.Join(usrSrcDir, entry.Name())
			log.Info("hypeman-init:headers", "removing mismatched headers: "+entry.Name())
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
