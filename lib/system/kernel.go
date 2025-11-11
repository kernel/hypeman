package system

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// downloadKernel downloads a kernel from Cloud Hypervisor releases
func (m *manager) downloadKernel(version KernelVersion, arch string) error {
	url, ok := KernelDownloadURLs[version][arch]
	if !ok {
		return fmt.Errorf("unsupported kernel version/arch: %s/%s", version, arch)
	}

	destPath := filepath.Join(m.dataDir, "system", "kernel", string(version), arch, "vmlinux")

	// Create parent directory
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("create kernel directory: %w", err)
	}

	// Download kernel
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Create output file
	outFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer outFile.Close()

	// Copy with progress
	_, err = io.Copy(outFile, resp.Body)
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	// Make executable
	if err := os.Chmod(destPath, 0755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	return nil
}

// ensureKernel ensures kernel exists, downloads if missing
func (m *manager) ensureKernel(version KernelVersion) (string, error) {
	arch := GetArch()

	kernelPath := filepath.Join(m.dataDir, "system", "kernel", string(version), arch, "vmlinux")

	// Check if already exists
	if _, err := os.Stat(kernelPath); err == nil {
		return kernelPath, nil
	}

	// Download kernel
	if err := m.downloadKernel(version, arch); err != nil {
		return "", fmt.Errorf("download kernel: %w", err)
	}

	return kernelPath, nil
}

