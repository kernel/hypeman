package firecracker

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
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

func TestResolveBinaryPathConcurrentExtraction(t *testing.T) {
	SetCustomBinaryPath("")
	t.Cleanup(func() { SetCustomBinaryPath("") })

	arch, err := normalizeArch()
	require.NoError(t, err)
	embeddedPath := filepath.ToSlash(filepath.Join("binaries", "firecracker", string(defaultVersion), arch, "firecracker"))
	if _, err := binaryFS.ReadFile(embeddedPath); err != nil {
		t.Skipf("embedded binary %s not present in this checkout", embeddedPath)
	}

	p := paths.New(t.TempDir())

	const workers = 16
	results := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, err := resolveBinaryPath(p, "")
			results <- path
			errs <- err
		}()
	}

	wg.Wait()
	close(results)
	close(errs)

	var firstPath string
	for path := range results {
		if firstPath == "" {
			firstPath = path
			continue
		}
		assert.Equal(t, firstPath, path)
	}

	for err := range errs {
		require.NoError(t, err)
	}

	require.NotEmpty(t, firstPath)
	require.NoError(t, validateExecutable(firstPath))
}
