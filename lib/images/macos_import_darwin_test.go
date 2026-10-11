//go:build darwin && arm64

package images

import (
	"context"
	"encoding/json"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestMacOSOfflineImport(t *testing.T) {
	source := t.TempDir()
	p := paths.New(t.TempDir())
	c := MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30}
	b, _ := json.Marshal(c)
	require.NoError(t, os.WriteFile(filepath.Join(source, "config.json"), b, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "disk.img"), []byte("boot disk"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "aux.img"), []byte("aux storage"), 0600))
	img, err := ImportMacOSImage(context.Background(), p, "localhost/macos:test", source)
	require.NoError(t, err)
	require.Equal(t, "darwin/arm64", img.Platform)
	require.Equal(t, c.MachineIdentifier, img.MacOS.MachineIdentifier)
	m := &manager{paths: p}
	loaded, err := m.GetImage(context.Background(), img.Name)
	require.NoError(t, err)
	require.NotNil(t, loaded.MacOS)
	disk, err := GetDiskPath(p, img.Name, img.Digest)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(filepath.Dir(disk), "aux.img"))
	require.NoError(t, err)
	require.Equal(t, "aux storage", string(got))
	_, err = m.TagImage(context.Background(), img.Name, "localhost/alias:tag")
	require.ErrorIs(t, err, ErrInvalidPlatform)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ImportMacOSImage(ctx, p, "localhost/macos:cancelled", source)
	require.Error(t, err)
}
func TestMacOSPlatformLocalOnly(t *testing.T) {
	p, err := ParsePlatform("darwin/arm64")
	require.NoError(t, err)
	require.Equal(t, "darwin", p.OS)
	_, err = ParsePlatform("darwin/amd64")
	require.Error(t, err)
	_, err = resolveManifestPlatform(&containerMetadata{OS: "darwin", Architecture: "arm64"}, "")
	require.ErrorIs(t, err, ErrInvalidPlatform)
}
