package builds

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSourceCleanupRequiresPersistedCompletion(t *testing.T) {
	for _, outcome := range []string{StatusReady, StatusFailed, "cancel-running", "cancel-queued"} {
		for _, failWrite := range []bool{false, true} {
			name := outcome
			if failWrite {
				name += "/write-failure"
			}
			t.Run(name, func(t *testing.T) {
				mgr, _, _, tempDir := setupTestManager(t)
				defer os.RemoveAll(tempDir)
				status := StatusBuilding
				if outcome == "cancel-queued" {
					status = StatusQueued
					mgr.queue = NewBuildQueue(1)
					release := make(chan struct{})
					done := make(chan struct{})
					mgr.queue.Enqueue("blocker", CreateBuildRequest{}, func() {
						defer close(done)
						<-release
					})
					defer func() { close(release); <-done }()
					mgr.queue.Enqueue("build", CreateBuildRequest{}, func() {})
				}
				writeTestBuild(t, mgr, "build", status)
				if failWrite {
					// Fail the atomic temp-file write, even when tests run as root.
					require.NoError(t, os.Mkdir(mgr.paths.BuildMetadata("build")+".tmp", 0700))
				}

				if outcome == "cancel-running" || outcome == "cancel-queued" {
					err := mgr.CancelBuild(context.Background(), "build")
					if failWrite {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
				} else {
					mgr.updateBuildComplete("build", outcome, nil, nil, nil, nil)
				}

				meta, err := readMetadata(mgr.paths, "build")
				require.NoError(t, err)
				if failWrite {
					assert.Equal(t, status, meta.Status)
					assert.Nil(t, meta.CompletedAt)
					data, err := os.ReadFile(filepath.Join(mgr.paths.BuildSourceDir("build"), "source.tar.gz"))
					require.NoError(t, err)
					assert.Equal(t, "API_KEY='synthetic-secret'", string(data))
					if outcome == "cancel-queued" {
						assert.Equal(t, 1, mgr.queue.PendingCount())
						require.NoError(t, os.Remove(mgr.paths.BuildMetadata("build")+".tmp"))
						require.NoError(t, mgr.CancelBuild(context.Background(), "build"))
						assert.Zero(t, mgr.queue.PendingCount())
						meta, err = readMetadata(mgr.paths, "build")
						require.NoError(t, err)
						assert.Equal(t, StatusCancelled, meta.Status)
						assert.NoDirExists(t, mgr.paths.BuildSourceDir("build"))
					}
				} else {
					assert.NotNil(t, meta.CompletedAt)
					assert.NoDirExists(t, mgr.paths.BuildSourceDir("build"))
				}
			})
		}
	}
}
