package instances

import (
	"context"
	"errors"
	"testing"

	"github.com/kernel/hypeman/lib/network"
	"github.com/stretchr/testify/require"
)

type bootNetworkFixture struct {
	network.Manager
	requested network.AllocateRequest
	config    *network.NetworkConfig
	createErr error
	lookupErr error
	released  bool
	fallback  bool
}

func (f *bootNetworkFixture) CreateAllocation(_ context.Context, req network.AllocateRequest) (*network.NetworkConfig, error) {
	f.requested = req
	return f.config, f.createErr
}
func (f *bootNetworkFixture) GetAllocation(context.Context, string) (*network.Allocation, error) {
	return &network.Allocation{InstanceID: f.requested.InstanceID, TAPDevice: f.config.TAPDevice}, f.lookupErr
}
func (f *bootNetworkFixture) ReleaseAllocation(_ context.Context, a *network.Allocation) error {
	f.released = a.InstanceID == f.requested.InstanceID && a.TAPDevice == f.config.TAPDevice
	return nil
}
func (f *bootNetworkFixture) ReleaseByInstanceID(_ context.Context, id string) error {
	f.fallback = id == f.requested.InstanceID
	return nil
}

func TestPrepareBootMachineSkipsLinuxDependencies(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		m := &manager{}
		stored := &StoredMetadata{Id: "machine", MacOS: testMacImage().MacOS, NetworkEnabled: enabled, IP: "old-lease"}
		config, release, err := m.prepareBootNetwork(context.Background(), stored, network.AllocateRequest{})
		require.NoError(t, err)
		require.Nil(t, release)
		if enabled {
			require.Equal(t, stored.MacOS.MAC, config.MAC)
			require.Empty(t, stored.IP)
		} else {
			require.Nil(t, config)
		}
		release, err = m.prepareBootConfig(context.Background(), stored, nil, config)
		require.NoError(t, err)
		require.Nil(t, release)
	}
}
func TestPrepareBootNetworkPreservesRequestAndRollback(t *testing.T) {
	for _, lookupFails := range []bool{false, true} {
		f := &bootNetworkFixture{config: &network.NetworkConfig{IP: "192.0.2.1", MAC: "02:00:00:00:00:01", TAPDevice: "synthetic"}}
		if lookupFails {
			f.lookupErr = errors.New("lookup failed")
		}
		m := &manager{networkManager: f}
		stored := &StoredMetadata{Id: "linux", NetworkEnabled: true}
		request := network.AllocateRequest{InstanceID: stored.Id, InstanceName: "test", DownloadBps: 100, UploadBps: 200, UploadCeilBps: 400}
		config, release, err := m.prepareBootNetwork(context.Background(), stored, request)
		require.NoError(t, err)
		require.Equal(t, request, f.requested)
		require.Same(t, f.config, config)
		require.Equal(t, config.IP, stored.IP)
		require.Equal(t, config.MAC, stored.MAC)
		release()
		require.Equal(t, !lookupFails, f.released)
		require.Equal(t, lookupFails, f.fallback)
	}
}
func TestPrepareBootNetworkFailureDoesNotMutateMetadata(t *testing.T) {
	cause := errors.New("allocation failed")
	f := &bootNetworkFixture{createErr: cause}
	m := &manager{networkManager: f}
	stored := &StoredMetadata{Id: "linux", NetworkEnabled: true, IP: "before", MAC: "before"}
	_, release, err := m.prepareBootNetwork(context.Background(), stored, network.AllocateRequest{InstanceID: stored.Id})
	require.ErrorIs(t, err, cause)
	require.Nil(t, release)
	require.Equal(t, "before", stored.IP)
	require.Equal(t, "before", stored.MAC)
}
