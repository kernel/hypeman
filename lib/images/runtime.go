package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kernel/hypeman/lib/paths"
)

// LayeredRuntimeManager prepares the writable overlay for an image whose
// read-only rootfs is a shared composed base.
type LayeredRuntimeManager interface {
	RequiredOverlayBytes(ctx context.Context, image *Image) (int64, error)
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
	baseRef := sharedBaseDigest(model)
	result := m.layers.flights.DoChan("base\x00"+baseRef, func() (any, error) {
		buildCtx, cancel := context.WithTimeout(context.Background(), layerBuildTimeout)
		defer cancel()
		select {
		case m.layers.builds <- struct{}{}:
			defer func() { <-m.layers.builds }()
		case <-buildCtx.Done():
			return nil, buildCtx.Err()
		}
		m.layers.beginLayerBuild()
		defer m.layers.endLayerBuild()
		return m.buildSharedBaseOnce(buildCtx, model, buildDir, baseRef)
	})
	select {
	case <-ctx.Done():
		return "", 0, ctx.Err()
	case shared := <-result:
		if shared.Err != nil {
			return "", 0, shared.Err
		}
		built := shared.Val.(sharedBaseBuildResult)
		return built.digest, built.size, nil
	}
}

type sharedBaseBuildResult struct {
	digest string
	size   int64
}

func (m *manager) buildSharedBaseOnce(ctx context.Context, model *imageManifestModel, buildDir, baseRef string) (sharedBaseBuildResult, error) {
	if model.BaseLayerCount == 0 || model.BaseLayerCount >= len(model.Layers) {
		return sharedBaseBuildResult{}, fmt.Errorf("invalid shared base layer count: %d", model.BaseLayerCount)
	}
	baseHex := strings.TrimPrefix(baseRef, "sha256:")
	basePath := m.paths.ImageBasePath(baseHex)
	if stat, err := os.Stat(basePath); err == nil {
		if stat.Mode().IsRegular() && stat.Size() > 0 {
			return sharedBaseBuildResult{digest: baseRef, size: stat.Size()}, nil
		}
		if err := removePath(basePath); err != nil {
			return sharedBaseBuildResult{}, fmt.Errorf("discard invalid shared base: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return sharedBaseBuildResult{}, fmt.Errorf("stat shared base: %w", err)
	}

	baseDir := filepath.Join(buildDir, "base-rootfs")
	if err := m.ociClient.composeLayers(ctx, baseDir, model.Layers[:model.BaseLayerCount]); err != nil {
		return sharedBaseBuildResult{}, fmt.Errorf("compose shared base: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(basePath), 0755); err != nil {
		return sharedBaseBuildResult{}, fmt.Errorf("create shared base directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(filepath.Dir(basePath), ".base-build-*")
	if err != nil {
		return sharedBaseBuildResult{}, fmt.Errorf("create shared base staging directory: %w", err)
	}
	defer removePath(tempDir)
	tempPath := filepath.Join(tempDir, filepath.Base(basePath))
	size, err := ExportRootfsWithContext(ctx, baseDir, tempPath, DefaultImageFormat)
	if err != nil {
		return sharedBaseBuildResult{}, fmt.Errorf("export shared base: %w", err)
	}
	if err := installAtomically(basePath, func(path string) error { return os.Rename(tempPath, path) }); err != nil {
		return sharedBaseBuildResult{}, fmt.Errorf("install shared base: %w", err)
	}
	return sharedBaseBuildResult{digest: baseRef, size: size}, nil
}

func (m *manager) runtimeLayerArtifact(ctx context.Context, desc layerDescriptor) (*layerArtifact, error) {
	layerHex := strings.TrimPrefix(desc.Digest, "sha256:")
	record, err := readLayerRecord(m.paths, layerHex)
	if err != nil && !errors.Is(err, errCorruptLayerRecord) {
		return nil, err
	}
	if err != nil || record == nil || !record.matches(desc) {
		return m.materializeLayerArtifact(ctx, desc)
	}
	info, err := os.Stat(layerArtifactPath(m.paths, layerHex))
	if err != nil || !info.Mode().IsRegular() || info.Size() != record.SizeBytes {
		return m.materializeLayerArtifact(ctx, desc)
	}
	return record, nil
}

func (m *manager) RequiredOverlayBytes(ctx context.Context, image *Image) (int64, error) {
	model, err := readRuntimeManifestModel(m.paths, image.Name, strings.TrimPrefix(image.Digest, "sha256:"))
	if err != nil {
		return 0, err
	}
	if model == nil || model.BaseDigest == "" {
		return 0, nil
	}
	if model.BaseLayerCount != len(model.Layers)-1 {
		return 0, fmt.Errorf("unsupported layered image plan: %d base layers, %d total layers", model.BaseLayerCount, len(model.Layers))
	}
	desc := model.Layers[model.BaseLayerCount]
	record, err := m.runtimeLayerArtifact(ctx, desc)
	if err != nil {
		return 0, fmt.Errorf("prepare image layer artifact: %w", err)
	}
	return alignToSector(record.UnpackedBytes + record.UnpackedBytes/2), nil
}

func (m *manager) PrepareInstanceOverlay(ctx context.Context, image *Image, overlayPath string, sizeBytes int64) error {
	digestHex := strings.TrimPrefix(image.Digest, "sha256:")
	model, err := readRuntimeManifestModel(m.paths, image.Name, digestHex)
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
	if _, err := m.runtimeLayerArtifact(ctx, desc); err != nil {
		return fmt.Errorf("prepare image layer artifact: %w", err)
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
	if err := extractLayerArtifact(ctx, artifactPath, upper); err != nil {
		return fmt.Errorf("copy image layer into overlay: %w", err)
	}
	// The merged root inherits the upperdir's mode. fsck.erofs may restore the
	// extracted layer root's mode, so set it after extraction.
	if err := os.Chmod(upper, 0755); err != nil {
		return fmt.Errorf("set overlay upper directory mode: %w", err)
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

func readRuntimeManifestModel(p *paths.Paths, imageName, digestHex string) (*imageManifestModel, error) {
	if imageName != "" {
		ref, err := ParseNormalizedRef(imageName)
		if err != nil {
			return nil, err
		}
		layout := resolveImageLayout(p, ref.Repository(), digestHex)
		return readManifestModelAt(manifestModelPath(p, layout, digestHex), digestHex)
	}
	return readManifestModel(p, digestHex)
}

func extractLayerArtifact(ctx context.Context, artifactPath, dest string) error {
	cmd := exec.CommandContext(ctx, "fsck.erofs", "--extract="+dest, "--xattrs", "--preserve", artifactPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract layer artifact: %w, output: %s", err, output)
	}
	return nil
}
