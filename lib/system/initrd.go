package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kernel/hypeman/lib/images"
)

const alpineBaseImage = "alpine:3.22"

// buildInitrd builds initrd from Alpine base + embedded guest-agent + embedded init binary
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

	// Write embedded guest-agent binary
	binDir := filepath.Join(rootfsDir, "usr/local/bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", fmt.Errorf("create bin dir: %w", err)
	}

	agentPath := filepath.Join(binDir, "guest-agent")
	if err := os.WriteFile(agentPath, GuestAgentBinary, 0755); err != nil {
		return "", fmt.Errorf("write guest-agent: %w", err)
	}

	// Write shell wrapper as /init (sets up /proc, /sys, /dev before Go runtime)
	// The Go runtime needs these filesystems during initialization
	initWrapperPath := filepath.Join(rootfsDir, "init")
	if err := os.WriteFile(initWrapperPath, InitWrapper, 0755); err != nil {
		return "", fmt.Errorf("write init wrapper: %w", err)
	}

	// Write Go init binary as /init.bin (called by wrapper after setup)
	initBinPath := filepath.Join(rootfsDir, "init.bin")
	if err := os.WriteFile(initBinPath, InitBinary, 0755); err != nil {
		return "", fmt.Errorf("write init binary: %w", err)
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
	currentHash := computeInitrdHash(arch)
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
			if !m.isInitrdStale(initrdPath, arch) {
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
func (m *manager) isInitrdStale(initrdPath, arch string) bool {
	// Read stored hash
	hashPath := filepath.Join(filepath.Dir(initrdPath), ".hash")
	storedHash, err := os.ReadFile(hashPath)
	if err != nil {
		// No hash file, consider stale
		return true
	}

	// Compare with current hash
	currentHash := computeInitrdHash(arch)
	return string(storedHash) != currentHash
}

// computeInitrdHash computes a hash of the embedded binaries
func computeInitrdHash(_ string) string {
	h := sha256.New()
	h.Write(GuestAgentBinary)
	h.Write(InitBinary)
	h.Write(InitWrapper)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
