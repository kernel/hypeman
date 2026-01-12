package builds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/volumes"
	"go.opentelemetry.io/otel/metric"
)

// Manager interface for the build system
type Manager interface {
	// CreateBuild starts a new build job
	CreateBuild(ctx context.Context, req CreateBuildRequest, sourceData []byte) (*Build, error)

	// GetBuild returns a build by ID
	GetBuild(ctx context.Context, id string) (*Build, error)

	// ListBuilds returns all builds
	ListBuilds(ctx context.Context) ([]*Build, error)

	// CancelBuild cancels a pending or running build
	CancelBuild(ctx context.Context, id string) error

	// GetBuildLogs returns the logs for a build
	GetBuildLogs(ctx context.Context, id string) ([]byte, error)

	// RecoverPendingBuilds recovers builds that were interrupted on restart
	RecoverPendingBuilds()
}

// Config holds configuration for the build manager
type Config struct {
	// MaxConcurrentBuilds is the maximum number of concurrent builds
	MaxConcurrentBuilds int

	// BuilderImage is the OCI image to use for builder VMs
	// This should contain rootless BuildKit and the builder agent
	BuilderImage string

	// RegistryURL is the URL of the registry to push built images to
	RegistryURL string

	// DefaultTimeout is the default build timeout in seconds
	DefaultTimeout int
}

// DefaultConfig returns the default build manager configuration
func DefaultConfig() Config {
	return Config{
		MaxConcurrentBuilds: 2,
		BuilderImage:        "hypeman/builder:latest",
		RegistryURL:         "localhost:8080",
		DefaultTimeout:       600, // 10 minutes
	}
}

type manager struct {
	config          Config
	paths           *paths.Paths
	instanceManager instances.Manager
	volumeManager   volumes.Manager
	secretProvider  SecretProvider
	vsockHandler    *VsockHandler
	logger          *slog.Logger
	metrics         *Metrics
	createMu        sync.Mutex
}

// NewManager creates a new build manager
func NewManager(
	p *paths.Paths,
	config Config,
	instanceMgr instances.Manager,
	volumeMgr volumes.Manager,
	secretProvider SecretProvider,
	logger *slog.Logger,
	meter metric.Meter,
) (Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	m := &manager{
		config:          config,
		paths:           p,
		instanceManager: instanceMgr,
		volumeManager:   volumeMgr,
		secretProvider:  secretProvider,
		vsockHandler:    NewVsockHandler(secretProvider, logger),
		logger:          logger,
	}

	// Initialize metrics if meter is provided
	if meter != nil {
		metrics, err := NewMetrics(meter)
		if err != nil {
			return nil, fmt.Errorf("create metrics: %w", err)
		}
		m.metrics = metrics
	}

	// Recover any pending builds from disk
	m.RecoverPendingBuilds()

	return m, nil
}

// CreateBuild starts a new build job
func (m *manager) CreateBuild(ctx context.Context, req CreateBuildRequest, sourceData []byte) (*Build, error) {
	m.logger.Info("creating build", "runtime", req.Runtime)

	// Validate runtime
	if !IsSupportedRuntime(req.Runtime) {
		return nil, fmt.Errorf("%w: %s", ErrInvalidRuntime, req.Runtime)
	}

	// Apply defaults to build policy
	policy := req.BuildPolicy
	if policy == nil {
		defaultPolicy := DefaultBuildPolicy()
		policy = &defaultPolicy
	} else {
		policy.ApplyDefaults()
	}

	// Preflight check: verify resources are available before accepting the build
	// This allows us to return 503 synchronously if resources are exhausted
	builderMemory := int64(policy.MemoryMB) * 1024 * 1024
	if err := m.instanceManager.CheckResourceAvailability(ctx, policy.CPUs, builderMemory); err != nil {
		if errors.Is(err, instances.ErrResourcesExhausted) {
			return nil, fmt.Errorf("%w: %v", ErrResourcesExhausted, err)
		}
		return nil, fmt.Errorf("check resource availability: %w", err)
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()

	// Generate build ID
	id := cuid2.Generate()

	// Create build metadata with status "building" (builds start immediately)
	meta := &buildMetadata{
		ID:        id,
		Status:    StatusBuilding,
		Runtime:   req.Runtime,
		Request:   &req,
		CreatedAt: time.Now(),
	}
	now := time.Now()
	meta.StartedAt = &now

	// Write initial metadata
	if err := writeMetadata(m.paths, meta); err != nil {
		return nil, fmt.Errorf("write metadata: %w", err)
	}

	// Store source data
	if err := m.storeSource(id, sourceData); err != nil {
		deleteBuild(m.paths, id)
		return nil, fmt.Errorf("store source: %w", err)
	}

	// Write build config for the builder agent
	buildConfig := &BuildConfig{
		JobID:           id,
		Runtime:         req.Runtime,
		BaseImageDigest: req.BaseImageDigest,
		RegistryURL:     m.config.RegistryURL,
		CacheScope:      req.CacheScope,
		SourcePath:      "/src",
		Dockerfile:      req.Dockerfile,
		BuildArgs:       req.BuildArgs,
		Secrets:         req.Secrets,
		TimeoutSeconds:  policy.TimeoutSeconds,
		NetworkMode:     policy.NetworkMode,
	}
	if err := writeBuildConfig(m.paths, id, buildConfig); err != nil {
		deleteBuild(m.paths, id)
		return nil, fmt.Errorf("write build config: %w", err)
	}

	// Start the build immediately in background
	go m.runBuild(context.Background(), id, req, policy)

	build := meta.toBuild()

	m.logger.Info("build created and started", "id", id)
	return build, nil
}

// storeSource stores the source tarball for a build
func (m *manager) storeSource(buildID string, data []byte) error {
	sourceDir := m.paths.BuildSourceDir(buildID)
	if err := ensureDir(sourceDir); err != nil {
		return err
	}

	// Write source tarball
	sourcePath := sourceDir + "/source.tar.gz"
	return writeFile(sourcePath, data)
}

// runBuild executes a build in a builder VM
func (m *manager) runBuild(ctx context.Context, id string, req CreateBuildRequest, policy *BuildPolicy) {
	start := time.Now()
	m.logger.Info("starting build", "id", id)

	// Check if build was cancelled before we started
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		m.logger.Error("failed to read metadata at build start", "id", id, "error", err)
		return
	}
	if isTerminalStatus(meta.Status) {
		m.logger.Info("build already in terminal state, skipping", "id", id, "status", meta.Status)
		return
	}

	// Update status to building (will be skipped if already terminal)
	m.updateStatus(id, StatusBuilding, nil)

	// Create timeout context
	buildCtx, cancel := context.WithTimeout(ctx, time.Duration(policy.TimeoutSeconds)*time.Second)
	defer cancel()

	// Run the build in a builder VM
	result, err := m.executeBuild(buildCtx, id, req, policy)

	duration := time.Since(start)
	durationMS := duration.Milliseconds()

	if err != nil {
		m.logger.Error("build failed", "id", id, "error", err, "duration", duration)
		errMsg := err.Error()
		m.updateBuildComplete(id, StatusFailed, nil, &errMsg, nil, &durationMS)
		if m.metrics != nil {
			m.metrics.RecordBuild(ctx, "failed", req.Runtime, duration)
		}
		return
	}

	if !result.Success {
		m.logger.Error("build failed", "id", id, "error", result.Error, "duration", duration)
		m.updateBuildComplete(id, StatusFailed, nil, &result.Error, &result.Provenance, &durationMS)
		if m.metrics != nil {
			m.metrics.RecordBuild(ctx, "failed", req.Runtime, duration)
		}
		return
	}

	m.logger.Info("build succeeded", "id", id, "digest", result.ImageDigest, "duration", duration)
	imageRef := fmt.Sprintf("%s/builds/%s", m.config.RegistryURL, id)
	m.updateBuildComplete(id, StatusReady, &result.ImageDigest, nil, &result.Provenance, &durationMS)

	// Update with image ref
	if meta, err := readMetadata(m.paths, id); err == nil {
		meta.ImageRef = &imageRef
		writeMetadata(m.paths, meta)
	}

	if m.metrics != nil {
		m.metrics.RecordBuild(ctx, "success", req.Runtime, duration)
	}
}

// executeBuild runs the build in a builder VM
func (m *manager) executeBuild(ctx context.Context, id string, req CreateBuildRequest, policy *BuildPolicy) (*BuildResult, error) {
	// Create a volume with the source data
	sourceVolID := fmt.Sprintf("build-source-%s", id)
	sourcePath := m.paths.BuildSourceDir(id) + "/source.tar.gz"

	// Open source tarball
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("open source: %w", err)
	}
	defer sourceFile.Close()

	// Create volume with source (using the volume manager's archive import)
	_, err = m.volumeManager.CreateVolumeFromArchive(ctx, volumes.CreateVolumeFromArchiveRequest{
		Id:     &sourceVolID,
		Name:   sourceVolID,
		SizeGb: 10, // 10GB should be enough for most source bundles
	}, sourceFile)
	if err != nil {
		return nil, fmt.Errorf("create source volume: %w", err)
	}
	defer m.volumeManager.DeleteVolume(ctx, sourceVolID)

	// Create builder instance
	builderName := fmt.Sprintf("builder-%s", id)
	networkEnabled := policy.NetworkMode == "egress"

	inst, err := m.instanceManager.CreateInstance(ctx, instances.CreateInstanceRequest{
		Name:           builderName,
		Image:          m.config.BuilderImage,
		Size:           int64(policy.MemoryMB) * 1024 * 1024,
		Vcpus:          policy.CPUs,
		NetworkEnabled: networkEnabled,
		Volumes: []instances.VolumeAttachment{
			{
				VolumeID:  sourceVolID,
				MountPath: "/src",
				Readonly:  true,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create builder instance: %w", err)
	}

	// Update metadata with builder instance
	if meta, err := readMetadata(m.paths, id); err == nil {
		meta.BuilderInstance = &inst.Id
		writeMetadata(m.paths, meta)
	}

	// Ensure cleanup
	defer func() {
		m.instanceManager.DeleteInstance(context.Background(), inst.Id)
	}()

	// Wait for build result via vsock
	// The builder agent will send the result when complete
	result, err := m.waitForResult(ctx, inst)
	if err != nil {
		return nil, fmt.Errorf("wait for result: %w", err)
	}

	return result, nil
}

// waitForResult waits for the build result from the builder agent
func (m *manager) waitForResult(ctx context.Context, inst *instances.Instance) (*BuildResult, error) {
	// Poll for the build result
	// In a production system, you'd use vsock for real-time communication
	// For now, we'll poll the instance state and check for completion

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	timeout := time.After(30 * time.Minute) // Maximum wait time

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, ErrBuildTimeout
		case <-ticker.C:
			// Check if instance is still running
			current, err := m.instanceManager.GetInstance(ctx, inst.Id)
			if err != nil {
				// Instance might have been deleted
				return nil, fmt.Errorf("check instance: %w", err)
			}

			// If instance stopped, check for result in logs
			if current.State == instances.StateStopped || current.State == instances.StateShutdown {
				// Try to parse result from logs
				// This is a fallback - ideally vsock would be used
				return &BuildResult{
					Success: false,
					Error:   "builder instance stopped unexpectedly",
				}, nil
			}
		}
	}
}

// updateStatus updates the build status
// It checks for terminal states to prevent race conditions (e.g., cancelled build being overwritten)
func (m *manager) updateStatus(id string, status string, err error) {
	meta, readErr := readMetadata(m.paths, id)
	if readErr != nil {
		m.logger.Error("read metadata for status update", "id", id, "error", readErr)
		return
	}

	// Don't overwrite terminal states - prevents race condition where cancelled
	// build gets overwritten by a concurrent goroutine setting it to building
	if isTerminalStatus(meta.Status) {
		m.logger.Debug("skipping status update for terminal build", "id", id, "current", meta.Status, "requested", status)
		return
	}

	meta.Status = status
	if status == StatusBuilding && meta.StartedAt == nil {
		now := time.Now()
		meta.StartedAt = &now
	}
	if err != nil {
		errMsg := err.Error()
		meta.Error = &errMsg
	}

	if writeErr := writeMetadata(m.paths, meta); writeErr != nil {
		m.logger.Error("write metadata for status update", "id", id, "error", writeErr)
	}
}

// isTerminalStatus returns true if the status represents a completed build
func isTerminalStatus(status string) bool {
	switch status {
	case StatusReady, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// updateBuildComplete updates the build with final results
func (m *manager) updateBuildComplete(id string, status string, digest *string, errMsg *string, provenance *BuildProvenance, durationMS *int64) {
	meta, readErr := readMetadata(m.paths, id)
	if readErr != nil {
		m.logger.Error("read metadata for completion", "id", id, "error", readErr)
		return
	}

	meta.Status = status
	meta.ImageDigest = digest
	meta.Error = errMsg
	meta.Provenance = provenance
	meta.DurationMS = durationMS

	now := time.Now()
	meta.CompletedAt = &now

	if writeErr := writeMetadata(m.paths, meta); writeErr != nil {
		m.logger.Error("write metadata for completion", "id", id, "error", writeErr)
	}
}

// GetBuild returns a build by ID
func (m *manager) GetBuild(ctx context.Context, id string) (*Build, error) {
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}

	return meta.toBuild(), nil
}

// ListBuilds returns all builds
func (m *manager) ListBuilds(ctx context.Context) ([]*Build, error) {
	metas, err := listAllBuilds(m.paths)
	if err != nil {
		return nil, err
	}

	builds := make([]*Build, 0, len(metas))
	for _, meta := range metas {
		builds = append(builds, meta.toBuild())
	}

	return builds, nil
}

// CancelBuild cancels a pending or running build
func (m *manager) CancelBuild(ctx context.Context, id string) error {
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return err
	}

	switch meta.Status {
	case StatusBuilding, StatusPushing:
		// Mark as cancelled first to prevent race condition with runBuild goroutine
		m.updateStatus(id, StatusCancelled, nil)

		// Then terminate the builder instance if it exists
		if meta.BuilderInstance != nil {
			// Use a fresh context with timeout since the request context may be cancelled
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := m.instanceManager.DeleteInstance(cleanupCtx, *meta.BuilderInstance); err != nil {
				m.logger.Warn("failed to delete builder instance during cancel", "id", id, "instance", *meta.BuilderInstance, "error", err)
			}
		}
		return nil

	case StatusReady, StatusFailed, StatusCancelled:
		return fmt.Errorf("build already completed with status: %s", meta.Status)

	default:
		return fmt.Errorf("unknown build status: %s", meta.Status)
	}
}

// GetBuildLogs returns the logs for a build
func (m *manager) GetBuildLogs(ctx context.Context, id string) ([]byte, error) {
	_, err := readMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}

	return readLog(m.paths, id)
}

// RecoverPendingBuilds recovers builds that were interrupted on restart
func (m *manager) RecoverPendingBuilds() {
	pending, err := listPendingBuilds(m.paths)
	if err != nil {
		m.logger.Error("list pending builds for recovery", "error", err)
		return
	}

	for _, meta := range pending {
		m.logger.Info("recovering build", "id", meta.ID, "status", meta.Status)

		// Start the build immediately in background
		if meta.Request != nil {
			// Capture values for goroutine
			buildID := meta.ID
			req := *meta.Request
			go func() {
				policy := DefaultBuildPolicy()
				if req.BuildPolicy != nil {
					policy = *req.BuildPolicy
				}
				m.runBuild(context.Background(), buildID, req, &policy)
			}()
		}
	}

	if len(pending) > 0 {
		m.logger.Info("recovered pending builds", "count", len(pending))
	}
}

// Helper functions

func ensureDir(path string) error {
	return os.MkdirAll(path, 0755)
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

