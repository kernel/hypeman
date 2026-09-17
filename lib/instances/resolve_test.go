package instances

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// A fork clones the source directory before writing its own metadata, so a
// half-built fork directory can hold a copy of the source's metadata. Looking
// up the source by name must not see that copy as a second instance.
func TestFindInstanceMetadata_IgnoresDirectoryHoldingAnotherInstancesMetadata(t *testing.T) {
	t.Parallel()
	p := paths.New(t.TempDir())
	mgr := &manager{paths: p}

	source := StoredMetadata{
		Id:             "source-id",
		Name:           "browser-seed",
		Image:          "nginx:latest",
		CreatedAt:      time.Now(),
		HypervisorType: hypervisor.TypeFirecracker,
		DataDir:        p.InstanceDir("source-id"),
	}
	write := func(dirID string, stored StoredMetadata) {
		require.NoError(t, mgr.ensureDirectories(dirID))
		data, err := json.MarshalIndent(&metadata{StoredMetadata: stored}, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(p.InstanceDir(dirID), instanceMetadataRelPath), data, 0644))
	}

	write("source-id", source)
	write("fork-id", source) // fork dir, source's metadata not yet overwritten

	meta, err := mgr.findInstanceMetadataByNameOrIDPrefix("browser-seed", 1)
	require.NoError(t, err)
	require.Equal(t, "source-id", meta.Id)

	// The fork's own id still resolves once it describes itself.
	fork := source
	fork.Id = "fork-id"
	fork.Name = "browser-fork"
	write("fork-id", fork)

	meta, err = mgr.findInstanceMetadataByNameOrIDPrefix("browser-fork", 1)
	require.NoError(t, err)
	require.Equal(t, "fork-id", meta.Id)
}
