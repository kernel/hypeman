package builds

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/nrednav/cuid2"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/volumes"
	"go.opentelemetry.io/otel/metric"
)

//go:embed images/generic/Dockerfile
var builderDockerfile []byte

// Manager interface for the build system
type Manager interface {
	// Start starts the build manager's background services (vsock handler, etc.)
	// This should be called once when the API server starts.
	Start(ctx context.Context) error

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

	// StreamBuildEvents streams build events (logs, status changes, heartbeats)
	// With follow=false, returns existing logs then closes
	// With follow=true, continues streaming until build completes or context cancels
	StreamBuildEvents(ctx context.Context, id string, follow bool) (<-chan BuildEvent, error)

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

	// RegistryInsecure skips TLS verification for the registry (for self-signed certs)
	RegistryInsecure bool

	// RegistryCACert is the PEM-encoded CA certificate for verifying the registry's TLS cert
	// If set, this is passed to the builder VM to enable TLS verification
	RegistryCACert string

	// DefaultTimeout is the default build timeout in seconds
	DefaultTimeout int

	// RegistrySecret is the secret used to sign registry access tokens
	// This should be the same secret used by the registry middleware
	RegistrySecret string

	// DockerSocket is the path to the Docker socket for building the builder image
	DockerSocket string
}

// DefaultConfig returns the default build manager configuration
func DefaultConfig() Config {
	return Config{
		MaxConcurrentBuilds: 2,
		RegistryURL:         "localhost:8080",
		DefaultTimeout:      600, // 10 minutes
	}
}

// stripRegistryScheme removes http:// or https:// prefix from registry URL.
// This is needed because image references should not contain the scheme.
func stripRegistryScheme(registryURL string) string {
	if strings.HasPrefix(registryURL, "https://") {
		return strings.TrimPrefix(registryURL, "https://")
	}
	if strings.HasPrefix(registryURL, "http://") {
		return strings.TrimPrefix(registryURL, "http://")
	}
	return registryURL
}

type manager struct {
	config          Config
	paths           *paths.Paths
	queue           *BuildQueue
	instanceManager instances.Manager
	volumeManager   volumes.Manager
	imageManager    images.Manager
	secretProvider  SecretProvider
	tokenGenerator  *RegistryTokenGenerator
	logger          *slog.Logger
	metrics         *Metrics
	createMu        sync.Mutex
	builderReady    atomic.Bool

	// Status subscription system for SSE streaming
	statusSubscribers map[string][]chan BuildEvent
	subscriberMu      sync.RWMutex
}

// NewManager creates a new build manager
func NewManager(
	p *paths.Paths,
	config Config,
	instanceMgr instances.Manager,
	volumeMgr volumes.Manager,
	imageMgr images.Manager,
	secretProvider SecretProvider,
	logger *slog.Logger,
	meter metric.Meter,
) (Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	m := &manager{
		config:            config,
		paths:             p,
		queue:             NewBuildQueue(config.MaxConcurrentBuilds),
		instanceManager:   instanceMgr,
		volumeManager:     volumeMgr,
		imageManager:      imageMgr,
		secretProvider:    secretProvider,
		tokenGenerator:    NewRegistryTokenGenerator(config.RegistrySecret),
		logger:            logger,
		statusSubscribers: make(map[string][]chan BuildEvent),
	}

	// Initialize metrics if meter is provided
	if meter != nil {
		metrics, err := NewMetrics(meter)
		if err != nil {
			return nil, fmt.Errorf("create metrics: %w", err)
		}
		m.metrics = metrics
	}

	return m, nil
}

// Start starts the build manager's background services
func (m *manager) Start(ctx context.Context) error {
	go func() {
		m.ensureBuilderImage(ctx)
		// Recover pending builds only after the builder image is ready,
		// otherwise recovered builds fail with "builder image is being prepared".
		m.RecoverPendingBuilds()
	}()
	m.logger.Info("build manager started")
	return nil
}

// ensureBuilderImage ensures the builder image is available in the image store.
//
// If BUILDER_IMAGE is set, it checks whether the image is already in the store
// and attempts to pull it from a remote registry if not.
//
// If BUILDER_IMAGE is unset/empty, it builds the image from the embedded Dockerfile
// using Docker, imports the result directly into the OCI layout cache (no docker push),
// and triggers ext4 conversion via ImportLocalImage.
//
// This runs in a background goroutine during startup.
func (m *manager) ensureBuilderImage(ctx context.Context) {
	defer m.builderReady.Store(true)

	if m.config.BuilderImage != "" {
		// Explicit builder image configured - check if already available
		if _, err := m.imageManager.GetImage(ctx, m.config.BuilderImage); err == nil {
			m.logger.Info("builder image already available", "image", m.config.BuilderImage)
			return
		}

		// Not in store - try to pull it from remote registry
		m.logger.Info("pulling builder image", "image", m.config.BuilderImage)
		if _, err := m.imageManager.CreateImage(ctx, images.CreateImageRequest{
			Name: m.config.BuilderImage,
		}); err != nil {
			m.logger.Warn("failed to pull builder image", "image", m.config.BuilderImage, "error", err)
			return
		}
		if err := m.waitForBuilderImageReady(ctx, m.config.BuilderImage); err != nil {
			m.logger.Warn("builder image failed to become ready", "image", m.config.BuilderImage, "error", err)
		}
		return
	}

	// No builder image configured - build from embedded Dockerfile
	m.logger.Info("building builder image from embedded Dockerfile")
	imageRef, err := m.buildBuilderFromDockerfile(ctx)
	if err != nil {
		m.logger.Warn("failed to build builder image", "error", err)
		return
	}
	m.config.BuilderImage = imageRef
	m.logger.Info("builder image ready", "image", imageRef)
}

// buildBuilderFromDockerfile builds the builder image from the embedded Dockerfile
// and imports it into the image store without using docker push.
//
// The flow is:
//  1. Write embedded Dockerfile to a temp directory
//  2. Build with Docker (uses cwd as context for COPY directives)
//  3. Export with docker save to a tarball
//  4. Load tarball with go-containerregistry and write to the shared OCI layout cache
//  5. Call ImportLocalImage to trigger ext4 conversion
//  6. Wait for the image to be ready
//
// This is intended for development; in production, set BUILDER_IMAGE to a pre-built image.
func (m *manager) buildBuilderFromDockerfile(ctx context.Context) (string, error) {
	dockerSocket := m.config.DockerSocket
	if dockerSocket == "" {
		dockerSocket = "/var/run/docker.sock"
	}
	if _, err := os.Stat(dockerSocket); err != nil {
		return "", fmt.Errorf("Docker socket not found at %s: %w", dockerSocket, err)
	}

	dockerEnv := append(os.Environ(), fmt.Sprintf("DOCKER_HOST=unix://%s", dockerSocket))

	// Write embedded Dockerfile to temp dir
	tmpDir, err := os.MkdirTemp("", "hypeman-builder-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dockerfilePath := filepath.Join(tmpDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, builderDockerfile, 0644); err != nil {
		return "", fmt.Errorf("write Dockerfile: %w", err)
	}

	// Build with Docker (context is cwd = repo root in development)
	localTag := fmt.Sprintf("hypeman-builder-tmp:%d", time.Now().Unix())
	m.logger.Info("building builder image with Docker", "tag", localTag)

	buildCmd := exec.CommandContext(ctx, "docker", "build", "-t", localTag, "-f", dockerfilePath, ".")
	buildCmd.Env = dockerEnv
	if output, err := buildCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker build: %s: %w", string(output), err)
	}
	defer func() {
		rmCmd := exec.Command("docker", "rmi", localTag)
		rmCmd.Env = dockerEnv
		rmCmd.Run()
	}()

	// Export image to tarball (avoids docker push)
	tarPath := filepath.Join(tmpDir, "builder.tar")
	saveCmd := exec.CommandContext(ctx, "docker", "save", "-o", tarPath, localTag)
	saveCmd.Env = dockerEnv
	if output, err := saveCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker save: %s: %w", string(output), err)
	}

	// Load tarball as a v1.Image
	img, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		return "", fmt.Errorf("load image tarball: %w", err)
	}

	// Get image digest
	digestHash, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("get image digest: %w", err)
	}
	digest := digestHash.String()   // "sha256:abc123..."
	digestHex := digestHash.Hex      // "abc123..."

	// Write directly to the shared OCI layout cache.
	// This is the same cache used by the image manager's OCI client, so when
	// ImportLocalImage triggers buildImage → pullAndExport, it will find the
	// layers already cached and skip the network pull entirely.
	cacheDir := m.paths.SystemOCICache()
	layoutPath, err := layout.FromPath(cacheDir)
	if err != nil {
		layoutPath, err = layout.Write(cacheDir, empty.Index)
		if err != nil {
			return "", fmt.Errorf("create OCI layout: %w", err)
		}
	}

	if err := layoutPath.AppendImage(img, layout.WithAnnotations(map[string]string{
		"org.opencontainers.image.ref.name": digestHex,
	})); err != nil {
		return "", fmt.Errorf("add image to OCI layout: %w", err)
	}

	m.logger.Info("builder image added to OCI cache", "digest", digest)

	// Import into the image store (triggers async ext4 conversion).
	// The repo includes the registry host so the image reference is consistent
	// with how other images are stored and looked up.
	registryHost := stripRegistryScheme(m.config.RegistryURL)
	repo := registryHost + "/internal/builder"
	reference := "latest"
	imageRef := repo + ":" + reference

	if _, err := m.imageManager.ImportLocalImage(ctx, repo, reference, digest); err != nil {
		return "", fmt.Errorf("import builder image: %w", err)
	}

	// Wait for ext4 conversion to complete
	if err := m.waitForBuilderImageReady(ctx, imageRef); err != nil {
		return "", fmt.Errorf("builder image conversion: %w", err)
	}

	return imageRef, nil
}

// waitForBuilderImageReady polls the image manager until the image is ready.
func (m *manager) waitForBuilderImageReady(ctx context.Context, imageRef string) error {
	const maxAttempts = 240
	const pollInterval = 500 * time.Millisecond

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		img, err := m.imageManager.GetImage(ctx, imageRef)
		if err == nil {
			switch img.Status {
			case images.StatusReady:
				return nil
			case images.StatusFailed:
				return fmt.Errorf("image conversion failed")
			}
		}
		time.Sleep(pollInterval)
	}

	return fmt.Errorf("timeout waiting for builder image after %v", time.Duration(maxAttempts)*pollInterval)
}

// CreateBuild starts a new build job
func (m *manager) CreateBuild(ctx context.Context, req CreateBuildRequest, sourceData []byte) (*Build, error) {
	m.logger.Info("creating build")

	// Apply defaults to build policy
	policy := req.BuildPolicy
	if policy == nil {
		defaultPolicy := DefaultBuildPolicy()
		policy = &defaultPolicy
	} else {
		policy.ApplyDefaults()
	}

	m.createMu.Lock()
	defer m.createMu.Unlock()

	// Generate build ID
	id := cuid2.Generate()

	// Create build metadata
	meta := &buildMetadata{
		ID:        id,
		Status:    StatusQueued,
		Request:   &req,
		CreatedAt: time.Now(),
	}

	// Write initial metadata
	if err := writeMetadata(m.paths, meta); err != nil {
		return nil, fmt.Errorf("write metadata: %w", err)
	}

	// Store source data
	if err := m.storeSource(id, sourceData); err != nil {
		deleteBuild(m.paths, id)
		return nil, fmt.Errorf("store source: %w", err)
	}

	// Generate scoped registry token for this build
	// Token grants per-repo access based on build type:
	// - Regular builds: push to builds/{id}, push to cache/{tenant}, pull from cache/global/{runtime}
	// - Admin builds: push to builds/{id}, push to cache/global/{runtime}
	tokenTTL := time.Duration(policy.TimeoutSeconds) * time.Second
	if tokenTTL < 30*time.Minute {
		tokenTTL = 30 * time.Minute // Minimum 30 minutes
	}

	repoAccess := []RepoPermission{
		{Repo: fmt.Sprintf("builds/%s", id), Scope: "push"},
	}

	// If the Dockerfile uses base images from the internal registry, grant pull access
	for _, baseRepo := range extractInternalBaseImageRepos(req.Dockerfile, m.config.RegistryURL) {
		repoAccess = append(repoAccess, RepoPermission{
			Repo:  baseRepo,
			Scope: "pull",
		})
	}

	if req.IsAdminBuild {
		// Admin build: push access to global cache
		if req.GlobalCacheKey != "" {
			repoAccess = append(repoAccess, RepoPermission{
				Repo:  fmt.Sprintf("cache/global/%s", req.GlobalCacheKey),
				Scope: "push",
			})
		}
	} else {
		// Regular tenant build
		// Pull access to global cache (if runtime specified)
		if req.GlobalCacheKey != "" {
			repoAccess = append(repoAccess, RepoPermission{
				Repo:  fmt.Sprintf("cache/global/%s", req.GlobalCacheKey),
				Scope: "pull",
			})
		}
		// Push access to tenant cache (if cache scope specified)
		if req.CacheScope != "" {
			repoAccess = append(repoAccess, RepoPermission{
				Repo:  fmt.Sprintf("cache/%s", req.CacheScope),
				Scope: "push",
			})
		}
	}

	// Add pull access for base image repos so the builder agent can
	// detect mirrored images via checkImageExistsInRegistry
	dockerfileContent := req.Dockerfile
	if dockerfileContent == "" {
		tarballPath := m.paths.BuildSourceDir(id) + "/source.tar.gz"
		if content, err := ExtractDockerfileFromTarball(tarballPath); err == nil {
			dockerfileContent = content
		}
	}
	if dockerfileContent != "" {
		refs := ParseDockerfileFROMs(dockerfileContent)
		seen := make(map[string]bool)
		for _, ref := range refs {
			repo := ref
			if idx := strings.LastIndex(repo, ":"); idx > 0 {
				repo = repo[:idx]
			}
			if !seen[repo] {
				seen[repo] = true
				repoAccess = append(repoAccess, RepoPermission{Repo: repo, Scope: "pull"})
			}
		}
	}

	registryToken, err := m.tokenGenerator.GenerateToken(id, repoAccess, tokenTTL)
	if err != nil {
		deleteBuild(m.paths, id)
		return nil, fmt.Errorf("generate registry token: %w", err)
	}

	// Write build config for the builder agent
	buildConfig := &BuildConfig{
		JobID:            id,
		BaseImageDigest:  req.BaseImageDigest,
		RegistryURL:      m.config.RegistryURL,
		RegistryToken:    registryToken,
		RegistryInsecure: m.config.RegistryInsecure,
		RegistryCACert:   m.config.RegistryCACert,
		CacheScope:       req.CacheScope,
		SourcePath:       "/src",
		Dockerfile:       req.Dockerfile,
		BuildArgs:        req.BuildArgs,
		Secrets:          req.Secrets,
		TimeoutSeconds:   policy.TimeoutSeconds,
		NetworkMode:      policy.NetworkMode,
		IsAdminBuild:     req.IsAdminBuild,
		GlobalCacheKey:   req.GlobalCacheKey,
	}
	if err := writeBuildConfig(m.paths, id, buildConfig); err != nil {
		deleteBuild(m.paths, id)
		return nil, fmt.Errorf("write build config: %w", err)
	}

	// Enqueue the build
	queuePos := m.queue.Enqueue(id, req, func() {
		m.runBuild(context.Background(), id, req, policy)
	})

	build := meta.toBuild()
	if queuePos > 0 {
		build.QueuePosition = &queuePos
	}

	m.logger.Info("build created", "id", id, "queue_position", queuePos)
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

	// Update status to building
	m.updateStatus(id, StatusBuilding, nil)

	// Create timeout context
	buildCtx, cancel := context.WithTimeout(ctx, time.Duration(policy.TimeoutSeconds)*time.Second)
	defer cancel()

	// Mirror base images to the local registry before launching the VM.
	// BuildKit is configured with our registry as a mirror for docker.io,
	// so pre-cached images will be served locally without pulling from Docker Hub.
	if err := m.mirrorBaseImagesForBuild(buildCtx, id, req); err != nil {
		m.logger.Warn("failed to mirror base images", "id", id, "error", err)
	}

	// Run the build in a builder VM
	result, err := m.executeBuild(buildCtx, id, req, policy)

	duration := time.Since(start)
	durationMS := duration.Milliseconds()

	if err != nil {
		m.logger.Error("build failed", "id", id, "error", err, "duration", duration)
		errMsg := err.Error()
		m.updateBuildComplete(id, StatusFailed, nil, &errMsg, nil, &durationMS)
		if m.metrics != nil {
			m.metrics.RecordBuild(ctx, "failed", duration)
		}
		return
	}

	// Save complete build logs from result.Logs as the authoritative log file.
	// Streamed "log" messages may have dropped lines due to channel overflow,
	// so we overwrite with the complete buffer to ensure no logs are lost.
	if result.Logs != "" {
		if err := writeLog(m.paths, id, []byte(result.Logs)); err != nil {
			m.logger.Warn("failed to save build logs", "id", id, "error", err)
		}
	}

	if !result.Success {
		m.logger.Error("build failed", "id", id, "error", result.Error, "duration", duration)
		m.updateBuildComplete(id, StatusFailed, nil, &result.Error, &result.Provenance, &durationMS)
		if m.metrics != nil {
			m.metrics.RecordBuild(ctx, "failed", duration)
		}
		return
	}

	m.logger.Info("build succeeded", "id", id, "digest", result.ImageDigest, "duration", duration)
	registryHost := stripRegistryScheme(m.config.RegistryURL)
	imageRef := fmt.Sprintf("%s/builds/%s", registryHost, id)

	// Wait for image to be ready before reporting build as complete.
	// This fixes the race condition (KERNEL-863) where build reports "ready"
	// but image conversion hasn't finished yet.
	// Use buildCtx to respect the build timeout during image wait.
	if err := m.waitForImageReady(buildCtx, id); err != nil {
		// Recalculate duration to include image wait time
		duration = time.Since(start)
		durationMS = duration.Milliseconds()
		m.logger.Error("image conversion failed after build", "id", id, "error", err, "duration", duration)
		errMsg := fmt.Sprintf("image conversion failed: %v", err)
		m.updateBuildComplete(id, StatusFailed, nil, &errMsg, &result.Provenance, &durationMS)
		if m.metrics != nil {
			m.metrics.RecordBuild(buildCtx, "failed", duration)
		}
		return
	}

	// Recalculate duration to include image wait time for accurate reporting
	duration = time.Since(start)
	durationMS = duration.Milliseconds()

	m.updateBuildComplete(id, StatusReady, &result.ImageDigest, nil, &result.Provenance, &durationMS)

	// Update with image ref
	if meta, err := readMetadata(m.paths, id); err == nil {
		meta.ImageRef = &imageRef
		writeMetadata(m.paths, meta)
	}

	if m.metrics != nil {
		m.metrics.RecordBuild(buildCtx, "success", duration)
	}
}

// executeBuild runs the build in a builder VM
func (m *manager) executeBuild(ctx context.Context, id string, req CreateBuildRequest, policy *BuildPolicy) (*BuildResult, error) {
	if !m.builderReady.Load() {
		return nil, fmt.Errorf("builder image is being prepared, please retry shortly")
	}

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
	defer m.volumeManager.DeleteVolume(context.Background(), sourceVolID)

	// Create config volume with build.json for the builder agent
	configVolID := fmt.Sprintf("build-config-%s", id)
	configVolPath, err := m.createBuildConfigVolume(id, configVolID)
	if err != nil {
		return nil, fmt.Errorf("create config volume: %w", err)
	}
	defer os.Remove(configVolPath) // Clean up the config disk file

	// Register the config volume with the volume manager
	_, err = m.volumeManager.CreateVolume(ctx, volumes.CreateVolumeRequest{
		Id:     &configVolID,
		Name:   configVolID,
		SizeGb: 1,
	})
	if err != nil {
		// If volume creation fails, try to use the disk file directly
		// by copying it to the expected location
		volPath := m.paths.VolumeData(configVolID)
		if copyErr := copyFile(configVolPath, volPath); copyErr != nil {
			return nil, fmt.Errorf("setup config volume: %w", copyErr)
		}
	} else {
		// Copy our config disk over the empty volume
		volPath := m.paths.VolumeData(configVolID)
		if err := copyFile(configVolPath, volPath); err != nil {
			m.volumeManager.DeleteVolume(context.Background(), configVolID)
			return nil, fmt.Errorf("write config to volume: %w", err)
		}
	}
	defer m.volumeManager.DeleteVolume(context.Background(), configVolID)

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
				Readonly:  false, // Builder needs to write generated Dockerfile
			},
			{
				VolumeID:  configVolID,
				MountPath: "/config",
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
	result, err := m.waitForResult(ctx, id, inst)
	if err != nil {
		return nil, fmt.Errorf("wait for result: %w", err)
	}

	return result, nil
}

// waitForResult waits for the build result from the builder agent via vsock
func (m *manager) waitForResult(ctx context.Context, buildID string, inst *instances.Instance) (*BuildResult, error) {
	// Wait a bit for the VM to start and the builder agent to listen on vsock
	time.Sleep(3 * time.Second)

	// Try to connect to the builder agent with retries
	var conn net.Conn
	var err error

	for attempt := 0; attempt < 30; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		dialer, dialerErr := m.instanceManager.GetVsockDialer(ctx, inst.Id)
		if dialerErr == nil {
			conn, err = dialer.DialVsock(ctx, BuildAgentVsockPort)
			if err == nil {
				break
			}
		} else {
			err = dialerErr
		}

		m.logger.Debug("waiting for builder agent", "attempt", attempt+1, "error", err)
		time.Sleep(2 * time.Second)

		// Check if instance is still running
		current, checkErr := m.instanceManager.GetInstance(ctx, inst.Id)
		if checkErr != nil {
			return nil, fmt.Errorf("check instance: %w", checkErr)
		}
		if current.State == instances.StateStopped || current.State == instances.StateShutdown {
			return &BuildResult{
				Success: false,
				Error:   "builder instance stopped unexpectedly",
			}, nil
		}
	}

	if conn == nil {
		return nil, fmt.Errorf("failed to connect to builder agent after retries: %w", err)
	}
	defer conn.Close()

	m.logger.Info("connected to builder agent", "instance", inst.Id)

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	// Tell the agent we're ready - it may request secrets
	m.logger.Info("sending host_ready to agent", "instance", inst.Id)
	if err := encoder.Encode(VsockMessage{Type: "host_ready"}); err != nil {
		return nil, fmt.Errorf("send host_ready: %w", err)
	}
	m.logger.Info("host_ready sent, waiting for agent messages", "instance", inst.Id)

	// Handle messages from agent until we get the build result
	for {
		// Use a goroutine for decoding so we can respect context cancellation.
		type decodeResult struct {
			response VsockMessage
			err      error
		}
		resultCh := make(chan decodeResult, 1)

		go func() {
			var response VsockMessage
			err := decoder.Decode(&response)
			resultCh <- decodeResult{response: response, err: err}
		}()

		// Wait for either a message or context cancellation
		var dr decodeResult
		select {
		case <-ctx.Done():
			conn.Close()
			<-resultCh
			return nil, ctx.Err()
		case dr = <-resultCh:
			if dr.err != nil {
				return nil, fmt.Errorf("read message: %w", dr.err)
			}
		}

		// Handle message based on type
		m.logger.Info("received message from agent", "type", dr.response.Type, "instance", inst.Id)
		switch dr.response.Type {
		case "get_secrets":
			// Agent is requesting secrets
			m.logger.Info("agent requesting secrets", "instance", inst.Id, "secret_ids", dr.response.SecretIDs)

			// Fetch secrets from provider
			secrets, err := m.secretProvider.GetSecrets(ctx, dr.response.SecretIDs)
			if err != nil {
				m.logger.Error("failed to fetch secrets", "error", err)
				secrets = make(map[string]string)
			}

			// Send secrets response
			if err := encoder.Encode(VsockMessage{Type: "secrets_response", Secrets: secrets}); err != nil {
				return nil, fmt.Errorf("send secrets response: %w", err)
			}
			m.logger.Info("sent secrets to agent", "count", len(secrets), "instance", inst.Id)

		case "log":
			// Stream log line to build log file immediately
			if dr.response.Log != "" {
				if err := appendLog(m.paths, buildID, []byte(dr.response.Log)); err != nil {
					m.logger.Error("failed to append streamed log", "error", err, "build_id", buildID)
				}
			}

		case "build_result":
			// Build completed
			if dr.response.Result == nil {
				return nil, fmt.Errorf("received build_result with nil result")
			}
			return dr.response.Result, nil

		default:
			m.logger.Warn("unexpected message type from agent", "type", dr.response.Type)
		}
	}
}

// updateStatus updates the build status
func (m *manager) updateStatus(id string, status string, err error) {
	meta, readErr := readMetadata(m.paths, id)
	if readErr != nil {
		m.logger.Error("read metadata for status update", "id", id, "error", readErr)
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

	// Notify subscribers of status change
	m.notifyStatusChange(id, status)
}

// updateBuildComplete updates the build with final results
func (m *manager) updateBuildComplete(id string, status string, digest *string, errMsg *string, provenance *BuildProvenance, durationMS *int64) {
	meta, readErr := readMetadata(m.paths, id)
	if readErr != nil {
		m.logger.Error("read metadata for completion", "id", id, "error", readErr)
		return
	}

	// Don't overwrite terminal states - this prevents race conditions where
	// a cancelled build's runBuild goroutine later fails and tries to set "failed"
	if meta.Status == StatusCancelled || meta.Status == StatusReady || meta.Status == StatusFailed {
		m.logger.Debug("skipping status update for already-terminal build",
			"id", id, "current_status", meta.Status, "attempted_status", status)
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

	// Notify subscribers of status change
	m.notifyStatusChange(id, status)
}

// waitForImageReady polls the image manager until the build's image is ready.
// This ensures that when a build reports "ready", the image is actually usable
// for instance creation (fixes KERNEL-863 race condition).
func (m *manager) waitForImageReady(ctx context.Context, id string) error {
	registryHost := stripRegistryScheme(m.config.RegistryURL)
	imageRef := fmt.Sprintf("%s/builds/%s", registryHost, id)

	// Poll for up to 60 seconds (image conversion is typically fast)
	const maxAttempts = 120
	const pollInterval = 500 * time.Millisecond

	m.logger.Debug("waiting for image to be ready", "id", id, "image_ref", imageRef)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		img, err := m.imageManager.GetImage(ctx, imageRef)
		if err == nil {
			switch img.Status {
			case images.StatusReady:
				m.logger.Debug("image is ready", "id", id, "image_ref", imageRef, "attempts", attempt+1)
				return nil
			case images.StatusFailed:
				return fmt.Errorf("image conversion failed")
			case images.StatusPending, images.StatusPulling, images.StatusConverting:
				// Still processing, continue polling
			}
		}
		// Image not found or still processing, wait and retry
		time.Sleep(pollInterval)
	}

	return fmt.Errorf("timeout waiting for image to be ready after %v", time.Duration(maxAttempts)*pollInterval)
}

// subscribeToStatus adds a subscriber channel for status updates on a build
func (m *manager) subscribeToStatus(buildID string, ch chan BuildEvent) {
	m.subscriberMu.Lock()
	defer m.subscriberMu.Unlock()
	m.statusSubscribers[buildID] = append(m.statusSubscribers[buildID], ch)
}

// unsubscribeFromStatus removes a subscriber channel
func (m *manager) unsubscribeFromStatus(buildID string, ch chan BuildEvent) {
	m.subscriberMu.Lock()
	defer m.subscriberMu.Unlock()

	subscribers := m.statusSubscribers[buildID]
	for i, sub := range subscribers {
		if sub == ch {
			m.statusSubscribers[buildID] = append(subscribers[:i], subscribers[i+1:]...)
			break
		}
	}

	// Clean up empty subscriber lists
	if len(m.statusSubscribers[buildID]) == 0 {
		delete(m.statusSubscribers, buildID)
	}
}

// notifyStatusChange broadcasts a status change to all subscribers
func (m *manager) notifyStatusChange(buildID string, status string) {
	m.subscriberMu.RLock()
	defer m.subscriberMu.RUnlock()

	event := BuildEvent{
		Type:      EventTypeStatus,
		Timestamp: time.Now(),
		Status:    status,
	}

	for _, ch := range m.statusSubscribers[buildID] {
		// Non-blocking send - drop if channel is full
		select {
		case ch <- event:
		default:
		}
	}
}

// GetBuild returns a build by ID
func (m *manager) GetBuild(ctx context.Context, id string) (*Build, error) {
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}

	build := meta.toBuild()

	// Add queue position if queued
	if meta.Status == StatusQueued {
		build.QueuePosition = m.queue.GetPosition(id)
	}

	return build, nil
}

// ListBuilds returns all builds
func (m *manager) ListBuilds(ctx context.Context) ([]*Build, error) {
	metas, err := listAllBuilds(m.paths)
	if err != nil {
		return nil, err
	}

	builds := make([]*Build, 0, len(metas))
	for _, meta := range metas {
		build := meta.toBuild()
		if meta.Status == StatusQueued {
			build.QueuePosition = m.queue.GetPosition(meta.ID)
		}
		builds = append(builds, build)
	}

	return builds, nil
}

// CancelBuild cancels a pending build
func (m *manager) CancelBuild(ctx context.Context, id string) error {
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return err
	}

	switch meta.Status {
	case StatusQueued:
		// Remove from queue
		if m.queue.Cancel(id) {
			m.updateStatus(id, StatusCancelled, nil)
			return nil
		}
		return ErrBuildInProgress // Was already picked up

	case StatusBuilding, StatusPushing:
		// Can't cancel a running build easily
		// Would need to terminate the builder instance
		if meta.BuilderInstance != nil {
			m.instanceManager.DeleteInstance(ctx, *meta.BuilderInstance)
		}
		m.updateStatus(id, StatusCancelled, nil)
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

// StreamBuildEvents streams build events (logs, status changes, heartbeats)
func (m *manager) StreamBuildEvents(ctx context.Context, id string, follow bool) (<-chan BuildEvent, error) {
	meta, err := readMetadata(m.paths, id)
	if err != nil {
		return nil, err
	}

	// Create output channel
	out := make(chan BuildEvent, 100)

	// Check if build is already complete
	isComplete := meta.Status == StatusReady || meta.Status == StatusFailed || meta.Status == StatusCancelled

	go func() {
		defer close(out)

		// Create a channel for status updates
		statusChan := make(chan BuildEvent, 10)
		if follow && !isComplete {
			m.subscribeToStatus(id, statusChan)
			defer m.unsubscribeFromStatus(id, statusChan)
		}

		// Stream existing logs using tail
		logPath := m.paths.BuildLog(id)

		// Check if log file exists
		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			// No logs yet - if not following, just return
			if !follow || isComplete {
				return
			}
			// Wait for log file to appear, or for build to complete
			for {
				select {
				case <-ctx.Done():
					return
				case event := <-statusChan:
					select {
					case out <- event:
					case <-ctx.Done():
						return
					}
					// Check if build completed
					if event.Status == StatusReady || event.Status == StatusFailed || event.Status == StatusCancelled {
						return
					}
					// Non-terminal status event - keep waiting for log file
					continue
				case <-time.After(500 * time.Millisecond):
					if _, err := os.Stat(logPath); err == nil {
						break // Log file appeared
					}
					continue
				}
				break
			}
		}

		// Build tail command args
		args := []string{"-n", "+1"} // Start from beginning
		if follow && !isComplete {
			args = append(args, "-f")
		}
		args = append(args, logPath)

		cmd := exec.CommandContext(ctx, "tail", args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			m.logger.Error("create stdout pipe for build logs", "id", id, "error", err)
			return
		}

		if err := cmd.Start(); err != nil {
			m.logger.Error("start tail for build logs", "id", id, "error", err)
			return
		}

		// Ensure tail process is cleaned up on all exit paths to avoid zombie processes.
		// Kill() is safe to call even if the process has already exited.
		// Wait() reaps the process to prevent zombies.
		defer func() {
			cmd.Process.Kill()
			cmd.Wait()
		}()

		// Goroutine to read log lines
		logLines := make(chan string, 100)
		go func() {
			defer close(logLines)
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				select {
				case logLines <- scanner.Text():
				case <-ctx.Done():
					return
				}
			}
		}()

		// Heartbeat ticker (30 seconds)
		heartbeatTicker := time.NewTicker(30 * time.Second)
		defer heartbeatTicker.Stop()

		// Main event loop
		for {
			select {
			case <-ctx.Done():
				return

			case line, ok := <-logLines:
				if !ok {
					// Log stream ended
					return
				}
				event := BuildEvent{
					Type:      EventTypeLog,
					Timestamp: time.Now(),
					Content:   line,
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}

			case event := <-statusChan:
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
				// Check if build completed
				if event.Status == StatusReady || event.Status == StatusFailed || event.Status == StatusCancelled {
					// Give a moment for final logs to come through
					time.Sleep(100 * time.Millisecond)
					return
				}

			case <-heartbeatTicker.C:
				if !follow {
					continue
				}
				event := BuildEvent{
					Type:      EventTypeHeartbeat,
					Timestamp: time.Now(),
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, nil
}

// RecoverPendingBuilds recovers builds that were interrupted on restart
func (m *manager) RecoverPendingBuilds() {
	pending, err := listPendingBuilds(m.paths)
	if err != nil {
		m.logger.Error("list pending builds for recovery", "error", err)
		return
	}

	for _, meta := range pending {
		meta := meta // Shadow loop variable for closure capture
		m.logger.Info("recovering build", "id", meta.ID, "status", meta.Status)

		// Re-enqueue the build
		if meta.Request != nil {
			// Regenerate registry token since the original token may have expired
			// during server downtime. Token TTL is minimum 30 minutes.
			if err := m.refreshBuildToken(meta.ID, meta.Request); err != nil {
				m.logger.Error("failed to refresh registry token for recovered build",
					"id", meta.ID, "error", err)
				// Mark the build as failed since we can't refresh the token
				errMsg := fmt.Sprintf("failed to refresh registry token on recovery: %v", err)
				m.updateBuildComplete(meta.ID, StatusFailed, nil, &errMsg, nil, nil)
				continue
			}

			m.queue.Enqueue(meta.ID, *meta.Request, func() {
				policy := DefaultBuildPolicy()
				if meta.Request.BuildPolicy != nil {
					policy = *meta.Request.BuildPolicy
				}
				m.runBuild(context.Background(), meta.ID, *meta.Request, &policy)
			})
		}
	}

	if len(pending) > 0 {
		m.logger.Info("recovered pending builds", "count", len(pending))
	}
}

// refreshBuildToken regenerates the registry token for a build and updates the config file
func (m *manager) refreshBuildToken(buildID string, req *CreateBuildRequest) error {
	// Read existing build config
	config, err := readBuildConfig(m.paths, buildID)
	if err != nil {
		return fmt.Errorf("read build config: %w", err)
	}

	// Determine token TTL from build policy
	policy := DefaultBuildPolicy()
	if req.BuildPolicy != nil {
		policy = *req.BuildPolicy
		policy.ApplyDefaults()
	}
	tokenTTL := time.Duration(policy.TimeoutSeconds) * time.Second
	if tokenTTL < 30*time.Minute {
		tokenTTL = 30 * time.Minute // Minimum 30 minutes
	}

	// Generate per-repo access list (same logic as CreateBuild)
	repoAccess := []RepoPermission{
		{Repo: fmt.Sprintf("builds/%s", buildID), Scope: "push"},
	}

	// If the Dockerfile uses base images from the internal registry, grant pull access
	for _, baseRepo := range extractInternalBaseImageRepos(req.Dockerfile, m.config.RegistryURL) {
		repoAccess = append(repoAccess, RepoPermission{
			Repo:  baseRepo,
			Scope: "pull",
		})
	}

	if req.IsAdminBuild {
		// Admin build: push access to global cache
		if req.GlobalCacheKey != "" {
			repoAccess = append(repoAccess, RepoPermission{
				Repo:  fmt.Sprintf("cache/global/%s", req.GlobalCacheKey),
				Scope: "push",
			})
		}
	} else {
		// Regular tenant build
		if req.GlobalCacheKey != "" {
			repoAccess = append(repoAccess, RepoPermission{
				Repo:  fmt.Sprintf("cache/global/%s", req.GlobalCacheKey),
				Scope: "pull",
			})
		}
		if req.CacheScope != "" {
			repoAccess = append(repoAccess, RepoPermission{
				Repo:  fmt.Sprintf("cache/%s", req.CacheScope),
				Scope: "push",
			})
		}
	}

	// Add pull access for base image repos so the builder agent can
	// detect mirrored images via checkImageExistsInRegistry
	dockerfileContent := req.Dockerfile
	if dockerfileContent == "" {
		tarballPath := m.paths.BuildSourceDir(buildID) + "/source.tar.gz"
		if content, err := ExtractDockerfileFromTarball(tarballPath); err == nil {
			dockerfileContent = content
		}
	}
	if dockerfileContent != "" {
		refs := ParseDockerfileFROMs(dockerfileContent)
		seen := make(map[string]bool)
		for _, ref := range refs {
			repo := ref
			if idx := strings.LastIndex(repo, ":"); idx > 0 {
				repo = repo[:idx]
			}
			if !seen[repo] {
				seen[repo] = true
				repoAccess = append(repoAccess, RepoPermission{Repo: repo, Scope: "pull"})
			}
		}
	}

	// Generate fresh registry token
	registryToken, err := m.tokenGenerator.GenerateToken(buildID, repoAccess, tokenTTL)
	if err != nil {
		return fmt.Errorf("generate registry token: %w", err)
	}

	// Update config with new token
	config.RegistryToken = registryToken

	// Write updated config back to disk
	if err := writeBuildConfig(m.paths, buildID, config); err != nil {
		return fmt.Errorf("write build config: %w", err)
	}

	m.logger.Debug("refreshed registry token for recovered build", "id", buildID)
	return nil
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

// createBuildConfigVolume creates an ext4 disk containing the build.json config file
// Returns the path to the disk file
func (m *manager) createBuildConfigVolume(buildID, volID string) (string, error) {
	// Read the build config
	configPath := m.paths.BuildConfig(buildID)
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("read build config: %w", err)
	}

	// Create temp directory with config file
	tmpDir, err := os.MkdirTemp("", "hypeman-build-config-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Write build.json to temp directory
	buildJSONPath := filepath.Join(tmpDir, "build.json")
	if err := os.WriteFile(buildJSONPath, configData, 0644); err != nil {
		return "", fmt.Errorf("write build.json: %w", err)
	}

	// Also write a metadata file for debugging
	metadata := map[string]interface{}{
		"build_id":   buildID,
		"created_at": time.Now().Format(time.RFC3339),
	}
	metadataData, _ := json.MarshalIndent(metadata, "", "  ")
	metadataPath := filepath.Join(tmpDir, "metadata.json")
	os.WriteFile(metadataPath, metadataData, 0644)

	// Create ext4 disk from the directory
	diskPath := filepath.Join(os.TempDir(), fmt.Sprintf("build-config-%s.ext4", buildID))
	_, err = images.ExportRootfs(tmpDir, diskPath, images.FormatExt4)
	if err != nil {
		return "", fmt.Errorf("create config disk: %w", err)
	}

	return diskPath, nil
}

// extractInternalBaseImageRepos parses the Dockerfile's FROM lines and returns
// all repository paths that reference the internal registry. Returns nil
// if no base images reference the internal registry.
func extractInternalBaseImageRepos(dockerfile, registryURL string) []string {
	if dockerfile == "" {
		return nil
	}

	registryHost := stripRegistryScheme(registryURL)
	seen := make(map[string]bool)
	var repos []string

	scanner := bufio.NewScanner(strings.NewReader(dockerfile))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		upper := strings.ToUpper(line)
		if !strings.HasPrefix(upper, "FROM ") {
			continue
		}

		// Parse: FROM [--platform=...] image[:tag|@digest] [AS name]
		parts := strings.Fields(line)
		imageIdx := 1
		for imageIdx < len(parts) && strings.HasPrefix(parts[imageIdx], "--") {
			imageIdx++
		}
		if imageIdx >= len(parts) {
			continue
		}

		imageRef := parts[imageIdx]
		if strings.ToLower(imageRef) == "scratch" {
			continue
		}

		// Check if the image references the internal registry
		if !strings.HasPrefix(imageRef, registryHost+"/") {
			continue
		}

		// Strip the registry host to get the repo path, then strip digest and tag.
		// An image ref can have both: registry/org/img:v1@sha256:abc123
		repo := strings.TrimPrefix(imageRef, registryHost+"/")
		if idx := strings.LastIndex(repo, "@"); idx != -1 {
			repo = repo[:idx]
		}
		if idx := strings.LastIndex(repo, ":"); idx != -1 {
			repo = repo[:idx]
		}

		if !seen[repo] {
			seen[repo] = true
			repos = append(repos, repo)
		}
	}

	return repos
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
