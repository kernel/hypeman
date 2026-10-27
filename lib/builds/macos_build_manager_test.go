package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
func TestMachineBuildSerializesByBaseDigest(t *testing.T) {
	first := CreateBuildRequest{MachineBaseImage: "registry-a/base@" + machineTestDigest}
	second := CreateBuildRequest{MachineBaseImage: "registry-b/another-name@" + machineTestDigest}
	require.Equal(t, machineBuildSerialKey(first), machineBuildSerialKey(second))
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
	for _, policy := range []BuildPolicy{{TimeoutSeconds: -1}, {TimeoutSeconds: 86401}, {NetworkMode: "host"}, {AllowedDomains: []string{"example.test"}}} {
		req := CreateBuildRequest{MachineBaseImage: "registry/base@" + machineTestDigest, BuildPolicy: &policy}
		_, err := m.admitMachineBuild(&req, []byte("source"))
		require.Error(t, err)
	}
	list, err := m.ListBuilds(context.Background())
	require.NoError(t, err)
	require.Empty(t, list)
}

// noPriorBuilder reports that no earlier attempt left a builder instance behind.
type noPriorBuilder struct{ instances.Manager }

func (noPriorBuilder) GetInstance(context.Context, string) (*instances.Instance, error) {
	return nil, instances.ErrNotFound
}
