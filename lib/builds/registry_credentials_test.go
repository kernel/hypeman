package builds

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/builds/registryauth"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/stretchr/testify/require"
)

func TestBuildRegistryCredentialsLifetime(t *testing.T) {
	for _, outcome := range []string{"cancel", "complete", "expire", "restart"} {
		t.Run(outcome, func(t *testing.T) {
			mgr, instanceMgr, _, dir := setupTestManager(t)
			defer os.RemoveAll(dir)
			mgr.queue = NewBuildQueue(1)
			mgr.queue.active["occupied"] = true
			const secret = "synthetic-build-secret"
			credentials := map[string]registryauth.Credential{"registry.example": {Username: "AWS", Password: secret}}
			build, err := mgr.CreateBuild(context.Background(), CreateBuildRequest{
				Dockerfile: "FROM registry.example/base:1", RegistryCredentials: credentials,
			}, []byte("source"))
			require.NoError(t, err)
			t.Cleanup(func() { mgr.releaseRegistryCredentials(build.ID) })
			credentials["registry.example"] = registryauth.Credential{}
			borrowed, deadline, err := mgr.borrowRegistryCredentials(build.ID)
			require.NoError(t, err)
			require.Equal(t, secret, borrowed["registry.example"].Password)
			require.WithinDuration(t, time.Now().Add(30*time.Minute), deadline, time.Second)
			for _, path := range []string{mgr.paths.BuildMetadata(build.ID), mgr.paths.BuildConfig(build.ID)} {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NotContains(t, string(data), secret)
				require.NotContains(t, string(data), borrowed["registry.example"].Auth())
			}
			config, err := readBuildConfig(mgr.paths, build.ID)
			require.NoError(t, err)
			require.True(t, config.RegistryCredentialsRequired)
			mgr.RecoverPendingBuilds()
			fresh, err := mgr.GetBuild(context.Background(), build.ID)
			require.NoError(t, err)
			require.Equal(t, StatusQueued, fresh.Status)
			switch outcome {
			case "cancel":
				require.NoError(t, mgr.CancelBuild(context.Background(), build.ID))
			case "complete":
				mgr.updateBuildComplete(build.ID, StatusReady, nil, nil, nil, nil)
			case "expire":
				mgr.credentialsMu.Lock()
				mgr.registryCredentials[build.ID].expiresAt = time.Now().Add(-time.Second)
				mgr.credentialsMu.Unlock()
				meta, err := readMetadata(mgr.paths, build.ID)
				require.NoError(t, err)
				mgr.instanceManager = nil // Must fail before any VM can launch.
				mgr.runBuild(context.Background(), build.ID, *meta.Request, &BuildPolicy{TimeoutSeconds: 5})
				meta, err = readMetadata(mgr.paths, build.ID)
				require.NoError(t, err)
				require.Equal(t, StatusFailed, meta.Status)
			case "restart":
				mgr.releaseRegistryCredentials(build.ID)
				meta, err := readMetadata(mgr.paths, build.ID)
				require.NoError(t, err)
				instanceID := "old-builder"
				meta.BuilderInstance = &instanceID
				require.NoError(t, writeMetadata(mgr.paths, meta))
				mgr.queue = NewBuildQueue(1)
				mgr.RecoverPendingBuilds()
				meta, err = readMetadata(mgr.paths, build.ID)
				require.NoError(t, err)
				require.Equal(t, StatusFailed, meta.Status)
				require.Equal(t, 1, instanceMgr.deleteCallCount)
				require.Zero(t, mgr.queue.QueueLength())
			}
			_, _, err = mgr.borrowRegistryCredentials(build.ID)
			require.ErrorIs(t, err, errRegistryCredentialsUnavailable)
		})
	}
}

func TestRegistryCredentialExpiryReleasesMemory(t *testing.T) {
	mgr, _, _, dir := setupTestManager(t)
	defer os.RemoveAll(dir)
	expires := time.Now().Add(50 * time.Millisecond)
	mgr.holdRegistryCredentials("expired", map[string]registryauth.Credential{
		"registry.example": {Username: "AWS", Password: "synthetic-token", ExpiresAt: &expires},
	})
	defer mgr.releaseRegistryCredentials("expired")
	require.Eventually(t, func() bool {
		mgr.credentialsMu.Lock()
		defer mgr.credentialsMu.Unlock()
		return mgr.registryCredentials["expired"] == nil
	}, time.Second, 5*time.Millisecond)
}

type credentialTestDialer struct{ net.Conn }

func (d credentialTestDialer) DialVsock(context.Context, int) (net.Conn, error) { return d.Conn, nil }
func (d credentialTestDialer) Key() string                                      { return "registry-auth-test" }

func TestRegistryCredentialsVsock(t *testing.T) {
	for _, scenario := range []string{"handoff", "old-builder", "startup-failure"} {
		t.Run(scenario, func(t *testing.T) {
			supported := scenario == "handoff"
			mgr, instanceMgr, _, dir := setupTestManager(t)
			defer os.RemoveAll(dir)
			credentials := map[string]registryauth.Credential{"registry.example": {Username: "AWS", Password: `synthetic-"secret"`}}
			mgr.holdRegistryCredentials("build", credentials)
			defer mgr.releaseRegistryCredentials("build")
			require.NoError(t, os.MkdirAll(mgr.paths.BuildDir("build"), 0755))
			host, guest := net.Pipe()
			defer guest.Close()
			instanceMgr.vsockDialerFunc = func(context.Context, string) (hypervisor.VsockDialer, error) { return credentialTestDialer{host}, nil }
			done := make(chan error, 1)
			go func() {
				decoder, encoder := json.NewDecoder(guest), json.NewEncoder(guest)
				var message VsockMessage
				if err := decoder.Decode(&message); err != nil {
					done <- err
					return
				}
				if supported {
					if err := encoder.Encode(VsockMessage{Type: "get_registry_credentials"}); err != nil {
						done <- err
						return
					}
					if err := decoder.Decode(&message); err != nil {
						done <- err
						return
					}
					if message.RegistryCredentials["registry.example"].Password != credentials["registry.example"].Password {
						done <- registryauth.ErrInvalid
						return
					}
					if err := encoder.Encode(VsockMessage{Type: "log", Log: credentials["registry.example"].Auth()}); err != nil {
						done <- err
						return
					}
				}
				if !supported {
					if err := encoder.Encode(VsockMessage{Type: "log", Log: credentials["registry.example"].Auth()}); err != nil {
						done <- err
						return
					}
				}
				result := &BuildResult{Success: true, Logs: credentials["registry.example"].Password}
				if scenario == "startup-failure" {
					result.Success = false
					result.Error = "load config: missing build.json"
				}
				done <- encoder.Encode(VsockMessage{Type: "build_result", Result: result})
			}()
			result, err := mgr.waitForResult(context.Background(), "build", &instances.Instance{StoredMetadata: instances.StoredMetadata{Id: "vm"}}, true)
			require.NoError(t, <-done)
			if scenario == "old-builder" {
				require.ErrorContains(t, err, "update the builder image")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "[REDACTED]", result.Logs)
			if scenario == "startup-failure" {
				require.False(t, result.Success)
				require.Equal(t, "load config: missing build.json", result.Error)
			}
			data, err := os.ReadFile(mgr.paths.BuildLog("build"))
			require.NoError(t, err)
			require.False(t, strings.Contains(string(data), credentials["registry.example"].Auth()))
		})
	}
}
