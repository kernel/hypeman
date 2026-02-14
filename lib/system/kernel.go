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

	destPath := m.paths.SystemKernel(string(version), arch)

	// Create parent directory
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("create kernel directory: %w", err)
	}

	// Download kernel (GitHub releases return 302 redirects)
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // Follow redirects
		},
	}
	
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed with status %d from %s", resp.StatusCode, url)
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

	kernelPath := m.paths.SystemKernel(string(version), arch)

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

