package builds

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kernel/hypeman/lib/images"
)

// executeMachineBuild is a backend of the same queued build job. runBuild owns
// deadline/status/log completion, and CreateBuild already staged verified source.
func (m *manager) executeMachineBuild(ctx context.Context, id string, req CreateBuildRequest, policy *BuildPolicy) (*BuildResult, error) {
	backend := m.config.MachineBuild
	if backend == nil {
		return nil, fmt.Errorf("machine build backend is not configured")
	}
	base, err := m.imageManager.GetImage(ctx, req.MachineBaseImage)
	if err != nil {
		return nil, machineFailure("resolve_base", err)
	}
	sourcePath, err := filepath.Abs(filepath.Join(m.paths.BuildSourceDir(id), "source.tar.gz"))
	if err != nil {
		return nil, err
	}
	result, err := backend.runPrepared(ctx, MachineBuildRequest{ID: id, BaseImage: req.MachineBaseImage, SourceHash: req.SourceHash, Policy: *policy}, base, sourcePath, filepath.Dir(filepath.Dir(sourcePath)))
	if err != nil {
		return nil, err
	}
	ref, err := images.ParseNormalizedRef(result.Publication.Reference)
	if err != nil || ref.Repository() != stripRegistryScheme(m.config.RegistryURL)+"/builds/"+id {
		return nil, fmt.Errorf("machine publication is outside the authorized job repository")
	}
	if meta, err := readMetadata(m.paths, id); err == nil {
		meta.BuilderInstance = &result.BuilderInstanceID
		if err = writeMetadata(m.paths, meta); err != nil {
			return nil, machineFailure("record_builder", err)
		}
	}
	return &BuildResult{Success: true, ImageDigest: result.Publication.Digest, Provenance: result.Provenance}, nil
}
