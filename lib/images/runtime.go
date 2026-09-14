package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LayeredRuntimeManager prepares the writable overlay for an image whose
// read-only rootfs is a shared composed base.
type LayeredRuntimeManager interface {
	PrepareInstanceOverlay(ctx context.Context, image *Image, overlayPath string, sizeBytes int64) error
}

func sharedBaseDigest(model *imageManifestModel) string {
	hash := sha256.New()
	hash.Write([]byte("hypeman-base-v1\x00"))
	for _, layer := range model.Layers[:model.BaseLayerCount] {
		hash.Write([]byte(layer.Digest))
		hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func (m *manager) buildSharedBase(ctx context.Context, model *imageManifestModel, buildDir string) (string, int64, error) {
	if model.BaseLayerCount == 0 || model.BaseLayerCount >= len(model.Layers) {
		return "", 0, fmt.Errorf("invalid shared base layer count: %d", model.BaseLayerCount)
	}
	baseRef := sharedBaseDigest(model)
	baseHex := strings.TrimPrefix(baseRef, "sha256:")
	basePath := m.paths.ImageBasePath(baseHex)
	if stat, err := os.Stat(basePath); err == nil {
		return baseRef, stat.Size(), nil
	} else if !os.IsNotExist(err) {
		return "", 0, fmt.Errorf("stat shared base: %w", err)
	}

	baseDir := filepath.Join(buildDir, "base-rootfs")
	if err := m.ociClient.composeLayers(ctx, baseDir, model.Layers[:model.BaseLayerCount]); err != nil {
		return "", 0, fmt.Errorf("compose shared base: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(basePath), 0755); err != nil {
		return "", 0, fmt.Errorf("create shared base directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(filepath.Dir(basePath), ".base-build-*")
	if err != nil {
		return "", 0, fmt.Errorf("create shared base staging directory: %w", err)
	}
	defer removePath(tempDir)
	tempPath := filepath.Join(tempDir, filepath.Base(basePath))
	size, err := ExportRootfsWithContext(ctx, baseDir, tempPath, DefaultImageFormat)
	if err != nil {
		return "", 0, fmt.Errorf("export shared base: %w", err)
	}
	if err := installAtomically(basePath, func(path string) error { return os.Rename(tempPath, path) }); err != nil {
		return "", 0, fmt.Errorf("install shared base: %w", err)
	}
	return baseRef, size, nil
}

func (m *manager) PrepareInstanceOverlay(ctx context.Context, image *Image, overlayPath string, sizeBytes int64) error {
	model, err := readManifestModel(m.paths, strings.TrimPrefix(image.Digest, "sha256:"))
	if err != nil {
		return fmt.Errorf("read image manifest model: %w", err)
	}
	if model == nil || model.BaseDigest == "" {
		return CreateEmptyExt4Disk(overlayPath, sizeBytes)
	}
	if model.BaseLayerCount != len(model.Layers)-1 {
		return fmt.Errorf("unsupported layered image plan: %d base layers, %d total layers", model.BaseLayerCount, len(model.Layers))
	}
	basePath := m.paths.ImageBasePath(strings.TrimPrefix(model.BaseDigest, "sha256:"))
	if _, err := os.Stat(basePath); err != nil {
		return fmt.Errorf("stat shared image base: %w", err)
	}

	desc := model.Layers[model.BaseLayerCount]
	layerHex := strings.TrimPrefix(desc.Digest, "sha256:")
	record, err := readLayerRecord(m.paths, layerHex)
	if err != nil {
		return fmt.Errorf("read image layer artifact: %w", err)
	}
	if record == nil || !record.matches(desc) {
		return fmt.Errorf("image layer artifact is missing or stale: %s", desc.Digest)
	}
	artifactPath := layerArtifactPath(m.paths, layerHex)
	if _, err := os.Stat(artifactPath); err != nil {
		return fmt.Errorf("stat image layer artifact: %w", err)
	}

	stage, err := os.MkdirTemp(filepath.Dir(overlayPath), ".overlay-stage-*")
	if err != nil {
		return fmt.Errorf("create overlay staging directory: %w", err)
	}
	defer removePath(stage)
	upper := filepath.Join(stage, "upper")
	if err := os.Mkdir(upper, 0755); err != nil {
		return fmt.Errorf("create overlay upper directory: %w", err)
	}
	work := filepath.Join(stage, "work")
	if err := os.Mkdir(work, 0755); err != nil {
		return fmt.Errorf("create overlay work directory: %w", err)
	}
	mounted, err := mountReadOnlyArtifact(ctx, artifactPath)
	if err != nil {
		return err
	}
	defer mounted.unmount()
	if err := copyArtifactTree(ctx, mounted.path, upper); err != nil {
		return fmt.Errorf("copy image layer into overlay: %w", err)
	}

	tempPath := overlayPath + ".tmp"
	defer os.Remove(tempPath)
	if err := CreateExt4DiskFromRootfsWithContext(ctx, stage, tempPath, sizeBytes); err != nil {
		return fmt.Errorf("create populated overlay disk: %w", err)
	}
	if err := os.Rename(tempPath, overlayPath); err != nil {
		return fmt.Errorf("install populated overlay disk: %w", err)
	}
	return nil
}

type mountedArtifact struct{ path string }

func mountReadOnlyArtifact(ctx context.Context, artifactPath string) (*mountedArtifact, error) {
	mountpoint, err := os.MkdirTemp("", "hypeman-layer-mount-*")
	if err != nil {
		return nil, fmt.Errorf("create artifact mountpoint: %w", err)
	}
	cmd := exec.CommandContext(ctx, "/bin/mount", "-o", "loop,ro", artifactPath, mountpoint)
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(mountpoint)
		return nil, fmt.Errorf("mount layer artifact: %w, output: %s", err, output)
	}
	return &mountedArtifact{path: mountpoint}, nil
}

func (m *mountedArtifact) unmount() {
	_ = exec.Command("/bin/umount", m.path).Run()
	_ = os.Remove(m.path)
}

func copyArtifactTree(ctx context.Context, source, dest string) error {
	cmd := exec.CommandContext(ctx, "/bin/cp", "-a", "--preserve=all", source+"/.", dest)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy tree: %w, output: %s", err, output)
	}
	return nil
}
