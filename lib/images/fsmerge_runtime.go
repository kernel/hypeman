package images

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernel/hypeman/lib/paths"
)

var errFsmergeUnsupported = errors.New("fsmerge is unsupported on this host")

// RootfsPathProvider supplies the host path for an image's read-only root
// disk. Layered images use a shared dm-linear device; legacy and fallback
// images continue to use their content-addressed disk file.
type RootfsPathProvider interface {
	RootfsPath(ctx context.Context, image *Image) (string, error)
}

func (m *manager) RootfsPath(ctx context.Context, image *Image) (string, error) {
	if image == nil {
		return "", errors.New("image is required")
	}
	fallback, err := GetDiskPath(m.paths, image.Name, image.Digest)
	if err != nil {
		return "", err
	}
	model, err := readRuntimeManifestModel(m.paths, image.Name, strings.TrimPrefix(image.Digest, "sha256:"))
	if err != nil {
		return "", err
	}
	if model == nil || model.RuntimeFSType != "fsmerge" {
		return fallback, nil
	}

	digestHex := strings.TrimPrefix(image.Digest, "sha256:")
	m.fsmergeMu.Lock()
	defer m.fsmergeMu.Unlock()
	if m.fsmergeDevices == nil {
		m.fsmergeDevices = make(map[string]*dmLinearDevice)
	}
	if device := m.fsmergeDevices[digestHex]; device != nil {
		return device.Path(), nil
	}

	backingPaths := make([]string, 0, len(model.Layers)+1)
	backingPaths = append(backingPaths, fallback)
	for _, descriptor := range model.Layers {
		layerHex, err := layerDigestHex(descriptor.Digest)
		if err != nil {
			return "", err
		}
		record, err := readLayerRecord(m.paths, layerHex)
		if err != nil {
			return "", err
		}
		if record == nil || !record.matches(descriptor) {
			return "", fmt.Errorf("missing materialized layer artifact %s", descriptor.Digest)
		}
		artifactPath := layerArtifactPath(m.paths, layerHex)
		info, err := os.Stat(artifactPath)
		if err != nil {
			return "", fmt.Errorf("stat layer artifact %s: %w", descriptor.Digest, err)
		}
		if !info.Mode().IsRegular() || info.Size() != record.SizeBytes {
			return "", fmt.Errorf("invalid layer artifact %s", descriptor.Digest)
		}
		backingPaths = append(backingPaths, artifactPath)
	}

	name := fsmergeDeviceName(digestHex)
	device, err := createDMLinearDevice(ctx, name, backingPaths)
	if err != nil {
		if errors.Is(err, errFsmergeUnsupported) {
			return "", fmt.Errorf("fsmerge rootfs requires device-mapper support")
		}
		return "", err
	}
	m.fsmergeDevices[digestHex] = device
	return device.Path(), nil
}

func readRuntimeManifestModel(p *paths.Paths, imageName, digestHex string) (*imageManifestModel, error) {
	if imageName == "" {
		return nil, nil
	}
	ref, err := ParseNormalizedRef(imageName)
	if err != nil {
		return nil, err
	}
	// The image manager's content layout is authoritative after a successful
	// build, while legacy images remain on their existing repository layout.
	layout := resolveImageLayout(p, ref.Repository(), digestHex)
	modelPath := p.ImageContentManifestModel(digestHex)
	if !layout.content {
		modelPath = filepath.Join(layout.dir, "manifest.json")
	}
	return readManifestModelAt(modelPath, digestHex)
}

func (m *manager) closeFsmergeDeviceIfUnreferenced(digestHex string) error {
	count, err := contentTagCount(m.paths, digestHex)
	if err != nil {
		return err
	}
	if count > 0 || contentPullInProgress(m.paths, digestHex) || contentIsDigestOnly(m.paths, digestHex) {
		return nil
	}
	return m.closeFsmergeDevice(digestHex)
}

func (m *manager) closeFsmergeDevice(digestHex string) error {
	m.fsmergeMu.Lock()
	defer m.fsmergeMu.Unlock()
	device := m.fsmergeDevices[digestHex]
	if device == nil {
		return nil
	}
	if err := device.Close(); err != nil {
		return err
	}
	delete(m.fsmergeDevices, digestHex)
	return nil
}

func fsmergeDeviceName(digestHex string) string {
	name := "hypeman-img-" + digestHex
	if len(name) > 128 {
		return name[:128]
	}
	return name
}
