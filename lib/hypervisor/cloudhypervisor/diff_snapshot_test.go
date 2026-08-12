package cloudhypervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/vmm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareDiffSnapshotDestination(t *testing.T) {
	t.Setenv(experimentalDiffSnapshotsEnv, "true")
	dir := t.TempDir()
	for _, name := range []string{
		cloudHypervisorMemoryFile,
		cloudHypervisorConfigFile,
		cloudHypervisorStateFile,
		cloudHypervisorDiffMemoryFile,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0600))
	}

	diff, err := prepareDiffSnapshotDestination(dir)
	require.NoError(t, err)
	assert.True(t, diff)
	assert.FileExists(t, filepath.Join(dir, cloudHypervisorMemoryFile))
	assert.NoFileExists(t, filepath.Join(dir, cloudHypervisorConfigFile))
	assert.NoFileExists(t, filepath.Join(dir, cloudHypervisorStateFile))
	assert.NoFileExists(t, filepath.Join(dir, cloudHypervisorDiffMemoryFile))
}

func TestExperimentalDiffSnapshotCapability(t *testing.T) {
	t.Setenv(experimentalDiffSnapshotsEnv, "true")
	assert.True(t, CapabilitiesForVersion(vmm.V51_1).SupportsSnapshotBaseReuse)
	t.Setenv(experimentalDiffSnapshotsEnv, "false")
	assert.False(t, CapabilitiesForVersion(vmm.V51_1).SupportsSnapshotBaseReuse)
}
