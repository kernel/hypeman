//go:build darwin

package system

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildMacOSAgentDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "macos-agent", "v1.img")
	require.NoError(t, buildMacOSAgentDisk(context.Background(), []byte("#!/bin/sh\n"), "v1", path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Zero(t, info.Size()%512, "a raw disk is whole sectors")
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the build directory is removed")
}
