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

func (m *manager) skipMachineBuildRecovery(meta *buildMetadata) bool {
	if !m.config.MachineBuildOnly {
		return false
	}
	if meta.Request != nil && meta.Request.MachineBaseImage != "" && m.config.MachineBuild != nil {
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
	// An interrupted attempt skips its own cleanup; drop its workspace first.
	removeMachineWorkspaces(m.paths.BuildDir(id))
	defer removeMachineWorkspaces(m.paths.BuildDir(id))
	result, err := backend.runPrepared(ctx, MachineBuildRequest{ID: id, BaseImage: req.MachineBaseImage, SourceHash: req.SourceHash, Policy: *policy, onStarted: func(instanceID string) error { return m.recordMachineBuilder(id, instanceID) }}, base, sourcePath, filepath.Dir(filepath.Dir(sourcePath)))
	if err != nil {
		return nil, err
	}
	ref, err := images.ParseNormalizedRef(result.Publication.Reference)
	if err != nil || ref.Repository() != strings.TrimSuffix(stripRegistryScheme(m.config.RegistryURL), "/")+"/builds/"+id {
		return nil, fmt.Errorf("machine publication is outside the authorized job repository")
	}
	return &BuildResult{Success: true, ImageDigest: result.Publication.Digest, Provenance: result.Provenance}, nil
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

// enqueueMachineBuild queues a machine build whose source is already staged.
func (m *manager) enqueueMachineBuild(meta *buildMetadata, req CreateBuildRequest, policy *BuildPolicy) *Build {
	queuePos := m.queue.EnqueueSerial(meta.ID, req, req.BuilderID, func() {
		m.runBuild(context.Background(), meta.ID, req, policy)
	})
	build := meta.toBuild()
	if queuePos > 0 {
		build.QueuePosition = &queuePos
	}
	m.logger.Info("build created", "id", meta.ID, "queue_position", queuePos)
	return build
}

// removeMachineWorkspaces deletes export workspaces under a build directory. Each
// holds a full exported disk.
func removeMachineWorkspaces(buildDir string) {
	matches, _ := filepath.Glob(filepath.Join(buildDir, "machine-build-*"))
	for _, dir := range matches {
		_ = os.RemoveAll(dir)
	}
}

// removeMachineSource deletes a machine build's staged source once its image is
// ready. It stays until then so recovery can rebuild an interrupted job.
func (m *manager) removeMachineSource(id string) {
	if err := os.Remove(filepath.Join(m.paths.BuildSourceDir(id), "source.tar.gz")); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Warn("failed to remove machine build source", "id", id, "error", err)
	}
}
