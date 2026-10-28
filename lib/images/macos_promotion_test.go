package images

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func seedMacOSPromotion(t *testing.T, p *paths.Paths, repository, digest string) {
	t.Helper()
	seedLegacy(t, p, repository, "latest", digest)
	dir := p.ImageDigestDir(repository, digest)
	require.NoError(t, os.Remove(p.ImageDigestPath(repository, digest)))
	for _, file := range []string{machineDiskFile, "aux.img"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(file), 0600))
	}
	meta, err := readMetadata(p, repository, digest)
	require.NoError(t, err)
	meta.Platform = "darwin/arm64"
	meta.MacOS = &MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30}
	require.NoError(t, writeMetadata(p, repository, digest, meta))
}

func TestMacOSPromotionRetainsCompleteBundleUntilLastTag(t *testing.T) {
	p, m, source := newTagTestCase(t)
	digest := strings.Repeat("ab", 32)
	target := "registry.example/releases/macos"
	seedMacOSPromotion(t, p, source, digest)

	img, err := m.TagImage(context.Background(), source+":latest", target+":canary")
	require.NoError(t, err)
	require.Equal(t, "darwin/arm64", img.Platform)
	for _, repo := range []string{source, target} {
		got, err := m.GetImage(context.Background(), repo+"@sha256:"+digest)
		require.NoError(t, err)
		require.Equal(t, img.MacOS, got.MacOS)
		disk, err := GetDiskPath(p, got.Name, got.Digest)
		require.NoError(t, err)
		require.Equal(t, filepath.Join(p.ImageContentDir(digest), machineDiskFile), disk)
		require.FileExists(t, filepath.Join(filepath.Dir(disk), "aux.img"))
	}
	require.NoError(t, m.DeleteImage(context.Background(), source+":latest"))
	_, err = m.GetImage(context.Background(), target+":canary")
	require.NoError(t, err)
	require.NoError(t, m.DeleteImage(context.Background(), target+":canary"))
	require.NoDirExists(t, p.ImageContentDir(digest))
}

func TestMacOSPromotionRejectsIncompleteOrUnsafeStorage(t *testing.T) {
	for _, mutation := range []string{"missing", "symlink", "empty", "public", "directory"} {
		t.Run(mutation, func(t *testing.T) {
			p, m, source := newTagTestCase(t)
			digest := strings.Repeat("cd", 32)
			seedMacOSPromotion(t, p, source, digest)
			aux := filepath.Join(p.ImageDigestDir(source, digest), "aux.img")
			switch mutation {
			case "missing":
				require.NoError(t, os.Remove(aux))
			case "symlink":
				require.NoError(t, os.Rename(aux, aux+".real"))
				require.NoError(t, os.Symlink(aux+".real", aux))
			case "empty":
				require.NoError(t, os.Truncate(aux, 0))
			case "public":
				require.NoError(t, os.Chmod(aux, 0644))
			case "directory":
				require.NoError(t, os.Remove(aux))
				require.NoError(t, os.Mkdir(aux, 0700))
			}
			_, err := m.TagImage(context.Background(), source+":latest", "registry.example/releases/macos:canary")
			require.Error(t, err)
			require.NoDirExists(t, p.ImageContentDir(digest))
			_, err = resolveTag(p, "registry.example/releases/macos", "canary")
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}
