package builds

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoopPublisher_ReturnsLocalRefUnchanged(t *testing.T) {
	ref, err := noopPublisher{}.Publish(context.Background(), "builds/abc123", "sha256:deadbeef")
	require.NoError(t, err)
	assert.Equal(t, "builds/abc123", ref)
}

func TestNewBuildPublisher_DisabledByDefault(t *testing.T) {
	p, err := newBuildPublisher(PublishConfig{}, "localhost:5000", NewRegistryTokenGenerator("test"))
	require.NoError(t, err)
	assert.IsType(t, noopPublisher{}, p)
}

func TestNewBuildPublisher_ValidatesConfig(t *testing.T) {
	tokenGen := NewRegistryTokenGenerator("test")

	_, err := newBuildPublisher(PublishConfig{RepositoryPrefix: "team/builds"}, "localhost:5000", tokenGen)
	require.ErrorContains(t, err, "registry")

	_, err = newBuildPublisher(PublishConfig{CredentialsFile: "/creds.json"}, "localhost:5000", tokenGen)
	require.ErrorContains(t, err, "registry")

	_, err = newBuildPublisher(PublishConfig{Registry: "registry.example.com"}, "localhost:5000", tokenGen)
	require.ErrorContains(t, err, "repository_prefix")
}

func TestPublishConfig_Enabled(t *testing.T) {
	assert.False(t, PublishConfig{}.Enabled())
	assert.True(t, PublishConfig{Registry: "registry.example.com"}.Enabled())
}

// staticVsockDialer returns a fixed connection, letting a test play the
// builder-agent side of the vsock protocol over net.Pipe.
type staticVsockDialer struct {
	conn net.Conn
}

func (d staticVsockDialer) DialVsock(ctx context.Context, port int) (net.Conn, error) {
	return d.conn, nil
}

func (d staticVsockDialer) Key() string { return "static-test-dialer" }

// serveFakeBuilderAgent answers host_ready with the given build result.
func serveFakeBuilderAgent(conn net.Conn, result *BuildResult) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var msg VsockMessage
		if err := dec.Decode(&msg); err != nil {
			return
		}
		if msg.Type == "host_ready" {
			_ = enc.Encode(VsockMessage{Type: "build_result", Result: result})
			return
		}
	}
}

// runBuildToCompletion drives runBuild with a fake builder agent that returns
// the given result, then returns the final build record.
func runBuildToCompletion(t *testing.T, mgr *manager, instanceMgr *mockInstanceManager, imageMgr *mockImageManager, id string, result *BuildResult) *Build {
	t.Helper()

	require.NoError(t, writeMetadata(mgr.paths, &buildMetadata{
		ID:        id,
		Status:    StatusQueued,
		Request:   &CreateBuildRequest{},
		CreatedAt: time.Now(),
	}))
	require.NoError(t, mgr.storeSource(id, []byte("fake-tarball-data")))
	require.NoError(t, writeBuildConfig(mgr.paths, id, &BuildConfig{
		JobID:          id,
		RegistryURL:    mgr.config.RegistryURL,
		SourcePath:     "/src",
		TimeoutSeconds: 600,
		NetworkMode:    "egress",
	}))

	hostConn, agentConn := net.Pipe()
	instanceMgr.vsockDialerFunc = func(ctx context.Context, instanceID string) (hypervisor.VsockDialer, error) {
		return staticVsockDialer{conn: hostConn}, nil
	}
	go serveFakeBuilderAgent(agentConn, result)

	imageMgr.SetImageReady(fmt.Sprintf("builds/%s", id))

	policy := DefaultBuildPolicy()
	mgr.runBuild(context.Background(), id, CreateBuildRequest{}, &policy)

	build, err := mgr.GetBuild(context.Background(), id)
	require.NoError(t, err)
	return build
}

// With no publish configuration, a successful build records the local
// builds/<id> reference, exactly as before publication existed.
func TestRunBuild_NoPublishConfigured_LeavesImageRefLocal(t *testing.T) {
	mgr, instanceMgr, _, imageMgr, tempDir := setupTestManagerWithImageMgr(t)
	defer os.RemoveAll(tempDir)

	digest := "sha256:" + strings.Repeat("ab", 32)
	build := runBuildToCompletion(t, mgr, instanceMgr, imageMgr, "build-no-publisher", &BuildResult{
		Success:     true,
		ImageDigest: digest,
	})

	assert.Equal(t, StatusReady, build.Status)
	require.NotNil(t, build.ImageDigest)
	assert.Equal(t, digest, *build.ImageDigest)
	require.NotNil(t, build.ImageRef)
	assert.Equal(t, "builds/build-no-publisher", *build.ImageRef)
}
