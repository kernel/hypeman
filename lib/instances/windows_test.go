package instances

import (
	"os"
	"testing"

	"github.com/kernel/hypeman/lib/autostandby"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func windowsImageFixture() *images.Image {
	return &images.Image{
		Name:     "registry.example/windows/image:test",
		Digest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Platform: "windows/amd64",
		Status:   images.StatusReady,
		Machine: &images.MachineImage{
			Kind:        images.MachineImageWindowsImage,
			Base:        "registry.example/windows/base@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			TPM:         "2.0",
			SecureBoot:  "required",
			VirtualSize: 80 << 30,
		},
	}
}

func TestValidateWindowsCreate(t *testing.T) {
	image := windowsImageFixture()
	windowsCaps := hypervisor.Capabilities{SupportsUEFIBoot: true, SupportsTPM: true}
	require.NoError(t, validateWindowsCreate(CreateInstanceRequest{}, image, windowsCaps))
	require.NoError(t, validateWindowsCreate(CreateInstanceRequest{NetworkEnabled: true}, image, windowsCaps))

	tests := []struct {
		name string
		req  CreateInstanceRequest
		caps hypervisor.Capabilities
	}{
		{name: "missing boot capabilities"},
		{name: "small memory", caps: windowsCaps, req: CreateInstanceRequest{Size: 2 << 30}},
		{name: "one CPU", caps: windowsCaps, req: CreateInstanceRequest{Vcpus: 1}},
		{name: "command", caps: windowsCaps, req: CreateInstanceRequest{Cmd: []string{"cmd.exe"}}},
		{name: "snapshot policy", caps: windowsCaps, req: CreateInstanceRequest{SnapshotPolicy: &SnapshotPolicy{}}},
		{name: "auto standby", caps: windowsCaps, req: CreateInstanceRequest{AutoStandby: &autostandby.Policy{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, validateWindowsCreate(tt.req, image, tt.caps))
		})
	}
}

func TestRejectWindowsSnapshotLifecycle(t *testing.T) {
	assert.ErrorIs(t, rejectWindowsSnapshotLifecycle("windows/amd64", "fork"), ErrNotSupported)
	assert.NoError(t, rejectWindowsSnapshotLifecycle("linux/amd64", "fork"))
}

func TestBuildWindowsHypervisorConfig(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	stored := StoredMetadata{Id: "instance", Platform: "windows/amd64", Size: 8 << 30, Vcpus: 4, VsockCID: 42}
	require.NoError(t, os.MkdirAll(p.InstanceDir(stored.Id), 0755))
	for _, path := range []string{p.InstanceWindowsDisk(stored.Id), p.InstanceOVMFCode(stored.Id), p.InstanceOVMFVars(stored.Id)} {
		require.NoError(t, os.WriteFile(path, []byte("fixture"), 0600))
	}

	config, err := m.buildWindowsHypervisorConfig(&Instance{StoredMetadata: stored}, windowsImageFixture(), nil)
	require.NoError(t, err)
	assert.Equal(t, hypervisor.BootModeUEFI, config.BootMode)
	assert.True(t, config.Firmware.SecureBoot)
	assert.Equal(t, p.InstanceTPMDir(stored.Id), config.TPM.StateDir)
	require.Len(t, config.Disks, 1)
	assert.Equal(t, hypervisor.DiskFormatQCOW2, config.Disks[0].Format)
	assert.Empty(t, config.KernelPath)
	assert.Empty(t, config.InitrdPath)
}
