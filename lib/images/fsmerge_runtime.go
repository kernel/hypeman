package images

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/paths"
)

var errFsmergeUnsupported = errors.New("fsmerge is unsupported on this host")

const (
	fsmergeReconcileInterval = time.Minute
	fsmergeCleanupTimeout    = 10 * time.Second
)

func (m *manager) removeDigestIfUnreferenced(repository, digestHex string, preserveDigestOnly bool) error {
	unreferenced, err := removeDigestIfUnreferenced(m.paths, repository, digestHex, preserveDigestOnly)
	if err != nil {
		return err
	}
	if !unreferenced {
		return nil
	}
	return m.closeFsmergeDevice(digestHex)
}

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
	if model == nil || model.RuntimeFSType != runtimeRootfsFsmerge {
		return fallback, nil
	}

	digestHex := strings.TrimPrefix(image.Digest, "sha256:")
	m.fsmergeMu.Lock()
	if m.fsmergeDevices == nil {
		m.fsmergeDevices = make(map[string]*dmLinearDevice)
	}
	device := m.fsmergeDevices[digestHex]
	m.fsmergeMu.Unlock()
	if device != nil {
		return device.Path(), nil
	}

	value, err, _ := m.fsmergeFlights.Do(digestHex, func() (any, error) {
		m.fsmergeMu.Lock()
		device := m.fsmergeDevices[digestHex]
		m.fsmergeMu.Unlock()
		if device != nil {
			return device, nil
		}

		backingPaths := make([]string, 0, len(model.Layers)+1)
		backingPaths = append(backingPaths, fallback)
		for _, descriptor := range model.Layers {
			layerHex, err := layerDigestHex(descriptor.Digest)
			if err != nil {
				return nil, err
			}
			record, err := readLayerRecord(m.paths, layerHex)
			if err != nil {
				return nil, err
			}
			if record == nil || !record.matches(descriptor) {
				return nil, fmt.Errorf("missing materialized layer artifact %s", descriptor.Digest)
			}
			artifactPath := layerArtifactPath(m.paths, layerHex)
			info, err := os.Stat(artifactPath)
			if err != nil {
				return nil, fmt.Errorf("stat layer artifact %s: %w", descriptor.Digest, err)
			}
			if !info.Mode().IsRegular() || info.Size() != record.SizeBytes {
				return nil, fmt.Errorf("invalid layer artifact %s", descriptor.Digest)
			}
			backingPaths = append(backingPaths, artifactPath)
		}

		name := fsmergeDeviceName(digestHex)
		device, found, err := existingDMLinearDevice(ctx, name, len(backingPaths))
		if err != nil {
			return nil, fmt.Errorf("inspect existing fsmerge device: %w", err)
		}
		if !found {
			device, err = createDMLinearDevice(ctx, name, backingPaths)
			if err != nil {
				if errors.Is(err, errFsmergeUnsupported) {
					return nil, fmt.Errorf("fsmerge rootfs requires device-mapper support")
				}
				return nil, err
			}
		}
		m.fsmergeMu.Lock()
		m.fsmergeDevices[digestHex] = device
		m.fsmergeMu.Unlock()
		return device, nil
	})
	if err != nil {
		return "", err
	}
	return value.(*dmLinearDevice).Path(), nil
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
	return readManifestModelAt(manifestModelPath(p, layout, digestHex), digestHex)
}

func (m *manager) StartFsmergeReconciler(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.fsmergeReconcilerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(fsmergeReconcileInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.reconcileFsmergeDevices(ctx)
				}
			}
		}()
	})
}

func (m *manager) reconcileFsmergeDevices(ctx context.Context) {
	names, err := listDMDeviceNames(ctx, "hypeman-img-")
	if err != nil {
		if !errors.Is(err, errFsmergeUnsupported) {
			slog.WarnContext(ctx, "failed to list fsmerge devices", "error", err)
		}
	} else {
		for _, name := range names {
			digestHex := strings.TrimPrefix(name, "hypeman-img-")
			if err := m.closeFsmergeDeviceIfUnreferenced(digestHex); err != nil {
				slog.WarnContext(ctx, "failed to reconcile fsmerge device", "device", name, "error", err)
			}
		}
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), fsmergeCleanupTimeout)
	defer cancel()

	m.fsmergeMu.Lock()
	defer m.fsmergeMu.Unlock()
	device := m.fsmergeDevices[digestHex]
	if device == nil {
		if !dmLinearAvailable(ctx) {
			return nil
		}
		var found bool
		var err error
		device, found, err = existingDMLinearDevice(ctx, fsmergeDeviceName(digestHex), -1)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
	}
	if err := device.Close(ctx); err != nil {
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
