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
	raw := make([]byte, 4<<20)
	copy(raw[1<<20:], "boot disk")
	require.NoError(t, os.WriteFile(filepath.Join(source, "disk.img"), raw, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "aux.img"), []byte("aux storage"), 0600))
	img, err := ImportMacOSImage(context.Background(), p, "localhost/macos:test", source)
	require.NoError(t, err)
	require.Equal(t, "darwin/arm64", img.Platform)
	require.Equal(t, c.MachineIdentifier, img.MacOS.MachineIdentifier)
	m := &manager{paths: p}
	loaded, err := m.GetImage(context.Background(), img.Name)
	require.NoError(t, err)
	require.NotNil(t, loaded.MacOS)
	chain, err := MacOSDiskChain(p, img.Name, img.Digest)
	require.NoError(t, err)
	require.Len(t, chain, 1, "a raw disk imports as a one-layer ASIF chain")
	disk, err := OpenMacOSDiskChain(chain)
	require.NoError(t, err)
	require.Equal(t, int64(len(raw)), disk.Size())
	buf := make([]byte, len("boot disk"))
	_, err = disk.ReadAt(buf, 1<<20)
	require.NoError(t, err)
	require.Equal(t, "boot disk", string(buf))
	require.NoError(t, disk.Close())
	got, err := os.ReadFile(filepath.Join(filepath.Dir(chain[0]), "aux.img"))
	require.NoError(t, err)
	require.Equal(t, "aux storage", string(got))
	_, err = m.TagImage(context.Background(), img.Name, "localhost/alias:tag")
	require.ErrorIs(t, err, ErrInvalidPlatform)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ImportMacOSImage(ctx, p, "localhost/macos:cancelled", source)
	require.Error(t, err)
}
func TestMacOSPlatform(t *testing.T) {
	p, err := ParsePlatform("darwin/arm64")
	require.NoError(t, err)
	require.Equal(t, "darwin", p.OS)
	_, err = ParsePlatform("darwin/amd64")
	require.Error(t, err)
	manifest, err := resolveManifestPlatform(&containerMetadata{OS: "darwin", Architecture: "arm64"}, "")
	require.NoError(t, err)
	require.Equal(t, "darwin/arm64", manifest.String())
}
