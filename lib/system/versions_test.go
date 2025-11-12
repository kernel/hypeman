package system

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedInitrdHashes maps initrd versions to their expected content hash
// The hash is computed from: sha256(initScript + busyboxDigest)
// This ensures that changes to either the script OR busybox version require a version bump
var expectedInitrdHashes = map[InitrdVersion]string{
	InitrdV1_0_0: "a787826fcc61f75cea4f28ef9b0c4f6c25e18866583c652174bb2c20bfe2de6c",
	// Add future versions here
}

func TestInitrdVersionIntegrity(t *testing.T) {
	for version, expectedHash := range expectedInitrdHashes {
		t.Run(string(version), func(t *testing.T) {
			// Get the busybox digest for this version
			busyboxDigest, ok := InitrdBusyboxVersions[version]
			require.True(t, ok, "Missing busybox digest for %s", version)

			// Compute hash from script + digest
			script := GenerateInitScript(version)
			combined := script + busyboxDigest
			actualHash := fmt.Sprintf("%x", sha256.Sum256([]byte(combined)))

			if expectedHash == "PLACEHOLDER" {
				t.Fatalf("Initrd %s needs hash to be set.\n"+
					"Add this to expectedInitrdHashes in versions_test.go:\n"+
					"    InitrdV1_0_0: %q,\n",
					version, actualHash)
			}

			require.Equal(t, expectedHash, actualHash,
				"Initrd %s content changed!\n"+
					"Expected hash: %s\n"+
					"Actual hash:   %s\n\n"+
					"If this is intentional, create a new version:\n"+
					"1. Add new constant in versions.go: InitrdV1_1_0 = \"v1.1.0\"\n"+
					"2. Add busybox digest to InitrdBusyboxVersions map\n"+
					"3. Add to SupportedInitrdVersions list\n"+
					"4. Add this hash to expectedInitrdHashes in versions_test.go:\n"+
					"    InitrdV1_1_0: %q,\n"+
					"5. Update DefaultInitrdVersion if this should be the new default\n",
				version, expectedHash, actualHash, actualHash)
		})
	}
}

func TestInitrdBusyboxVersionsArePinned(t *testing.T) {
	// Ensure all initrd versions use digest-pinned busybox references (not mutable tags)
	for version, busyboxRef := range InitrdBusyboxVersions {
		require.Contains(t, busyboxRef, "@sha256:",
			"busybox version for %s must be pinned to a digest (e.g., busybox@sha256:...), not a mutable tag like :stable",
			version)
	}
}

func TestAllInitrdVersionsHaveExpectedHash(t *testing.T) {
	// Ensure every initrd version in InitrdBusyboxVersions has a corresponding hash
	for version := range InitrdBusyboxVersions {
		_, ok := expectedInitrdHashes[version]
		require.True(t, ok, "Initrd version %s is missing from expectedInitrdHashes map in versions_test.go", version)
	}
}

