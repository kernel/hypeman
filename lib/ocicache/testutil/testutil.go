// Package testutil provides helpers for tests that write images into
// hypeman's OCI blob cache, shared across packages.
package testutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/kernel/hypeman/lib/paths"
)

// WriteImage stores an image's blobs (layers, config, manifest) in the OCI
// cache under p and returns the manifest digest.
func WriteImage(p *paths.Paths, img v1.Image) (string, error) {
	blobDir := p.OCICacheBlobDir()
	if err := os.MkdirAll(blobDir, 0755); err != nil {
		return "", fmt.Errorf("create blob dir: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return "", fmt.Errorf("layers: %w", err)
	}
	for _, layer := range layers {
		rc, err := layer.Compressed()
		if err != nil {
			return "", fmt.Errorf("layer reader: %w", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return "", fmt.Errorf("read layer: %w", err)
		}
		hash, err := layer.Digest()
		if err != nil {
			return "", fmt.Errorf("layer digest: %w", err)
		}
		if err := os.WriteFile(filepath.Join(blobDir, hash.Hex), data, 0644); err != nil {
			return "", fmt.Errorf("write layer blob: %w", err)
		}
	}

	rawConfig, err := img.RawConfigFile()
	if err != nil {
		return "", fmt.Errorf("raw config: %w", err)
	}
	configHash, err := img.ConfigName()
	if err != nil {
		return "", fmt.Errorf("config name: %w", err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, configHash.Hex), rawConfig, 0644); err != nil {
		return "", fmt.Errorf("write config blob: %w", err)
	}

	rawManifest, err := img.RawManifest()
	if err != nil {
		return "", fmt.Errorf("raw manifest: %w", err)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("digest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, digest.Hex), rawManifest, 0644); err != nil {
		return "", fmt.Errorf("write manifest blob: %w", err)
	}

	return digest.String(), nil
}
