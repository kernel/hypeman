package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKernelHeadersAlreadyInstalled(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	kernel := "test-kernel"
	paths := kernelHeadersPaths{
		libModulesDir: filepath.Join(root, "lib/modules"),
		usrSrcDir:     filepath.Join(root, "usr/src"),
	}

	headersDir := filepath.Join(paths.usrSrcDir, "linux-headers-"+kernel)
	modulesDir := filepath.Join(paths.libModulesDir, kernel)
	require.NoError(t, os.MkdirAll(headersDir, 0755))
	require.NoError(t, os.MkdirAll(modulesDir, 0755))
	require.NoError(t, os.Symlink("/usr/src/linux-headers-"+kernel, filepath.Join(modulesDir, "build")))

	ready, err := kernelHeadersAlreadyInstalled(kernel, paths)
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestKernelHeadersAlreadyInstalledSymlinkMismatch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	kernel := "test-kernel"
	paths := kernelHeadersPaths{
		libModulesDir: filepath.Join(root, "lib/modules"),
		usrSrcDir:     filepath.Join(root, "usr/src"),
	}

	headersDir := filepath.Join(paths.usrSrcDir, "linux-headers-"+kernel)
	modulesDir := filepath.Join(paths.libModulesDir, kernel)
	require.NoError(t, os.MkdirAll(headersDir, 0755))
	require.NoError(t, os.MkdirAll(modulesDir, 0755))
	require.NoError(t, os.Symlink("/usr/src/linux-headers-other", filepath.Join(modulesDir, "build")))

	ready, err := kernelHeadersAlreadyInstalled(kernel, paths)
	require.NoError(t, err)
	assert.False(t, ready)
}

func TestWriteKernelHeadersStatus(t *testing.T) {
	t.Parallel()

	statusPath := filepath.Join(t.TempDir(), "run/hypeman/kernel-headers.status")
	require.NoError(t, writeKernelHeadersStatus(statusPath, headersStatusRunning))

	data, err := os.ReadFile(statusPath)
	require.NoError(t, err)
	assert.Equal(t, "running\n", string(data))
}
