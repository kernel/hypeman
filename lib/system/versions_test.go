package system

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedInitrdHashes maps initrd versions to their expected content hash
// The hash is computed from: sha256(initScript + baseImageDigest)
// This ensures that changes to either the script OR base image require a version bump
var expectedInitrdHashes = map[InitrdVersion]string{
	InitrdV2_0_0: "aaa467ebd20117aeb5aa96831accc9bfd74ed40f25a557a296f5b579b641425b",
	// Add future versions here
}

func TestInitrdVersionIntegrity(t *testing.T) {
	for version, expectedHash := range expectedInitrdHashes {
		t.Run(string(version), func(t *testing.T) {
			// Get the base image digest for this version
			baseImageDigest, ok := InitrdBaseImages[version]
			require.True(t, ok, "Missing base image digest for %s", version)

			// Compute hash from script + digest
			script := GenerateInitScript(version)
			combined := script + baseImageDigest
			actualHash := fmt.Sprintf("%x", sha256.Sum256([]byte(combined)))

			if expectedHash == "PLACEHOLDER" {
				t.Fatalf("Initrd %s needs hash to be set.\n"+
					"Add this to expectedInitrdHashes in versions_test.go:\n"+
					"    InitrdV2_0_0: %q,\n",
					version, actualHash)
			}

			require.Equal(t, expectedHash, actualHash,
				"Initrd %s content changed!\n"+
					"Expected hash: %s\n"+
					"Actual hash:   %s\n\n"+
					"If this is intentional, create a new version:\n"+
					"1. Add new constant in versions.go: InitrdV2_1_0 = \"v2.1.0\"\n"+
					"2. Add base image digest to InitrdBaseImages map\n"+
					"3. Add to SupportedInitrdVersions list\n"+
					"4. Add this hash to expectedInitrdHashes in versions_test.go:\n"+
					"    InitrdV2_1_0: %q,\n"+
					"5. Update DefaultInitrdVersion if this should be the new default\n",
				version, expectedHash, actualHash, actualHash)
		})
	}
}

func TestInitrdBaseImagesArePinned(t *testing.T) {
	// Ensure all initrd versions have valid image references
	// Tags are acceptable since the OCI client resolves them to digests
	for version, baseImageRef := range InitrdBaseImages {
		require.NotEmpty(t, baseImageRef,
			"base image for %s must not be empty",
			version)
		require.Contains(t, baseImageRef, "docker.io/",
			"base image for %s must be a fully qualified reference",
			version)
	}
}

func TestAllInitrdVersionsHaveExpectedHash(t *testing.T) {
	// Ensure every initrd version in InitrdBaseImages has a corresponding hash
	for version := range InitrdBaseImages {
		_, ok := expectedInitrdHashes[version]
		require.True(t, ok, "Initrd version %s is missing from expectedInitrdHashes map in versions_test.go", version)
	}
}

