package cloudhypervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const experimentalDiffSnapshotsEnv = "HYPEMAN_EXPERIMENTAL_CH_DIFF_SNAPSHOTS"

const cloudHypervisorDiffMemoryFile = cloudHypervisorMemoryFile + ".diff"

type diffMergeStats struct {
	DeltaBytes     int64
	ExtentCount    int
	ReflinkedBytes int64
	CopiedBytes    int64
}

func experimentalDiffSnapshotsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(experimentalDiffSnapshotsEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// prepareDiffSnapshotDestination leaves the retained memory baseline in place
// and removes metadata that Cloud Hypervisor recreates with O_EXCL.
func prepareDiffSnapshotDestination(snapshotDir string) (bool, error) {
	if !experimentalDiffSnapshotsEnabled() {
		return false, nil
	}
	memoryPath := filepath.Join(snapshotDir, cloudHypervisorMemoryFile)
	if _, err := os.Stat(memoryPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat retained snapshot memory: %w", err)
	}

	for _, name := range []string{
		cloudHypervisorConfigFile,
		cloudHypervisorStateFile,
		cloudHypervisorDiffMemoryFile,
	} {
		if err := os.Remove(filepath.Join(snapshotDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("remove retained snapshot file %s: %w", name, err)
		}
	}
	return true, nil
}
