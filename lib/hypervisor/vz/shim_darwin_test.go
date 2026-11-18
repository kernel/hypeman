//go:build darwin

package vz

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/stretchr/testify/require"
)

// runShim runs the embedded shim with args and returns its stderr and exit code.
func runShim(t *testing.T, args ...string) (string, int) {
	t.Helper()
	bin, err := extractShim()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stderr.String(), exitErr.ExitCode()
	}
	require.NoError(t, err)
	return stderr.String(), 0
}

func runShimConfig(t *testing.T, config shimconfig.ShimConfig) (string, int) {
	t.Helper()
	config.LogPath = filepath.Join(t.TempDir(), "vz-shim.log")
	data, err := json.Marshal(config)
	require.NoError(t, err)
	return runShim(t, "-config", string(data))
}

func TestShimRejectsMissingOrInvalidConfig(t *testing.T) {
	stderr, code := runShim(t)
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "-config is required")
	stderr, code = runShim(t, "-config", "{")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "invalid config JSON")
}

func TestShimMacBootRequiresCompleteIdentity(t *testing.T) {
	for _, c := range []shimconfig.ShimConfig{
		{MacHardwareModelData: "AA=="},
		{MacMachineIdentifierData: "AA=="},
		{MacAuxStoragePath: "/missing"},
	} {
		stderr, code := runShimConfig(t, c)
		require.Equal(t, 1, code)
		require.Contains(t, stderr, "macOS boot requires")
	}
}

func TestShimMacPlatformRejectsMalformedModel(t *testing.T) {
	stderr, code := runShimConfig(t, shimconfig.ShimConfig{MacHardwareModelData: "not base64!", MacMachineIdentifierData: "AA==", MacAuxStoragePath: "/missing"})
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "decode Mac hardware model")
}

// The shim holds a lock named for the identity's bytes, so encodings of the same
// identity contend for it.
func TestShimMacIdentityLockExcludesEquivalentIdentity(t *testing.T) {
	identity := []byte("identity")
	path := filepath.Join("/tmp", fmt.Sprintf("hypeman-mac-identity-%d-%x.lock", os.Getuid(), sha256.Sum256(identity)))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))

	encoded := base64.StdEncoding.EncodeToString(identity) + "\n"
	stderr, code := runShimConfig(t, shimconfig.ShimConfig{MacMachineIdentifierData: encoded})
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "Mac machine identity already in use")

	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
	stderr, code = runShimConfig(t, shimconfig.ShimConfig{MacMachineIdentifierData: encoded})
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "macOS boot requires", "with the lock free the shim gets past admission")
}
