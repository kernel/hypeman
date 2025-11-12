package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/onkernel/hypeman/lib/images"
)

// buildInitrd builds initrd from busybox + custom init script
func (m *manager) buildInitrd(ctx context.Context, version InitrdVersion, arch string) error {
	// Create temp directory for building
	tempDir, err := os.MkdirTemp("", "hypeman-initrd-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	rootfsDir := filepath.Join(tempDir, "rootfs")

	// Get pinned busybox version for this initrd version (ensures reproducible builds)
	busyboxRef, ok := InitrdBusyboxVersions[version]
	if !ok {
		return fmt.Errorf("no busybox version defined for initrd %s", version)
	}

	// Create a temporary OCI client (reuses image manager's cache)
	cacheDir := filepath.Join(m.dataDir, "system", "oci-cache")
	ociClient, err := images.NewOCIClient(cacheDir)
	if err != nil {
		return fmt.Errorf("create oci client: %w", err)
	}

	// Inspect to get digest
	digest, err := ociClient.InspectManifest(ctx, busyboxRef)
	if err != nil {
		return fmt.Errorf("inspect busybox manifest: %w", err)
	}

	// Pull and unpack busybox
	if err := ociClient.PullAndUnpack(ctx, busyboxRef, digest, rootfsDir); err != nil {
		return fmt.Errorf("pull busybox: %w", err)
	}

	// Inject init script
	initScript := GenerateInitScript(version)
	initPath := filepath.Join(rootfsDir, "init")
	if err := os.WriteFile(initPath, []byte(initScript), 0755); err != nil {
		return fmt.Errorf("write init script: %w", err)
	}

	// Package as cpio.gz (initramfs format)
	outputPath := filepath.Join(m.dataDir, "system", "initrd", string(version), arch, "initrd")
	if _, err := images.ExportRootfs(rootfsDir, outputPath, images.FormatCpio); err != nil {
		return fmt.Errorf("export initrd: %w", err)
	}

	return nil
}

// ensureInitrd ensures initrd exists, builds if missing
func (m *manager) ensureInitrd(ctx context.Context, version InitrdVersion) (string, error) {
	arch := GetArch()

	initrdPath := filepath.Join(m.dataDir, "system", "initrd", string(version), arch, "initrd")

	// Check if already exists
	if _, err := os.Stat(initrdPath); err == nil {
		return initrdPath, nil
	}

	// Build initrd
	if err := m.buildInitrd(ctx, version, arch); err != nil {
		return "", fmt.Errorf("build initrd: %w", err)
	}

	return initrdPath, nil
}

