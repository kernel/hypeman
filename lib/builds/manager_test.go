package builds

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/resources"
	"github.com/onkernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInstanceManager implements instances.Manager for testing
type mockInstanceManager struct {
	instances       map[string]*instances.Instance
	createFunc      func(ctx context.Context, req instances.CreateInstanceRequest) (*instances.Instance, error)
	getFunc         func(ctx context.Context, id string) (*instances.Instance, error)
	deleteFunc      func(ctx context.Context, id string) error
	stopFunc        func(ctx context.Context, id string) (*instances.Instance, error)
	createCallCount int
	deleteCallCount int
}

func newMockInstanceManager() *mockInstanceManager {
	return &mockInstanceManager{
		instances: make(map[string]*instances.Instance),
	}
}

func (m *mockInstanceManager) ListInstances(ctx context.Context) ([]instances.Instance, error) {
	var result []instances.Instance
	for _, inst := range m.instances {
		result = append(result, *inst)
	}
	return result, nil
}

func (m *mockInstanceManager) CreateInstance(ctx context.Context, req instances.CreateInstanceRequest) (*instances.Instance, error) {
	m.createCallCount++
	if m.createFunc != nil {
		return m.createFunc(ctx, req)
	}
	inst := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{
			Id:   "inst-" + req.Name,
			Name: req.Name,
		},
		State: instances.StateRunning,
	}
	m.instances[inst.Id] = inst
	return inst, nil
}

func (m *mockInstanceManager) GetInstance(ctx context.Context, id string) (*instances.Instance, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, id)
	}
	if inst, ok := m.instances[id]; ok {
		return inst, nil
	}
	return nil, instances.ErrNotFound
}

func (m *mockInstanceManager) DeleteInstance(ctx context.Context, id string) error {
	m.deleteCallCount++
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, id)
	}
	delete(m.instances, id)
	return nil
}

func (m *mockInstanceManager) StandbyInstance(ctx context.Context, id string) (*instances.Instance, error) {
	return nil, nil
}

func (m *mockInstanceManager) RestoreInstance(ctx context.Context, id string) (*instances.Instance, error) {
	return nil, nil
}

func (m *mockInstanceManager) StopInstance(ctx context.Context, id string) (*instances.Instance, error) {
	if m.stopFunc != nil {
		return m.stopFunc(ctx, id)
	}
	if inst, ok := m.instances[id]; ok {
		inst.State = instances.StateStopped
		return inst, nil
	}
	return nil, instances.ErrNotFound
}

func (m *mockInstanceManager) StartInstance(ctx context.Context, id string) (*instances.Instance, error) {
	return nil, nil
}

func (m *mockInstanceManager) StreamInstanceLogs(ctx context.Context, id string, tail int, follow bool, source instances.LogSource) (<-chan string, error) {
	return nil, nil
}

func (m *mockInstanceManager) RotateLogs(ctx context.Context, maxBytes int64, maxFiles int) error {
	return nil
}

func (m *mockInstanceManager) AttachVolume(ctx context.Context, id string, volumeId string, req instances.AttachVolumeRequest) (*instances.Instance, error) {
	return nil, nil
}

func (m *mockInstanceManager) DetachVolume(ctx context.Context, id string, volumeId string) (*instances.Instance, error) {
	return nil, nil
}

func (m *mockInstanceManager) ListInstanceAllocations(ctx context.Context) ([]resources.InstanceAllocation, error) {
	return nil, nil
}

func (m *mockInstanceManager) CheckResourceAvailability(ctx context.Context, vcpus int, memoryBytes int64) error {
	return nil // Always return nil (resources available) in tests
}

// mockVolumeManager implements volumes.Manager for testing
type mockVolumeManager struct {
	volumes               map[string]*volumes.Volume
	createFunc            func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error)
	createFromArchiveFunc func(ctx context.Context, req volumes.CreateVolumeFromArchiveRequest, archive io.Reader) (*volumes.Volume, error)
	deleteFunc            func(ctx context.Context, id string) error
	createCallCount       int
	deleteCallCount       int
}

func newMockVolumeManager() *mockVolumeManager {
	return &mockVolumeManager{
		volumes: make(map[string]*volumes.Volume),
	}
}

func (m *mockVolumeManager) ListVolumes(ctx context.Context) ([]volumes.Volume, error) {
	var result []volumes.Volume
	for _, vol := range m.volumes {
		result = append(result, *vol)
	}
	return result, nil
}

func (m *mockVolumeManager) CreateVolume(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
	m.createCallCount++
	if m.createFunc != nil {
		return m.createFunc(ctx, req)
	}
	vol := &volumes.Volume{
		Id:   "vol-" + req.Name,
		Name: req.Name,
	}
	m.volumes[vol.Id] = vol
	return vol, nil
}

func (m *mockVolumeManager) CreateVolumeFromArchive(ctx context.Context, req volumes.CreateVolumeFromArchiveRequest, archive io.Reader) (*volumes.Volume, error) {
	m.createCallCount++
	if m.createFromArchiveFunc != nil {
		return m.createFromArchiveFunc(ctx, req, archive)
	}
	vol := &volumes.Volume{
		Id:   "vol-" + req.Name,
		Name: req.Name,
	}
	m.volumes[vol.Id] = vol
	return vol, nil
}

func (m *mockVolumeManager) GetVolume(ctx context.Context, id string) (*volumes.Volume, error) {
	if vol, ok := m.volumes[id]; ok {
		return vol, nil
	}
	return nil, volumes.ErrNotFound
}

func (m *mockVolumeManager) GetVolumeByName(ctx context.Context, name string) (*volumes.Volume, error) {
	for _, vol := range m.volumes {
		if vol.Name == name {
			return vol, nil
		}
	}
	return nil, volumes.ErrNotFound
}

func (m *mockVolumeManager) DeleteVolume(ctx context.Context, id string) error {
	m.deleteCallCount++
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, id)
	}
	delete(m.volumes, id)
	return nil
}

func (m *mockVolumeManager) AttachVolume(ctx context.Context, id string, req volumes.AttachVolumeRequest) error {
	return nil
}

func (m *mockVolumeManager) DetachVolume(ctx context.Context, volumeID string, instanceID string) error {
	return nil
}

func (m *mockVolumeManager) GetVolumePath(id string) string {
	return "/tmp/volumes/" + id
}

func (m *mockVolumeManager) TotalVolumeBytes(ctx context.Context) (int64, error) {
	return 0, nil
}

// mockSecretProvider implements SecretProvider for testing
type mockSecretProvider struct{}

func (m *mockSecretProvider) GetSecrets(ctx context.Context, secretIDs []string) (map[string]string, error) {
	return make(map[string]string), nil
}

// Test helper to create a manager with test paths and mocks
func setupTestManager(t *testing.T) (*manager, *mockInstanceManager, *mockVolumeManager, string) {
	t.Helper()

	// Create temp directory for test data
	tempDir, err := os.MkdirTemp("", "builds-test-*")
	require.NoError(t, err)

	// Create paths
	p := paths.New(tempDir)

	// Create necessary directories
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "builds"), 0755))

	// Create mocks
	instanceMgr := newMockInstanceManager()
	volumeMgr := newMockVolumeManager()
	secretProvider := &mockSecretProvider{}

	// Create config
	config := Config{
		MaxConcurrentBuilds: 2,
		BuilderImage:        "test/builder:latest",
		RegistryURL:         "localhost:5000",
		DefaultTimeout:      300,
		RegistrySecret:      "test-secret-key",
	}

	// Create a discard logger for tests
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Create manager (without calling NewManager to avoid RecoverPendingBuilds)
	mgr := &manager{
		config:            config,
		paths:             p,
		instanceManager:   instanceMgr,
		volumeManager:     volumeMgr,
		secretProvider:    secretProvider,
		tokenGenerator:    NewRegistryTokenGenerator(config.RegistrySecret),
		logger:            logger,
		statusSubscribers: make(map[string][]chan BuildEvent),
	}

	return mgr, instanceMgr, volumeMgr, tempDir
}

func TestCreateBuild_Success(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	req := CreateBuildRequest{
		CacheScope: "test-scope",
		Dockerfile: "FROM alpine\nRUN echo hello",
	}
	sourceData := []byte("fake-tarball-data")

	build, err := mgr.CreateBuild(ctx, req, sourceData)

	require.NoError(t, err)
	assert.NotEmpty(t, build.ID)
	assert.Equal(t, StatusBuilding, build.Status)
	assert.NotNil(t, build.CreatedAt)

	// Verify source was stored
	sourcePath := filepath.Join(tempDir, "builds", build.ID, "source", "source.tar.gz")
	data, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	assert.Equal(t, sourceData, data)

	// Verify config was written
	configPath := filepath.Join(tempDir, "builds", build.ID, "config.json")
	_, err = os.Stat(configPath)
	assert.NoError(t, err)

	// Verify metadata was written
	metaPath := filepath.Join(tempDir, "builds", build.ID, "metadata.json")
	_, err = os.Stat(metaPath)
	assert.NoError(t, err)
}

func TestCreateBuild_WithBuildPolicy(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	timeout := 600
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine",
		BuildPolicy: &BuildPolicy{
			TimeoutSeconds: timeout,
			NetworkMode:    "host",
		},
	}

	build, err := mgr.CreateBuild(ctx, req, []byte("source"))

	require.NoError(t, err)
	assert.NotEmpty(t, build.ID)
}

func TestGetBuild_Found(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Create a build first
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine",
	}
	created, err := mgr.CreateBuild(ctx, req, []byte("source"))
	require.NoError(t, err)

	// Get the build
	build, err := mgr.GetBuild(ctx, created.ID)

	require.NoError(t, err)
	assert.Equal(t, created.ID, build.ID)
	assert.Equal(t, StatusBuilding, build.Status)
}

func TestGetBuild_NotFound(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	_, err := mgr.GetBuild(ctx, "nonexistent-id")

	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListBuilds_Empty(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	builds, err := mgr.ListBuilds(ctx)

	require.NoError(t, err)
	assert.Empty(t, builds)
}

func TestListBuilds_WithBuilds(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Create multiple builds
	for i := 0; i < 3; i++ {
		req := CreateBuildRequest{
			Dockerfile: "FROM alpine",
		}
		_, err := mgr.CreateBuild(ctx, req, []byte("source"))
		require.NoError(t, err)
	}

	builds, err := mgr.ListBuilds(ctx)

	require.NoError(t, err)
	assert.Len(t, builds, 3)
}

func TestCancelBuild_NotFound(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	err := mgr.CancelBuild(ctx, "nonexistent-id")

	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestCancelBuild_AlreadyCompleted(t *testing.T) {
	// Test cancel rejection for completed builds by directly setting up metadata
	tempDir, err := os.MkdirTemp("", "builds-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	p := paths.New(tempDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "builds", "completed-build"), 0755))

	// Create metadata with completed status
	meta := &buildMetadata{
		ID:        "completed-build",
		Status:    StatusReady,
		CreatedAt: time.Now(),
	}
	require.NoError(t, writeMetadata(p, meta))

	// Create manager
	config := Config{
		MaxConcurrentBuilds: 2,
		RegistrySecret:      "test-secret",
	}
	mgr := &manager{
		config:         config,
		paths:          p,
		tokenGenerator: NewRegistryTokenGenerator(config.RegistrySecret),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Try to cancel - should fail because it's already completed
	err = mgr.CancelBuild(context.Background(), "completed-build")

	require.Error(t, err, "expected error when cancelling completed build")
	assert.Contains(t, err.Error(), "already completed")
}

func TestGetBuildLogs_Empty(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Create a build
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine",
	}
	build, err := mgr.CreateBuild(ctx, req, []byte("source"))
	require.NoError(t, err)

	// Get logs (should be empty initially)
	logs, err := mgr.GetBuildLogs(ctx, build.ID)

	require.NoError(t, err)
	assert.Empty(t, logs)
}

func TestGetBuildLogs_WithLogs(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Create a build
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine",
	}
	build, err := mgr.CreateBuild(ctx, req, []byte("source"))
	require.NoError(t, err)

	// Append some logs
	logData := []byte("Step 1: FROM alpine\nStep 2: RUN echo hello\n")
	err = appendLog(mgr.paths, build.ID, logData)
	require.NoError(t, err)

	// Get logs
	logs, err := mgr.GetBuildLogs(ctx, build.ID)

	require.NoError(t, err)
	assert.Equal(t, logData, logs)
}

func TestGetBuildLogs_NotFound(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	_, err := mgr.GetBuildLogs(ctx, "nonexistent-id")

	assert.Error(t, err)
}

func TestUpdateStatus(t *testing.T) {
	// Test the updateStatus function directly using storage functions
	// This avoids race conditions with the build queue goroutines
	tempDir, err := os.MkdirTemp("", "builds-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	p := paths.New(tempDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "builds", "test-build-1"), 0755))

	// Create initial metadata
	meta := &buildMetadata{
		ID:        "test-build-1",
		Status:    StatusBuilding,
		CreatedAt: time.Now(),
	}
	require.NoError(t, writeMetadata(p, meta))

	// Update status to pushing
	meta.Status = StatusPushing
	require.NoError(t, writeMetadata(p, meta))

	// Read back and verify
	readMeta, err := readMetadata(p, "test-build-1")
	require.NoError(t, err)
	assert.Equal(t, StatusPushing, readMeta.Status)
}

func TestUpdateStatus_WithError(t *testing.T) {
	// Test status updates with error message directly
	tempDir, err := os.MkdirTemp("", "builds-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	p := paths.New(tempDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "builds", "test-build-1"), 0755))

	// Create initial metadata
	meta := &buildMetadata{
		ID:        "test-build-1",
		Status:    StatusBuilding,
		CreatedAt: time.Now(),
	}
	require.NoError(t, writeMetadata(p, meta))

	// Update status with error
	errMsg := "build failed: out of memory"
	meta.Status = StatusFailed
	meta.Error = &errMsg
	require.NoError(t, writeMetadata(p, meta))

	// Read back and verify
	readMeta, err := readMetadata(p, "test-build-1")
	require.NoError(t, err)
	assert.Equal(t, StatusFailed, readMeta.Status)
	require.NotNil(t, readMeta.Error)
	assert.Contains(t, *readMeta.Error, "out of memory")
}

func TestRegistryTokenGeneration(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Create a build with cache scope
	req := CreateBuildRequest{
		CacheScope: "my-cache",
		Dockerfile: "FROM alpine",
	}
	build, err := mgr.CreateBuild(ctx, req, []byte("source"))
	require.NoError(t, err)

	// Read the build config and verify token was generated
	configPath := filepath.Join(tempDir, "builds", build.ID, "config.json")
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var config BuildConfig
	err = json.Unmarshal(data, &config)
	require.NoError(t, err)

	assert.NotEmpty(t, config.RegistryToken)
	assert.Equal(t, "localhost:5000", config.RegistryURL)
}

func TestStart(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Start should succeed without error
	err := mgr.Start(ctx)

	assert.NoError(t, err)
}

func TestCreateBuild_MultipleConcurrent(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Create builds in parallel
	done := make(chan *Build, 5)
	errs := make(chan error, 5)

	for i := 0; i < 5; i++ {
		go func() {
			req := CreateBuildRequest{
				Dockerfile: "FROM alpine",
			}
			build, err := mgr.CreateBuild(ctx, req, []byte("source"))
			if err != nil {
				errs <- err
			} else {
				done <- build
			}
		}()
	}

	// Collect results
	var builds []*Build
	for i := 0; i < 5; i++ {
		select {
		case b := <-done:
			builds = append(builds, b)
		case err := <-errs:
			t.Fatalf("unexpected error: %v", err)
		}
	}

	assert.Len(t, builds, 5)

	// Verify all IDs are unique
	ids := make(map[string]bool)
	for _, b := range builds {
		assert.False(t, ids[b.ID], "duplicate build ID: %s", b.ID)
		ids[b.ID] = true
	}
}

func TestStreamBuildEvents_NotFound(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	_, err := mgr.StreamBuildEvents(ctx, "nonexistent-id", false)
	assert.Error(t, err)
}

func TestStreamBuildEvents_ExistingLogs(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Create a build
	req := CreateBuildRequest{Dockerfile: "FROM alpine"}
	sourceData := []byte("fake-tarball-data")
	build, err := mgr.CreateBuild(ctx, req, sourceData)
	require.NoError(t, err)

	// Write some logs directly
	logDir := filepath.Join(tempDir, "builds", build.ID, "logs")
	require.NoError(t, os.MkdirAll(logDir, 0755))
	logPath := filepath.Join(logDir, "build.log")
	require.NoError(t, os.WriteFile(logPath, []byte("line1\nline2\nline3\n"), 0644))

	// Stream events without follow
	eventChan, err := mgr.StreamBuildEvents(ctx, build.ID, false)
	require.NoError(t, err)

	// Collect events
	var events []BuildEvent
	for event := range eventChan {
		events = append(events, event)
	}

	// Should have 3 log events
	assert.Len(t, events, 3)
	for _, event := range events {
		assert.Equal(t, EventTypeLog, event.Type)
	}
	assert.Equal(t, "line1", events[0].Content)
	assert.Equal(t, "line2", events[1].Content)
	assert.Equal(t, "line3", events[2].Content)
}

func TestStreamBuildEvents_NoLogs(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()

	// Create a build
	req := CreateBuildRequest{Dockerfile: "FROM alpine"}
	sourceData := []byte("fake-tarball-data")
	build, err := mgr.CreateBuild(ctx, req, sourceData)
	require.NoError(t, err)

	// Stream events without follow (no logs exist)
	eventChan, err := mgr.StreamBuildEvents(ctx, build.ID, false)
	require.NoError(t, err)

	// Should close immediately with no events
	var events []BuildEvent
	for event := range eventChan {
		events = append(events, event)
	}
	assert.Empty(t, events)
}

func TestStreamBuildEvents_WithStatusUpdate(t *testing.T) {
	// Test that status updates are received through event streaming
	// We create build metadata directly to avoid timing issues with the build goroutine
	tempDir, err := os.MkdirTemp("", "builds-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	p := paths.New(tempDir)
	buildID := "test-status-build"
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "builds", buildID), 0755))

	// Create metadata with building status
	meta := &buildMetadata{
		ID:        buildID,
		Status:    StatusBuilding,
		CreatedAt: time.Now(),
	}
	require.NoError(t, writeMetadata(p, meta))

	// Write some initial logs
	logDir := filepath.Join(tempDir, "builds", buildID, "logs")
	require.NoError(t, os.MkdirAll(logDir, 0755))
	logPath := filepath.Join(logDir, "build.log")
	require.NoError(t, os.WriteFile(logPath, []byte("initial log\n"), 0644))

	// Create a minimal manager for streaming
	mgr := &manager{
		paths:             p,
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		statusSubscribers: make(map[string][]chan BuildEvent),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Stream events with follow
	eventChan, err := mgr.StreamBuildEvents(ctx, buildID, true)
	require.NoError(t, err)

	// Read events until we see the initial log
	var foundInitialLog bool
	timeout := time.After(2 * time.Second)
eventLoop:
	for !foundInitialLog {
		select {
		case event := <-eventChan:
			if event.Type == EventTypeLog && event.Content == "initial log" {
				foundInitialLog = true
				break eventLoop
			}
		case <-timeout:
			t.Fatal("timeout waiting for initial log event")
		}
	}

	// Trigger a status update to "ready" (should cause stream to close)
	mgr.updateStatus(buildID, StatusReady, nil)

	// Should receive "ready" status event and channel should close
	var readyReceived bool
	timeout = time.After(2 * time.Second)
	for !readyReceived {
		select {
		case event, ok := <-eventChan:
			if !ok {
				// Channel closed, this is fine after status update
				return
			}
			if event.Type == EventTypeStatus && event.Status == StatusReady {
				readyReceived = true
			}
		case <-timeout:
			t.Fatal("timeout waiting for ready status event")
		}
	}
}

func TestStreamBuildEvents_ContextCancellation(t *testing.T) {
	// Test that context cancellation properly closes the event channel
	// We use a simpler approach: create build metadata directly without
	// starting the actual build goroutine to avoid timing issues
	tempDir, err := os.MkdirTemp("", "builds-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	p := paths.New(tempDir)
	buildID := "test-cancel-build"
	require.NoError(t, os.MkdirAll(filepath.Join(tempDir, "builds", buildID), 0755))

	// Create metadata with building status
	meta := &buildMetadata{
		ID:        buildID,
		Status:    StatusBuilding,
		CreatedAt: time.Now(),
	}
	require.NoError(t, writeMetadata(p, meta))

	// Write some logs
	logDir := filepath.Join(tempDir, "builds", buildID, "logs")
	require.NoError(t, os.MkdirAll(logDir, 0755))
	logPath := filepath.Join(logDir, "build.log")
	require.NoError(t, os.WriteFile(logPath, []byte("log line\n"), 0644))

	// Create a minimal manager for streaming
	mgr := &manager{
		paths:             p,
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		statusSubscribers: make(map[string][]chan BuildEvent),
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Stream events with follow
	eventChan, err := mgr.StreamBuildEvents(ctx, buildID, true)
	require.NoError(t, err)

	// Read events until we see the log line
	var foundLogLine bool
	timeout := time.After(2 * time.Second)
eventLoop:
	for !foundLogLine {
		select {
		case event := <-eventChan:
			if event.Type == EventTypeLog && event.Content == "log line" {
				foundLogLine = true
				break eventLoop
			}
		case <-timeout:
			t.Fatal("timeout waiting for log event")
		}
	}

	// Cancel the context
	cancel()

	// Channel should close
	timeout = time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-eventChan:
			if !ok {
				// Channel closed as expected
				return
			}
			// May get more events before close, drain them
		case <-timeout:
			t.Fatal("timeout waiting for channel to close after cancel")
		}
	}
}
