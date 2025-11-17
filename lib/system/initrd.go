package system

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/onkernel/hypeman/lib/images"
)

// buildInitrd builds initrd from base image + custom init script
func (m *manager) buildInitrd(ctx context.Context, version InitrdVersion, arch string) error {
	// Create temp directory for building
	tempDir, err := os.MkdirTemp("", "hypeman-initrd-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	rootfsDir := filepath.Join(tempDir, "rootfs")

	// Get base image for this initrd version
	baseImageRef, ok := InitrdBaseImages[version]
	if !ok {
		return fmt.Errorf("no base image defined for initrd %s", version)
	}

	// Create a temporary OCI client (reuses image manager's cache)
	cacheDir := m.paths.SystemOCICache()
	ociClient, err := images.NewOCIClient(cacheDir)
	if err != nil {
		return fmt.Errorf("create oci client: %w", err)
	}

	// Inspect to get digest
	digest, err := ociClient.InspectManifest(ctx, baseImageRef)
	if err != nil {
		return fmt.Errorf("inspect base image manifest: %w", err)
	}

	// Pull and unpack base image
	if err := ociClient.PullAndUnpack(ctx, baseImageRef, digest, rootfsDir); err != nil {
		return fmt.Errorf("pull base image: %w", err)
	}

	// Inject init script
	initScript := GenerateInitScript(version)
	initPath := filepath.Join(rootfsDir, "init")
	if err := os.WriteFile(initPath, []byte(initScript), 0755); err != nil {
		return fmt.Errorf("write init script: %w", err)
	}

	// Package as cpio.gz (initramfs format)
	outputPath := m.paths.SystemInitrd(string(version), arch)
	if _, err := images.ExportRootfs(rootfsDir, outputPath, images.FormatCpio); err != nil {
		return fmt.Errorf("export initrd: %w", err)
	}

	return nil
}

// ensureInitrd ensures initrd exists, builds if missing
func (m *manager) ensureInitrd(ctx context.Context, version InitrdVersion) (string, error) {
	arch := GetArch()

	initrdPath := m.paths.SystemInitrd(string(version), arch)

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

