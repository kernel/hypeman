package testsupport

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

// EnsureImageReady pre-warms a shared image cache under /tmp and seeds that
// image into the test data directory so instance integration tests don't need
// to repull/reconvert from scratch.
func EnsureImageReady(t *testing.T, ctx context.Context, p *paths.Paths, imageManager images.Manager, image string) {
	t.Helper()

	ref, err := images.ParseNormalizedRef(image)
	require.NoError(t, err)

	cachePaths := paths.New(filepath.Join(os.TempDir(), "hypeman-snapshot-image-cache"))
	cacheMgr, err := images.NewManager(cachePaths, 1, nil)
	require.NoError(t, err)

	prewarmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	created, err := cacheMgr.CreateImage(prewarmCtx, images.CreateImageRequest{Name: image})
	require.NoError(t, err)

	waitName := created.Name
	if created.Digest != "" {
		waitName = fmt.Sprintf("%s@%s", ref.Repository(), created.Digest)
	}
	require.NoError(t, cacheMgr.WaitForReady(prewarmCtx, waitName))

	cached, err := cacheMgr.GetImage(prewarmCtx, waitName)
	require.NoError(t, err)
	require.Equal(t, images.StatusReady, cached.Status)
	require.NotEmpty(t, cached.Digest)

	digestHex := strings.TrimPrefix(cached.Digest, "sha256:")
	require.NotEmpty(t, digestHex)

	srcDigestDir := cachePaths.ImageDigestDir(ref.Repository(), digestHex)
	dstDigestDir := p.ImageDigestDir(ref.Repository(), digestHex)
	require.NoError(t, copyDirWithHardlinks(srcDigestDir, dstDigestDir))

	if ref.Tag() != "" {
		linkPath := p.ImageTagSymlink(ref.Repository(), ref.Tag())
		require.NoError(t, os.MkdirAll(filepath.Dir(linkPath), 0755))
		_ = os.Remove(linkPath)
		require.NoError(t, os.Symlink(digestHex, linkPath))
	}

	reference := ref.Tag()
	if reference == "" {
		reference = cached.Digest
	}
	imported, err := imageManager.ImportLocalImage(ctx, ref.Repository(), reference, cached.Digest)
	require.NoError(t, err)

	if imported.Status != images.StatusReady {
		waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
		defer waitCancel()
		require.NoError(t, imageManager.WaitForReady(waitCtx, imported.Name))
	}
}

func copyDirWithHardlinks(srcDir, dstDir string) error {
	srcInfo, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if !srcInfo.IsDir() {
		return fmt.Errorf("source is not a directory: %s", srcDir)
	}
	if err := os.MkdirAll(dstDir, srcInfo.Mode().Perm()); err != nil {
		return err
	}

	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		dstPath := filepath.Join(dstDir, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.Mode().IsDir() {
			return os.MkdirAll(dstPath, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(dstPath)
			return os.Symlink(target, dstPath)
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		_ = os.Remove(dstPath)
		if err := os.Link(path, dstPath); err == nil {
			return nil
		}
		return copyRegularFile(path, dstPath, info.Mode().Perm())
	})
}

func copyRegularFile(src, dst string, perms fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perms)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
