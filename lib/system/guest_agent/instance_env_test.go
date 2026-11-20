package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckConfigDiskRefusesDiskImages(t *testing.T) {
	info := parseDiskInfo("   Device Node:              /dev/disk5\n   Device Location:          Internal\n   Removable Media:          Fixed\n   Protocol:                 VirtIO\n")
	require.NoError(t, checkConfigDisk(info))
	info["Protocol"] = "Disk Image"
	require.Error(t, checkConfigDisk(info), "a disk image a guest user attached under the volume name is refused")
}

func TestWriteGuestEnvIsRootOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hypeman", "env.json")
	env, err := parseEnv([]byte(`{"HTTP_PROXY":"http://10.0.0.1:3128"}`))
	require.NoError(t, err)
	require.NoError(t, writeGuestEnv(path, env))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.JSONEq(t, `{"HTTP_PROXY":"http://10.0.0.1:3128"}`, string(data))
}
