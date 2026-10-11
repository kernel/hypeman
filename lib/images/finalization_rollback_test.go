package images

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func TestFinalizationRollbackPreservesUninstalledManifest(t *testing.T) {
	for _, machine := range []bool{false, true} {
		name := "rootfs"
		if machine {
			name = "macos machine"
		}
		t.Run(name, func(t *testing.T) {
			p := paths.New(t.TempDir())
			m := &manager{paths: p}
			ref, err := ParseNormalizedRef("localhost/qa@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			require.NoError(t, err)
			resolved := NewResolvedRef(ref, ref.Digest())
			require.NoError(t, writeMetadata(p, ref.Repository(), ref.DigestHex(), &imageMetadata{BuildID: "qa-build", Status: StatusPending}))
			layout := resolveImageLayout(p, ref.Repository(), ref.DigestHex())
			modelPath := manifestModelPath(p, layout, ref.DigestHex())
			require.NoError(t, os.MkdirAll(filepath.Dir(modelPath), 0700))
			require.NoError(t, os.WriteFile(modelPath, []byte("preexisting-manifest"), 0600))
			staged := stagedImageFiles{disk: filepath.Join(layout.dir, "qa-staged-disk"), sizeBytes: 14}
			require.NoError(t, os.WriteFile(staged.disk, []byte("synthetic-disk"), 0600))
			config := &containerMetadata{OS: "linux", Architecture: "arm64"}
			if machine {
				staged.macos = &stagedMacOS{aux: filepath.Join(layout.dir, "qa-staged-aux"), platform: &MacOSImage{}}
				require.NoError(t, os.WriteFile(staged.macos.aux, []byte("synthetic-aux"), 0600))
				config = &containerMetadata{OS: "darwin", Architecture: "arm64"}
			}

			// The empty manifest model fails validation after the disk (and aux) are installed.
			err = m.finalizeImage(resolved, &pullResult{Metadata: config, Manifest: &imageManifestModel{}}, "qa-build", staged)
			require.ErrorContains(t, err, "write manifest model")
			data, err := os.ReadFile(modelPath)
			require.NoError(t, err, "rollback must not remove a manifest that this attempt did not install")
			require.Equal(t, "preexisting-manifest", string(data))
			require.NoFileExists(t, staged.finalDiskPath(layout), "the installed disk is rolled back")
			require.NoFileExists(t, filepath.Join(layout.dir, "aux.img"), "installed auxiliary storage is rolled back")
		})
	}
}
