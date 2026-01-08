package builds

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nrednav/cuid2"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/volumes"
	"go.opentelemetry.io/otel/metric"
)

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

	// DefaultTimeout is the default build timeout in seconds
	DefaultTimeout int

	// RegistrySecret is the secret used to sign registry access tokens
	// This should be the same secret used by the registry middleware
	RegistrySecret string
}

// DefaultConfig returns the default build manager configuration
func DefaultConfig() Config {
	return Config{
		MaxConcurrentBuilds: 2,
		BuilderImage:        "hypeman/builder:latest",
		RegistryURL:         "localhost:8080",
		DefaultTimeout:      600, // 10 minutes
	}
}

type manager struct {
	config          Config
	paths           *paths.Paths
	queue           *BuildQueue
	instanceManager instances.Manager
	volumeManager   volumes.Manager
	secretProvider  SecretProvider
	tokenGenerator  *RegistryTokenGenerator
	logger          *slog.Logger
	metrics         *Metrics
	createMu        sync.Mutex

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

	// Recover any pending builds from disk
	m.RecoverPendingBuilds()

	return m, nil
}

// Start starts the build manager's background services
func (m *manager) Start(ctx context.Context) error {
	// Note: We no longer use a global vsock listener.
	// Instead, we connect TO each builder VM's vsock socket directly.
	// This follows the Cloud Hypervisor vsock pattern where host initiates connections.
	m.logger.Info("build manager started")
	return nil
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
	// Token grants push access to the build output repo and cache repo
	allowedRepos := []string{fmt.Sprintf("builds/%s", id)}
	if req.CacheScope != "" {
		allowedRepos = append(allowedRepos, fmt.Sprintf("cache/%s", req.CacheScope))
	}
	tokenTTL := time.Duration(policy.TimeoutSeconds) * time.Second
	if tokenTTL < 30*time.Minute {
		tokenTTL = 30 * time.Minute // Minimum 30 minutes
	}
	registryToken, err := m.tokenGenerator.GeneratePushToken(id, allowedRepos, tokenTTL)
	if err != nil {
		deleteBuild(m.paths, id)
		return nil, fmt.Errorf("generate registry token: %w", err)
	}

	// Write build config for the builder agent
	buildConfig := &BuildConfig{
		JobID:           id,
		BaseImageDigest: req.BaseImageDigest,
		RegistryURL:     m.config.RegistryURL,
		RegistryToken:   registryToken,
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

	// Save build logs (regardless of success/failure)
	if result.Logs != "" {
		if err := appendLog(m.paths, id, []byte(result.Logs)); err != nil {
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
	imageRef := fmt.Sprintf("%s/builds/%s", m.config.RegistryURL, id)
	m.updateBuildComplete(id, StatusReady, &result.ImageDigest, nil, &result.Provenance, &durationMS)

	// Update with image ref
	if meta, err := readMetadata(m.paths, id); err == nil {
		meta.ImageRef = &imageRef
		writeMetadata(m.paths, meta)
	}

	if m.metrics != nil {
		m.metrics.RecordBuild(ctx, "success", duration)
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
	result, err := m.waitForResult(ctx, inst)
	if err != nil {
		return nil, fmt.Errorf("wait for result: %w", err)
	}

	return result, nil
}

// waitForResult waits for the build result from the builder agent via vsock
func (m *manager) waitForResult(ctx context.Context, inst *instances.Instance) (*BuildResult, error) {
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

		conn, err = m.dialBuilderVsock(inst.VsockSocket)
		if err == nil {
			break
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
	if err := encoder.Encode(VsockMessage{Type: "host_ready"}); err != nil {
		return nil, fmt.Errorf("send host_ready: %w", err)
	}

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
		switch dr.response.Type {
		case "get_secrets":
			// Agent is requesting secrets
			m.logger.Debug("agent requesting secrets", "instance", inst.Id, "secret_ids", dr.response.SecretIDs)

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
			m.logger.Debug("sent secrets to agent", "count", len(secrets))

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

// dialBuilderVsock connects to a builder VM's vsock socket using Cloud Hypervisor's handshake
func (m *manager) dialBuilderVsock(vsockSocketPath string) (net.Conn, error) {
	// Connect to the Cloud Hypervisor vsock Unix socket
	conn, err := net.DialTimeout("unix", vsockSocketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial vsock socket %s: %w", vsockSocketPath, err)
	}

	// Set deadline for handshake
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	// Perform Cloud Hypervisor vsock handshake
	// Format: "CONNECT <port>\n" -> "OK <port>\n"
	handshakeCmd := fmt.Sprintf("CONNECT %d\n", BuildAgentVsockPort)
	if _, err := conn.Write([]byte(handshakeCmd)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send vsock handshake: %w", err)
	}

	// Read handshake response
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read vsock handshake response: %w", err)
	}

	// Clear deadline after successful handshake
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clear deadline: %w", err)
	}

	response = strings.TrimSpace(response)
	if !strings.HasPrefix(response, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake failed: %s", response)
	}

	return &bufferedConn{Conn: conn, reader: reader}, nil
}

// bufferedConn wraps a net.Conn with a bufio.Reader to ensure any buffered
// data from the handshake is properly drained before reading from the connection
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
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
				cmd.Process.Kill()
				return

			case line, ok := <-logLines:
				if !ok {
					// Log stream ended - wait for tail to exit
					cmd.Wait()
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
					cmd.Process.Kill()
					return
				}

			case event := <-statusChan:
				select {
				case out <- event:
				case <-ctx.Done():
					cmd.Process.Kill()
					return
				}
				// Check if build completed
				if event.Status == StatusReady || event.Status == StatusFailed || event.Status == StatusCancelled {
					// Give a moment for final logs to come through
					time.Sleep(100 * time.Millisecond)
					cmd.Process.Kill()
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
					cmd.Process.Kill()
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

	// Generate allowed repos list
	allowedRepos := []string{fmt.Sprintf("builds/%s", buildID)}
	if req.CacheScope != "" {
		allowedRepos = append(allowedRepos, fmt.Sprintf("cache/%s", req.CacheScope))
	}

	// Generate fresh registry token
	registryToken, err := m.tokenGenerator.GeneratePushToken(buildID, allowedRepos, tokenTTL)
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
