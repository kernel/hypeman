package builds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
)

// machineBuild reports whether a request selects the native macOS build path.
func machineBuild(req *CreateBuildRequest) bool {
	return req != nil && req.MachineBaseImage != ""
}

func (m *manager) skipMachineBuildRecovery(meta *buildMetadata) bool {
	if !m.config.MachineBuildOnly {
		return false
	}
	if machineBuild(meta.Request) && m.config.MachineBuild != nil {
		return false
	}
	m.logger.Warn("leaving unsupported build pending in machine-build-only mode", "id", meta.ID)
	return true
}

// executeMachineBuild is a backend of the same queued build job. runBuild owns
// deadline/status/log completion, and CreateBuild already staged verified source.
func (m *manager) executeMachineBuild(ctx context.Context, id string, req CreateBuildRequest, policy *BuildPolicy) (*BuildResult, error) {
	backend := m.config.MachineBuild
	if backend == nil {
		return nil, fmt.Errorf("machine build backend is not configured")
	}
	if err := m.reapPriorMachineBuilder(ctx, id); err != nil {
		return nil, machineFailure("reap_prior_builder", err)
	}
	base, err := m.imageManager.GetImage(ctx, req.MachineBaseImage)
	if err != nil {
		return nil, machineFailure("resolve_base", err)
	}
	sourcePath, err := filepath.Abs(filepath.Join(m.paths.BuildSourceDir(id), "source.tar.gz"))
	if err != nil {
		return nil, err
	}
	job := MachineBuildRequest{
		ID: id, BaseImage: req.MachineBaseImage, SourceHash: req.SourceHash, Policy: *policy,
		registryBase: machineRegistryBase(backend.Publisher, req.MachineBaseImage),
		onStarted:    func(instanceID string) error { return m.recordMachineBuilder(id, instanceID) },
		onPublished: func(ctx context.Context, publication MachinePublication) error {
			if err := m.imageManager.WaitForReady(ctx, "builds/"+id+"@"+publication.Digest); err != nil {
				return fmt.Errorf("machine image import: %w", err)
			}
			return nil
		},
	}
	result, err := backend.runPrepared(ctx, job, base, sourcePath)
	if err != nil {
		return nil, err
	}
	ref, err := images.ParseNormalizedRef(result.Publication.Reference)
	if err != nil || ref.Repository() != strings.TrimSuffix(stripRegistryScheme(m.config.RegistryURL), "/")+"/builds/"+id {
		return nil, fmt.Errorf("machine publication is outside the authorized job repository")
	}
	return &BuildResult{Success: true, ImageDigest: result.Publication.Digest, ImageRef: result.Publication.Reference, Provenance: result.Provenance}, nil
}

// reapPriorMachineBuilder removes the builder recorded by an interrupted attempt of
// this same job before a re-enqueued attempt starts. Nothing from that builder is
// published, so a forced stop is acceptable; a failed delete fails this attempt
// rather than leaving two builders with storage in play.
func (m *manager) reapPriorMachineBuilder(ctx context.Context, id string) error {
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return err
	}
	// An attempt interrupted before it recorded its builder still named it after the build.
	prior := machineBuilderName(id)
	if meta.BuilderInstance != nil && *meta.BuilderInstance != "" {
		prior = *meta.BuilderInstance
	}
	inst, err := m.instanceManager.GetInstance(ctx, prior)
	if errors.Is(err, instances.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if inst.State != instances.StateStopped {
		if _, err := m.instanceManager.StopInstance(ctx, inst.Id); err != nil {
			return fmt.Errorf("stop prior builder: %w", err)
		}
	}
	if err := m.instanceManager.DeleteInstance(ctx, inst.Id); err != nil && !errors.Is(err, instances.ErrNotFound) {
		return fmt.Errorf("delete prior builder: %w", err)
	}
	m.logger.Info("reaped prior machine builder", "build_id", id, "instance_id", inst.Id)
	return nil
}

// storeMachineSource stages machine source privately and records its SHA256
// provenance. A caller-supplied source hash must match.
func (m *manager) storeMachineSource(ctx context.Context, meta *buildMetadata, data []byte) error {
	sourceDir := m.paths.BuildSourceDir(meta.ID)
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		return fmt.Errorf("store source: %w", err)
	}
	hash, err := stageBuildSource(ctx, bytes.NewReader(data), filepath.Join(sourceDir, "source.tar.gz"), int64(len(data)))
	if err != nil {
		return fmt.Errorf("store source: %w", err)
	}
	if meta.Request.SourceHash != "" && meta.Request.SourceHash != hash {
		return ErrSourceHashMismatch
	}
	meta.Request.SourceHash = hash
	if err := writeMetadata(m.paths, meta); err != nil {
		return fmt.Errorf("write source provenance: %w", err)
	}
	return nil
}

// admitMachineBuild validates a machine build request and returns its policy.
// Machine resources inherit the installed base, not Linux builder defaults.
func (m *manager) admitMachineBuild(req *CreateBuildRequest, sourceData []byte) (*BuildPolicy, error) {
	if m.config.MachineBuild == nil || m.config.MachineBuild.Driver == nil || m.config.MachineBuild.Publisher == nil {
		return nil, fmt.Errorf("machine build backend is not configured")
	}
	if req.Dockerfile != "" || req.BuilderID != "" || len(req.BuildArgs) != 0 || len(req.Secrets) != 0 || req.CacheScope != "" || req.GlobalCacheKey != "" || req.IsAdminBuild {
		return nil, fmt.Errorf("machine builds do not support Linux builder/cache/secret options")
	}
	if len(sourceData) > maxBuildSourceBytes {
		return nil, fmt.Errorf("machine build source too large")
	}
	ref, err := images.ParseNormalizedRef(req.MachineBaseImage)
	if err != nil || !ref.IsDigest() || !ValidMachineDigest(ref.Digest()) {
		return nil, fmt.Errorf("machine base must be pinned by sha256 digest")
	}
	policy := &BuildPolicy{}
	if req.BuildPolicy != nil {
		requested := *req.BuildPolicy
		policy = &requested
	}
	if err := normalizeMachinePolicy(policy); err != nil {
		return nil, err
	}
	req.BuildPolicy = policy
	return policy, nil
}

// machineRegistryBase returns the base as a registry base when the publisher
// writes to the registry that stores it, so its layers are reused. Any other base
// publishes all of its layers.
func machineRegistryBase(publisher MachineBuildPublisher, baseImage string) *MachineBase {
	streaming, ok := publisher.(*StreamingMachinePublisher)
	if !ok {
		return nil
	}
	registry, err := streaming.registry()
	if err != nil {
		return nil
	}
	ref, err := images.ParseNormalizedRef(baseImage)
	if err != nil || !ref.IsDigest() {
		return nil
	}
	repo, ok := strings.CutPrefix(ref.Repository(), registry.Host+"/")
	if !ok {
		return nil
	}
	return &MachineBase{Repo: repo, Digest: ref.Digest()}
}

// removeMachineSource deletes a machine build's staged source after the job is terminal.
func (m *manager) removeMachineSource(id string) {
	if err := os.Remove(filepath.Join(m.paths.BuildSourceDir(id), "source.tar.gz")); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Warn("failed to remove machine build source", "id", id, "error", err)
	}
}
