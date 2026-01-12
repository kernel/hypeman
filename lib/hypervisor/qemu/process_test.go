package qemu

import (
	"os/exec"
	"regexp"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetVersion_Integration is an integration test that verifies GetVersion
// works correctly with the actual QEMU binary installed on the system.
func TestGetVersion_Integration(t *testing.T) {
	// Skip if QEMU is not installed
	binaryName, err := qemuBinaryName()
	if err != nil {
		t.Skipf("Skipping test: %v", err)
	}

	_, err = exec.LookPath(binaryName)
	if err != nil {
		t.Skipf("Skipping test: QEMU binary %s not found in PATH", binaryName)
	}

	// Create starter and get version
	starter := NewStarter()
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)

	version, err := starter.GetVersion(p)
	require.NoError(t, err, "GetVersion should not return an error")

	// Verify version is not empty
	assert.NotEmpty(t, version, "Version should not be empty")

	// Verify version matches expected format (e.g., "8.2.0", "9.0", "7.2.1")
	versionPattern := regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`)
	assert.Regexp(t, versionPattern, version, "Version should match pattern X.Y or X.Y.Z")

	t.Logf("Detected QEMU version: %s", version)
}

// TestGetVersion_ParsesVersionCorrectly tests the version parsing logic
// with various version string formats.
func TestGetVersion_ParsesVersionCorrectly(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected string
		wantErr  bool
	}{
		{
			name:     "debian format",
			output:   "QEMU emulator version 8.2.0 (Debian 1:8.2.0+dfsg-1)",
			expected: "8.2.0",
		},
		{
			name:     "simple format",
			output:   "QEMU emulator version 9.0.0",
			expected: "9.0.0",
		},
		{
			name:     "two part version",
			output:   "QEMU emulator version 9.0",
			expected: "9.0",
		},
		{
			name:     "with git info",
			output:   "QEMU emulator version 7.2.1 (qemu-7.2.1-1.fc38)",
			expected: "7.2.1",
		},
		{
			name:    "invalid format",
			output:  "Some random output",
			wantErr: true,
		},
		{
			name:    "empty output",
			output:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use the same regex as in GetVersion
			re := regexp.MustCompile(`version (\d+\.\d+(?:\.\d+)?)`)
			matches := re.FindStringSubmatch(tt.output)

			if tt.wantErr {
				assert.Less(t, len(matches), 2, "Should not match for invalid input")
			} else {
				require.GreaterOrEqual(t, len(matches), 2, "Should find version match")
				assert.Equal(t, tt.expected, matches[1], "Parsed version should match expected")
			}
		})
	}
}
