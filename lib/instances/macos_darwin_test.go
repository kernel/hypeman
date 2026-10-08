//go:build darwin

package instances

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestMacOSStorageClone(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "rootfs.raw")
	auxSource := filepath.Join(root, "aux.img")
	require.NoError(t, os.WriteFile(source, []byte("template disk"), 0644))
	require.NoError(t, os.WriteFile(auxSource, []byte("template aux"), 0644))
	probe := filepath.Join(root, "probe")
	err := unix.Clonefile(source, probe, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) {
		t.Skip("test filesystem does not support clonefile")
	}
	require.NoError(t, err)
	require.NoError(t, os.Remove(probe))

	t.Run("private writable copies", func(t *testing.T) {
		dest := t.TempDir()
		disk, aux := filepath.Join(dest, "disk"), filepath.Join(dest, "aux")
		require.NoError(t, cloneMacOSStorage(source, disk, aux))
		for path, expected := range map[string]string{disk: "template disk", aux: "template aux"} {
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, expected, string(data))
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			require.NoError(t, os.WriteFile(path, []byte("instance changes"), 0600))
		}
		for path, expected := range map[string]string{source: "template disk", auxSource: "template aux"} {
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, expected, string(data))
		}
	})

	t.Run("aux failure rolls back only new disk", func(t *testing.T) {
		dest := t.TempDir()
		disk, aux := filepath.Join(dest, "disk"), filepath.Join(dest, "aux")
		require.NoError(t, os.WriteFile(aux, []byte("existing aux"), 0600))
		require.Error(t, cloneMacOSStorage(source, disk, aux))
		_, err := os.Stat(disk)
		require.ErrorIs(t, err, os.ErrNotExist)
		data, err := os.ReadFile(aux)
		require.NoError(t, err)
		require.Equal(t, "existing aux", string(data))
	})

	t.Run("existing disk is preserved", func(t *testing.T) {
		dest := t.TempDir()
		disk, aux := filepath.Join(dest, "disk"), filepath.Join(dest, "aux")
		require.NoError(t, os.WriteFile(disk, []byte("existing disk"), 0600))
		require.Error(t, cloneMacOSStorage(source, disk, aux))
		data, err := os.ReadFile(disk)
		require.NoError(t, err)
		require.Equal(t, "existing disk", string(data))
		_, err = os.Stat(aux)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestMacOSLeaseNormalization(t *testing.T) {
	mac, err := net.ParseMAC("02:00:01:0a:0b:ff")
	require.NoError(t, err)
	data := "{\n ip_address=192.168.64.7\n hw_address=1,2:0:1:a:b:ff\n}\n"
	require.Equal(t, "192.168.64.7", macOSLeaseIP(data, mac))
	require.Empty(t, macOSLeaseIP("garbage", mac))
}
