package system

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/onkernel/hypeman/lib/images"
)

const alpineBaseImage = "alpine:3.22"

// buildInitrd builds initrd from Alpine base + embedded exec-agent + generated init script
func (m *manager) buildInitrd(ctx context.Context, arch string) (string, error) {
	// Create temp directory for building
	tempDir, err := os.MkdirTemp("", "hypeman-initrd-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	rootfsDir := filepath.Join(tempDir, "rootfs")

	// Create OCI client (reuses image manager's cache)
	cacheDir := m.paths.SystemOCICache()
	ociClient, err := images.NewOCIClient(cacheDir)
	if err != nil {
		return "", fmt.Errorf("create oci client: %w", err)
	}

	// Inspect Alpine base to get digest
	digest, err := ociClient.InspectManifest(ctx, alpineBaseImage)
	if err != nil {
		return "", fmt.Errorf("inspect alpine manifest: %w", err)
	}

	// Pull and unpack Alpine base
	if err := ociClient.PullAndUnpack(ctx, alpineBaseImage, digest, rootfsDir); err != nil {
		return "", fmt.Errorf("pull alpine base: %w", err)
	}

	// Write embedded exec-agent binary
	binDir := filepath.Join(rootfsDir, "usr/local/bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", fmt.Errorf("create bin dir: %w", err)
	}

	agentPath := filepath.Join(binDir, "exec-agent")
	if err := os.WriteFile(agentPath, ExecAgentBinary, 0755); err != nil {
		return "", fmt.Errorf("write exec-agent: %w", err)
	}

	// Add NVIDIA kernel modules (for GPU passthrough support)
	if err := m.addNvidiaModules(ctx, rootfsDir, arch); err != nil {
		// Log but don't fail - NVIDIA modules are optional (not available on all architectures)
		fmt.Printf("initrd: skipping NVIDIA modules: %v\n", err)
	}

	// Write generated init script
	// Note: The init script is generated at instance creation time with hasGPU flag,
	// so we write a placeholder here that will be replaced per-instance
	initScript := GenerateInitScript()
	initPath := filepath.Join(rootfsDir, "init")
	if err := os.WriteFile(initPath, []byte(initScript), 0755); err != nil {
		return "", fmt.Errorf("write init script: %w", err)
	}

	// Generate timestamp for this build
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	// Package as cpio.gz
	outputPath := m.paths.SystemInitrdTimestamp(timestamp, arch)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}

	if _, err := images.ExportRootfs(rootfsDir, outputPath, images.FormatCpio); err != nil {
		return "", fmt.Errorf("export initrd: %w", err)
	}

	// Store hash for staleness detection
	hashPath := filepath.Join(filepath.Dir(outputPath), ".hash")
	currentHash := computeInitrdHash()
	if err := os.WriteFile(hashPath, []byte(currentHash), 0644); err != nil {
		return "", fmt.Errorf("write hash file: %w", err)
	}

	// Update 'latest' symlink
	latestLink := m.paths.SystemInitrdLatest(arch)
	// Remove old symlink if it exists
	os.Remove(latestLink)
	// Create new symlink (relative path)
	if err := os.Symlink(timestamp, latestLink); err != nil {
		return "", fmt.Errorf("create latest symlink: %w", err)
	}

	return outputPath, nil
}

// ensureInitrd ensures initrd exists and is up-to-date, builds if missing or stale
func (m *manager) ensureInitrd(ctx context.Context) (string, error) {
	arch := GetArch()
	latestLink := m.paths.SystemInitrdLatest(arch)

	// Check if latest symlink exists
	if target, err := os.Readlink(latestLink); err == nil {
		// Symlink exists, check if the actual file exists
		initrdPath := m.paths.SystemInitrdTimestamp(target, arch)
		if _, err := os.Stat(initrdPath); err == nil {
			// File exists, check if it's stale by comparing embedded binary hash
			if !m.isInitrdStale(initrdPath) {
				return initrdPath, nil
			}
		}
	}

	// Build new initrd
	initrdPath, err := m.buildInitrd(ctx, arch)
	if err != nil {
		return "", fmt.Errorf("build initrd: %w", err)
	}

	return initrdPath, nil
}

// isInitrdStale checks if the initrd needs rebuilding by comparing hashes
func (m *manager) isInitrdStale(initrdPath string) bool {
	// Read stored hash
	hashPath := filepath.Join(filepath.Dir(initrdPath), ".hash")
	storedHash, err := os.ReadFile(hashPath)
	if err != nil {
		// No hash file, consider stale
		return true
	}

	// Compare with current hash
	currentHash := computeInitrdHash()
	return string(storedHash) != currentHash
}

// computeInitrdHash computes a hash of the embedded binary, init script, and NVIDIA assets
func computeInitrdHash() string {
	h := sha256.New()
	h.Write(ExecAgentBinary)
	h.Write([]byte(GenerateInitScript()))
	// Include NVIDIA driver version in hash so initrd is rebuilt when driver changes
	if ver, ok := NvidiaDriverVersion[DefaultKernelVersion]; ok {
		h.Write([]byte(ver))
	}
	// Include driver libs URL so initrd is rebuilt when the libs tarball changes
	if archURLs, ok := NvidiaDriverLibURLs[DefaultKernelVersion]; ok {
		if url, ok := archURLs["x86_64"]; ok {
			h.Write([]byte(url))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// addNvidiaModules downloads and extracts NVIDIA kernel modules into the rootfs
func (m *manager) addNvidiaModules(ctx context.Context, rootfsDir, arch string) error {
	// Check if NVIDIA modules are available for this architecture
	archURLs, ok := NvidiaModuleURLs[DefaultKernelVersion]
	if !ok {
		return fmt.Errorf("no NVIDIA modules for kernel version %s", DefaultKernelVersion)
	}
	url, ok := archURLs[arch]
	if !ok {
		return fmt.Errorf("no NVIDIA modules for architecture %s", arch)
	}

	// Download the tarball
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // Follow redirects
		},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download nvidia modules: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Extract tarball directly into rootfs
	if err := extractTarGz(resp.Body, rootfsDir); err != nil {
		return fmt.Errorf("extract nvidia modules: %w", err)
	}

	// Add userspace driver libraries (libcuda.so, libnvidia-ml.so, nvidia-smi, etc.)
	// These are injected into containers at boot time - see lib/devices/GPU.md
	if err := m.addNvidiaDriverLibs(ctx, rootfsDir, arch); err != nil {
		fmt.Printf("initrd: warning: could not add nvidia driver libs: %v\n", err)
		// Don't fail - kernel modules can still work, but containers won't have driver libs
	}

	return nil
}

// addNvidiaDriverLibs downloads and extracts NVIDIA userspace driver libraries
// These libraries (libcuda.so, libnvidia-ml.so, nvidia-smi, etc.) are injected
// into containers at boot time, eliminating the need for containers to bundle
// matching driver versions. See lib/devices/GPU.md for documentation.
func (m *manager) addNvidiaDriverLibs(ctx context.Context, rootfsDir, arch string) error {
	archURLs, ok := NvidiaDriverLibURLs[DefaultKernelVersion]
	if !ok {
		return fmt.Errorf("no NVIDIA driver libs for kernel version %s", DefaultKernelVersion)
	}
	url, ok := archURLs[arch]
	if !ok {
		return fmt.Errorf("no NVIDIA driver libs for architecture %s", arch)
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // Follow redirects
		},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download nvidia driver libs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// Extract tarball directly into rootfs
	if err := extractTarGz(resp.Body, rootfsDir); err != nil {
		return fmt.Errorf("extract nvidia driver libs: %w", err)
	}

	fmt.Printf("initrd: added NVIDIA driver libraries from %s\n", url)
	return nil
}

// extractTarGz extracts a gzipped tarball into the destination directory
func extractTarGz(r io.Reader, destDir string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		// Calculate destination path
		destPath := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("create directory %s: %w", destPath, err)
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return fmt.Errorf("create parent dir: %w", err)
			}

			outFile, err := os.Create(destPath)
			if err != nil {
				return fmt.Errorf("create file %s: %w", destPath, err)
			}

			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return fmt.Errorf("write file %s: %w", destPath, err)
			}
			outFile.Close()

			if err := os.Chmod(destPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("chmod %s: %w", destPath, err)
			}
		}
	}

	return nil
}
