package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

type machineJobImages struct {
	images.Manager
	fixture *machineFixture
}

func (m machineJobImages) GetImage(ctx context.Context, ref string) (*images.Image, error) {
	if ref == "registry/base@"+machineTestDigest {
		return m.fixture.ResolveBase(ctx, ref)
	}
	return &images.Image{Status: images.StatusReady, Digest: machineTestDigest}, nil
}

func (m machineJobImages) WaitForReady(context.Context, string) error { return nil }

type machineJobPublisher struct {
	fixture     *machineFixture
	publishedID string
}

func (p *machineJobPublisher) Publish(ctx context.Context, id, bundle string) (MachinePublication, error) {
	p.publishedID = id
	// Reuse the fixture's export/source-scrubbing assertions with its fixed ID.
	receipt, err := p.fixture.Publish(ctx, "build-test", bundle)
	if err == nil {
		receipt.Reference = "localhost:4973/builds/" + id + "@" + receipt.Digest
	}
	return receipt, err
}
func TestMachineBuildUsesSharedQueueStatusAndSourceLifecycle(t *testing.T) {
	f := newMachineFixture()
	publisher := &machineJobPublisher{fixture: f}
	config := DefaultConfig()
	config.RegistrySecret = "synthetic-test-secret"
	config.MachineBuild = &MachineBuildBackend{Driver: f, Publisher: publisher}
	api, err := NewManager(paths.New(t.TempDir()), config, nil, nil, nil, machineJobImages{fixture: f}, nil, nil, nil)
	require.NoError(t, err)
	m := api.(*manager)
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
	require.Equal(t, build.ID, publisher.publishedID)
	require.Equal(t, req.SourceHash, done.Provenance.SourceHash)
	require.Equal(t, machineTestDigest, done.Provenance.BaseImageDigest)
	require.Equal(t, f.ID(), *done.BuilderInstanceID)
	meta, err := readMetadata(m.paths, build.ID)
	require.NoError(t, err)
	require.Equal(t, req.MachineBaseImage, meta.Request.MachineBaseImage)
	require.Equal(t, "isolated", meta.Request.BuildPolicy.NetworkMode)
	require.Equal(t, 0, meta.Request.BuildPolicy.CPUs, "machine defaults must not use Linux CPU defaults")
	require.NoFileExists(t, filepath.Join(m.paths.BuildSourceDir(build.ID), "source.tar.gz"))
}
func TestMachineBuildAdmissionAndSharedSourceHash(t *testing.T) {
	config := DefaultConfig()
	config.RegistrySecret = "synthetic-test-secret"
	api, err := NewManager(paths.New(t.TempDir()), config, nil, nil, nil, nil, nil, nil, nil)
	require.NoError(t, err)
	m := api.(*manager)
	_, err = m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest}, []byte("source"))
	require.ErrorContains(t, err, "not configured")
	f := newMachineFixture()
	m.config.MachineBuild = &MachineBuildBackend{Driver: f, Publisher: f}
	_, err = m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base:mutable"}, []byte("source"))
	require.ErrorContains(t, err, "pinned")
	_, err = m.CreateBuild(context.Background(), CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest, Secrets: []SecretRef{{ID: "unsupported"}}}, []byte("source"))
	require.ErrorContains(t, err, "secret options")
	_, err = m.CreateBuild(context.Background(), CreateBuildRequest{SourceHash: "wrong"}, []byte("source"))
	require.True(t, errors.Is(err, ErrSourceHashMismatch))
	list, err := m.ListBuilds(context.Background())
	require.NoError(t, err)
	require.Empty(t, list)
}
