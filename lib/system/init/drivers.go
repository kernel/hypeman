package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// loadGPUDrivers loads NVIDIA kernel modules for GPU passthrough.
func loadGPUDrivers(log *Logger) error {
	log.Info("gpu", "loading NVIDIA kernel modules")

	// Find kernel version directory
	modules, err := os.ReadDir("/lib/modules")
	if err != nil {
		return fmt.Errorf("read /lib/modules: %w", err)
	}

	if len(modules) == 0 {
		return fmt.Errorf("no kernel modules found")
	}

	kver := modules[0].Name()
	gpuDir := filepath.Join("/lib/modules", kver, "kernel/drivers/gpu")

	if _, err := os.Stat(gpuDir); err != nil {
		return fmt.Errorf("GPU modules not found for kernel %s", kver)
	}

	// Load modules in order (dependencies first)
	moduleOrder := []string{
		"nvidia.ko",
		"nvidia-uvm.ko",
		"nvidia-modeset.ko",
		"nvidia-drm.ko",
	}

	for _, mod := range moduleOrder {
		modPath := filepath.Join(gpuDir, mod)
		if _, err := os.Stat(modPath); err != nil {
			log.Error("gpu", fmt.Sprintf("%s not found", mod), nil)
			continue
		}

		args := []string{modPath}
		// nvidia-drm needs modeset=1
		if mod == "nvidia-drm.ko" {
			args = append(args, "modeset=1")
		}

		cmd := exec.Command("/sbin/insmod", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Error("gpu", fmt.Sprintf("insmod %s failed", mod), fmt.Errorf("%s", output))
		}
	}

	log.Info("gpu", fmt.Sprintf("loaded NVIDIA modules for kernel %s", kver))

	// Create device nodes using nvidia-modprobe if available
	if err := createNvidiaDevices(log); err != nil {
		log.Error("gpu", "failed to create device nodes", err)
	}

	// Inject NVIDIA userspace driver libraries into container rootfs
	if err := injectNvidiaLibraries(log); err != nil {
		log.Error("gpu", "failed to inject driver libraries", err)
	}

	return nil
}

// createNvidiaDevices creates NVIDIA device nodes.
func createNvidiaDevices(log *Logger) error {
	// Try nvidia-modprobe first (the official NVIDIA utility)
	if _, err := os.Stat("/usr/bin/nvidia-modprobe"); err == nil {
		log.Info("gpu", "running nvidia-modprobe to create device nodes")

		cmd := exec.Command("/usr/bin/nvidia-modprobe")
		cmd.CombinedOutput()

		cmd = exec.Command("/usr/bin/nvidia-modprobe", "-u", "-c=0")
		cmd.CombinedOutput()

		return nil
	}

	// Fallback: Manual device node creation
	log.Info("gpu", "nvidia-modprobe not found, creating device nodes manually")

	// Read major numbers from /proc/devices
	data, err := os.ReadFile("/proc/devices")
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	var nvidiaMajor, uvmMajor string

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if fields[1] == "nvidia-frontend" || fields[1] == "nvidia" {
				nvidiaMajor = fields[0]
			} else if fields[1] == "nvidia-uvm" {
				uvmMajor = fields[0]
			}
		}
	}

	if nvidiaMajor != "" {
		exec.Command("/bin/mknod", "-m", "666", "/dev/nvidiactl", "c", nvidiaMajor, "255").Run()
		exec.Command("/bin/mknod", "-m", "666", "/dev/nvidia0", "c", nvidiaMajor, "0").Run()
		log.Info("gpu", fmt.Sprintf("created /dev/nvidiactl and /dev/nvidia0 (major %s)", nvidiaMajor))
	}

	if uvmMajor != "" {
		exec.Command("/bin/mknod", "-m", "666", "/dev/nvidia-uvm", "c", uvmMajor, "0").Run()
		exec.Command("/bin/mknod", "-m", "666", "/dev/nvidia-uvm-tools", "c", uvmMajor, "1").Run()
		log.Info("gpu", fmt.Sprintf("created /dev/nvidia-uvm* (major %s)", uvmMajor))
	}

	return nil
}

// injectNvidiaLibraries injects NVIDIA userspace driver libraries into the container rootfs.
// This allows containers to use standard CUDA images without bundled drivers.
func injectNvidiaLibraries(log *Logger) error {
	srcDir := "/usr/lib/nvidia"
	if _, err := os.Stat(srcDir); err != nil {
		return nil // No driver libraries to inject
	}

	log.Info("gpu", "injecting NVIDIA driver libraries into container")

	// Determine library path based on architecture
	var libDst string
	if runtime.GOARCH == "arm64" {
		libDst = "/overlay/newroot/usr/lib/aarch64-linux-gnu"
	} else {
		libDst = "/overlay/newroot/usr/lib/x86_64-linux-gnu"
	}
	binDst := "/overlay/newroot/usr/bin"

	if err := os.MkdirAll(libDst, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(binDst, 0755); err != nil {
		return err
	}

	// Copy all driver libraries
	libs, _ := filepath.Glob(filepath.Join(srcDir, "*.so.*"))
	for _, lib := range libs {
		libname := filepath.Base(lib)
		data, err := os.ReadFile(lib)
		if err != nil {
			continue
		}
		os.WriteFile(filepath.Join(libDst, libname), data, 0755)

		// Create standard symlinks
		base := strings.Split(libname, ".so.")[0]
		os.Symlink(libname, filepath.Join(libDst, base+".so.1"))
		os.Symlink(base+".so.1", filepath.Join(libDst, base+".so"))
	}

	// Copy nvidia-smi and nvidia-modprobe binaries
	for _, bin := range []string{"nvidia-smi", "nvidia-modprobe"} {
		srcPath := filepath.Join("/usr/bin", bin)
		if data, err := os.ReadFile(srcPath); err == nil {
			os.WriteFile(filepath.Join(binDst, bin), data, 0755)
		}
	}

	// Update ldconfig cache
	exec.Command("/usr/sbin/chroot", "/overlay/newroot", "ldconfig").Run()

	// Read driver version
	version := "unknown"
	if data, err := os.ReadFile(filepath.Join(srcDir, "version")); err == nil {
		version = strings.TrimSpace(string(data))
	}

	log.Info("gpu", fmt.Sprintf("injected NVIDIA driver libraries (version: %s)", version))
	return nil
}

