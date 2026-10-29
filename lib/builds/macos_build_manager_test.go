package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

type machineJobImages struct {
	images.Manager
	fixture    *machineFixture
	resolveErr error
}

func (m machineJobImages) GetImage(ctx context.Context, ref string) (*images.Image, error) {
	if m.resolveErr != nil {
		return nil, m.resolveErr
	}
	if ref == "registry/base@"+machineTestDigest {
		return m.fixture.ResolveBase(ctx, ref)
	}
	return &images.Image{Status: images.StatusReady, Digest: machineTestDigest}, nil
}

func (m machineJobImages) WaitForReady(context.Context, string) error { return nil }

func newMachineTestManager(t *testing.T, f *machineFixture, publisher MachineBuildPublisher, imageManager images.Manager, machineOnly bool) *manager {
	t.Helper()
	config := DefaultConfig()
	config.RegistrySecret = "synthetic-test-secret"
	config.MachineBuildOnly = machineOnly
	if f != nil && publisher != nil {
		config.MachineBuild = &MachineBuildBackend{Driver: f, Publisher: publisher}
	}
	if imageManager == nil && f != nil {
		imageManager = machineJobImages{fixture: f}
	}
	api, err := NewManager(paths.New(t.TempDir()), config, noPriorBuilder{}, nil, nil, imageManager, nil, nil, nil)
	require.NoError(t, err)
	return api.(*manager)
}

type machineJobPublisher struct {
	fixture     *machineFixture
	publishedID string
	// sourcePath, when set, must still exist at publish time.
	sourcePath func(id string) string
}

func (p *machineJobPublisher) Publish(ctx context.Context, id, bundle string) (MachinePublication, error) {
	p.publishedID = id
	if p.sourcePath != nil {
		if _, err := os.Stat(p.sourcePath(id)); err != nil {
			return MachinePublication{}, errors.New("source removed before publish")
		}
	}
	// Reuse the fixture's export assertions with its fixed ID.
	receipt, err := p.fixture.Publish(ctx, "build-test", bundle)
	if err == nil {
		receipt.Reference = "localhost:4973/builds/" + id + "@" + receipt.Digest
	}
	return receipt, err
}
func TestMachineBuildUsesSharedQueueStatusAndSourceLifecycle(t *testing.T) {
	f := newMachineFixture()
	publisher := &machineJobPublisher{fixture: f}
	m := newMachineTestManager(t, f, publisher, nil, false)
	publisher.sourcePath = func(id string) string { return filepath.Join(m.paths.BuildSourceDir(id), "source.tar.gz") }
	source := []byte("synthetic compressed-source bytes")
	sum := sha256.Sum256(source)
	req := CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest, SourceHash: hex.EncodeToString(sum[:])}
	build, err := m.CreateBuild(context.Background(), req, source)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		b, e := m.GetBuild(context.Background(), build.ID)
		return e == nil && (b.Status == StatusReady || b.Status == StatusFailed)
	}, 5*time.Second, 10*time.Millisecond)
	done, err := m.GetBuild(context.Background(), build.ID)
	require.NoError(t, err)
	require.Equal(t, StatusReady, done.Status, "failure: %v", done.Error)
	require.NotNil(t, done.ImageRef)
	require.Equal(t, "localhost:4973/builds/"+build.ID+"@"+machineTestDigest, *done.ImageRef)
	require.Equal(t, build.ID, publisher.publishedID)
	require.Equal(t, req.SourceHash, done.Provenance.SourceHash)
	require.Equal(t, machineTestDigest, done.Provenance.BaseImageDigest)
	require.Equal(t, f.ID(), *done.BuilderInstanceID)
	meta, err := readMetadata(m.paths, build.ID)
	require.NoError(t, err)
	require.Equal(t, req.MachineBaseImage, meta.Request.MachineBaseImage)
	require.Equal(t, "isolated", meta.Request.BuildPolicy.NetworkMode)
	require.Equal(t, 0, meta.Request.BuildPolicy.CPUs, "machine defaults must not use Linux CPU defaults")
	_, err = readBuildConfig(m.paths, build.ID)
	require.Error(t, err, "machine builds must not persist a builder config or registry token")
	require.NoFileExists(t, filepath.Join(m.paths.BuildSourceDir(build.ID), "source.tar.gz"))
}
func TestMachineBuildsOnOneBaseRunConcurrently(t *testing.T) {
	fixture := newMachineFixture()
	fixture.waitForCancel = true
	m := newMachineTestManager(t, fixture, fixture, nil, false)
	req := CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}
	var ids []string
	for range 2 {
		build, err := m.CreateBuild(context.Background(), req, []byte("source"))
		require.NoError(t, err)
		ids = append(ids, build.ID)
	}
	require.Eventually(t, func() bool { return m.queue.ActiveCount() == 2 }, 5*time.Second, 10*time.Millisecond, "builds on one base must not serialize")
	for _, id := range ids {
		// Cancel only once the attempt has recorded its builder, so it is Building.
		require.Eventually(t, func() bool {
			meta, err := readMetadata(m.paths, id)
			return err == nil && meta.BuilderInstance != nil
		}, 5*time.Second, 10*time.Millisecond)
		require.NoError(t, m.CancelBuild(context.Background(), id))
	}
	require.Eventually(t, func() bool { return m.queue.ActiveCount() == 0 }, 5*time.Second, 10*time.Millisecond)
}

func TestMachineBuildAdmissionAndSourceHash(t *testing.T) {
	m := newMachineTestManager(t, nil, nil, nil, false)
	_, err := m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}, []byte("source"))
	require.ErrorContains(t, err, "not configured")
	f := newMachineFixture()
	m.config.MachineBuild = &MachineBuildBackend{Driver: f, Publisher: f}
	_, err = m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base:mutable"}, []byte("source"))
	require.ErrorContains(t, err, "pinned")
	_, err = m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest, Secrets: []SecretRef{{ID: "unsupported"}}}, []byte("source"))
	require.ErrorContains(t, err, "secret options")
	_, err = m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest, SourceHash: "wrong"}, []byte("source"))
	require.True(t, errors.Is(err, ErrSourceHashMismatch))
	_, err = m.admitMachineBuild(&CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}, make([]byte, maxBuildSourceBytes+1))
	require.ErrorContains(t, err, "source too large")
	list, err := m.ListBuilds(context.Background())
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestQueuedMachineCancellationRemovesSource(t *testing.T) {
	fixture := newMachineFixture()
	fixture.waitForCancel = true
	m := newMachineTestManager(t, fixture, fixture, nil, false)
	req := CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}
	// Fill both concurrency slots so the next build stays queued.
	for range m.config.MaxConcurrentBuilds {
		first, err := m.CreateBuild(context.Background(), req, []byte("synthetic source"))
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = m.CancelBuild(context.Background(), first.ID)
			require.Eventually(t, func() bool { _, active := m.machineBuilds.Load(first.ID); return !active }, 5*time.Second, 10*time.Millisecond)
		})
		require.Eventually(t, func() bool {
			meta, err := readMetadata(m.paths, first.ID)
			return err == nil && meta.BuilderInstance != nil
		}, 5*time.Second, 10*time.Millisecond)
	}
	queued, err := m.CreateBuild(context.Background(), req, []byte("queued source"))
	require.NoError(t, err)
	source := filepath.Join(m.paths.BuildSourceDir(queued.ID), "source.tar.gz")
	require.FileExists(t, source)
	require.NoError(t, m.CancelBuild(context.Background(), queued.ID))
	done, err := m.GetBuild(context.Background(), queued.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, done.Status)
	require.NotNil(t, done.CompletedAt)
	require.NoFileExists(t, source)
}

func TestMachineCancellationInterruptsPhasesWithoutPublishing(t *testing.T) {
	fixture := newMachineFixture()
	fixture.waitForCancel = true
	m := newMachineTestManager(t, fixture, fixture, nil, false)
	build, e := m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}, []byte("synthetic source"))
	require.NoError(t, e)
	require.Eventually(t, func() bool {
		meta, e := readMetadata(m.paths, build.ID)
		return e == nil && meta.BuilderInstance != nil
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, m.CancelBuild(context.Background(), build.ID))
	require.Eventually(t, func() bool { _, active := m.machineBuilds.Load(build.ID); return !active }, 5*time.Second, 10*time.Millisecond)
	done, e := m.GetBuild(context.Background(), build.ID)
	require.NoError(t, e)
	require.Equal(t, StatusCancelled, done.Status)
	require.False(t, fixture.publisherCalled)
	require.Contains(t, fixture.calls, "abort")
	require.Contains(t, fixture.calls, "destroy")
}

func TestMachineFailureUsesSharedFailedStatusWithoutPublication(t *testing.T) {
	for _, tc := range []struct {
		name       string
		phase      string
		resolveErr error
		wantError  string
	}{
		{name: "base resolution", resolveErr: errors.New("sensitive-resolution-error"), wantError: "machine build resolve_base failed"},
		{name: "provision", phase: "provision", wantError: "machine build provision failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMachineFixture()
			f.fail = tc.phase
			m := newMachineTestManager(t, f, f, machineJobImages{fixture: f, resolveErr: tc.resolveErr}, false)
			build, err := m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}, []byte("source"))
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				b, e := m.GetBuild(context.Background(), build.ID)
				return e == nil && b.Status == StatusFailed
			}, 5*time.Second, 10*time.Millisecond)
			done, err := m.GetBuild(context.Background(), build.ID)
			require.NoError(t, err)
			require.Equal(t, StatusFailed, done.Status)
			require.Nil(t, done.ImageDigest)
			require.NotContains(t, f.calls, "publish")
			require.NotContains(t, *done.Error, "sensitive-resolution-error")
			require.Equal(t, tc.wantError, *done.Error)
			if tc.phase == "provision" {
				require.Contains(t, f.calls, "destroy")
			} else {
				require.Empty(t, f.calls)
			}
		})
	}
}

func TestMachineFailureShowsOnlyExitCode(t *testing.T) {
	err := machineFailure("provision", fmt.Errorf("guest said secret-token: %w", &machineExitError{code: 7}))
	require.Equal(t, "machine build provision failed: build.sh exited with code 7", err.Error())
	require.Equal(t, "machine build provision failed", machineFailure("provision", fmt.Errorf("secret-token")).Error())
	admission := fmt.Errorf("%w: macOS template identity is already in use", instances.ErrInsufficientResources)
	require.Contains(t, machineFailure("start", admission).Error(), "machine build start failed: insufficient resources")
}

// noPriorBuilder reports that no earlier attempt left a builder instance behind.
type noPriorBuilder struct{ instances.Manager }

func (noPriorBuilder) GetInstance(context.Context, string) (*instances.Instance, error) {
	return nil, instances.ErrNotFound
}
