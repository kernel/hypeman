package firecracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveBinaryPathPrefersCustomPath(t *testing.T) {
	SetCustomBinaryPath("")
	t.Cleanup(func() { SetCustomBinaryPath("") })

	tmpDir := t.TempDir()
	customPath := filepath.Join(tmpDir, "firecracker")
	require.NoError(t, os.WriteFile(customPath, []byte("#!/bin/sh\nexit 0\n"), 0755))

	SetCustomBinaryPath(customPath)
	path, err := resolveBinaryPath(nil, "")
	require.NoError(t, err)
	assert.Equal(t, customPath, path)
}

func TestResolveBinaryPathInvalidCustomPath(t *testing.T) {
	SetCustomBinaryPath("")
	t.Cleanup(func() { SetCustomBinaryPath("") })

	SetCustomBinaryPath("/does/not/exist/firecracker")
	_, err := resolveBinaryPath(nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid firecracker custom binary path")
}

func TestParseVersionFallback(t *testing.T) {
	assert.Equal(t, defaultVersion, parseVersion(""))
	assert.Equal(t, defaultVersion, parseVersion("unknown"))
	assert.Equal(t, V1_14_2, parseVersion("v1.14.2"))
}
