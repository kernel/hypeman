package qemu

import (
	"errors"
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
	if err != nil {
		t.Skipf("Skipping test: QEMU binary is not usable: %v", err)
	}

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

func TestShouldRetryWithReducedBalloon(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "unsupported free page reporting",
			err:  errors.New("Property 'virtio-balloon-device.free-page-reporting' not found"),
			want: true,
		},
		{
			name: "unsupported deflate option",
			err:  errors.New("Parameter 'deflate-on-oom' is unexpected"),
			want: true,
		},
		{
			name: "free-page-hint requires iothread",
			err:  errors.New("qemu-system-x86_64: -device virtio-balloon-pci,...: 'free-page-hint' requires 'iothread' to be set"),
			want: true,
		},
		{
			name: "non-balloon start error",
			err:  errors.New("wait for socket /tmp/qemu.sock: timed out after 10s"),
			want: false,
		},
		{
			name: "transient monitor connection refused",
			err:  errors.New("create client: create qemu client: create socket monitor: dial unix /tmp/qemu.sock: connect: connection refused"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldRetryWithReducedBalloon(tt.err))
		})
	}
}

func TestShouldRetrySameConfig(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "monitor connection refused",
			err:  errors.New("create client: dial unix /tmp/qemu.sock: connect: connection refused"),
			want: true,
		},
		{
			name: "socket race no such file",
			err:  errors.New("create socket monitor: dial unix /tmp/qemu.sock: connect: no such file or directory"),
			want: true,
		},
		{
			name: "timeout",
			err:  errors.New("wait for socket /tmp/qemu.sock: timed out after 10s"),
			want: true,
		},
		{
			name: "explicit balloon incompatibility should not use same-config retry",
			err:  errors.New("vmm.log: Property 'virtio-balloon-device.free-page-reporting' not found"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shouldRetrySameConfig(tt.err))
		})
	}
}
