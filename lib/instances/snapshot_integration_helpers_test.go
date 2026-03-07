package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

type prewarmedImageState struct {
	once   sync.Once
	digest string
	err    error
}

var prewarmedImages sync.Map // map[string]*prewarmedImageState

func ensureTestImageReady(t *testing.T, ctx context.Context, p *paths.Paths, imageManager images.Manager, image string) {
	t.Helper()

	ref, err := images.ParseNormalizedRef(image)
	require.NoError(t, err)

	digest, err := ensurePrewarmedImageDigest(image)
	require.NoError(t, err)

	require.NoError(t, seedImageIntoTestDataDir(p, ref.Repository(), ref.Tag(), digest))

	reference := ref.Tag()
	if reference == "" {
		reference = digest
	}
	img, err := imageManager.ImportLocalImage(ctx, ref.Repository(), reference, digest)
	require.NoError(t, err)

	if img.Status != images.StatusReady {
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		require.NoError(t, imageManager.WaitForReady(waitCtx, img.Name))
	}
}

func ensurePrewarmedImageDigest(image string) (string, error) {
	stateAny, _ := prewarmedImages.LoadOrStore(image, &prewarmedImageState{})
	state := stateAny.(*prewarmedImageState)

	state.once.Do(func() {
		cacheRoot := snapshotImageCacheRoot()
		cachePaths := paths.New(cacheRoot)

		ref, err := images.ParseNormalizedRef(image)
		if err != nil {
			state.err = err
			return
		}

		if digest, err := readReadyDigestFromCache(cachePaths, ref); err == nil {
			state.digest = digest
			return
		}

		if err := os.MkdirAll(cacheRoot, 0755); err != nil {
			state.err = fmt.Errorf("create snapshot image cache dir: %w", err)
			return
		}

		mgr, err := images.NewManager(cachePaths, 1, nil)
		if err != nil {
			state.err = fmt.Errorf("create cache image manager: %w", err)
			return
		}

		prewarmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		created, err := mgr.CreateImage(prewarmCtx, images.CreateImageRequest{Name: image})
		if err != nil {
			state.err = fmt.Errorf("prewarm create image: %w", err)
			return
		}

		waitName := created.Name
		if created.Digest != "" {
			waitName = fmt.Sprintf("%s@%s", ref.Repository(), created.Digest)
		}
		if err := mgr.WaitForReady(prewarmCtx, waitName); err != nil {
			if failed, getErr := mgr.GetImage(prewarmCtx, waitName); getErr == nil && failed.Error != nil {
				state.err = fmt.Errorf("wait for prewarmed image readiness: %w (%s)", err, *failed.Error)
				return
			}
			state.err = fmt.Errorf("wait for prewarmed image readiness: %w", err)
			return
		}

		img, err := mgr.GetImage(prewarmCtx, waitName)
		if err != nil {
			state.err = fmt.Errorf("get prewarmed image: %w", err)
			return
		}
		if img.Status != images.StatusReady || img.Digest == "" {
			state.err = fmt.Errorf("prewarmed image not ready (status=%s digest=%q)", img.Status, img.Digest)
			return
		}
		state.digest = img.Digest
	})

	return state.digest, state.err
}

func snapshotImageCacheRoot() string {
	return filepath.Join(os.TempDir(), "hypeman-snapshot-image-cache")
}

func readReadyDigestFromCache(cachePaths *paths.Paths, ref *images.NormalizedRef) (string, error) {
	repo := ref.Repository()
	var digestHex string

	if ref.IsDigest() {
		digestHex = ref.DigestHex()
	} else {
		linkTarget, err := os.Readlink(cachePaths.ImageTagSymlink(repo, ref.Tag()))
		if err != nil {
			return "", err
		}
		digestHex = linkTarget
	}
	if digestHex == "" {
		return "", fmt.Errorf("empty cached digest hex")
	}

	metaPath := cachePaths.ImageMetadata(repo, digestHex)
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return "", err
	}
	var meta struct {
		Status string `json:"status"`
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return "", err
	}
	if meta.Status != images.StatusReady || meta.Digest == "" {
		return "", fmt.Errorf("cached image not ready")
	}
	if _, err := os.Stat(cachePaths.ImageDigestPath(repo, digestHex)); err != nil {
		return "", err
	}
	return meta.Digest, nil
}

func seedImageIntoTestDataDir(testPaths *paths.Paths, repository, tag, digest string) error {
	cachePaths := paths.New(snapshotImageCacheRoot())
	digestHex := strings.TrimPrefix(digest, "sha256:")
	if digestHex == "" || digestHex == digest {
		return fmt.Errorf("invalid digest format %q", digest)
	}

	srcDigestDir := cachePaths.ImageDigestDir(repository, digestHex)
	dstDigestDir := testPaths.ImageDigestDir(repository, digestHex)
	if err := copyDirWithHardlinks(srcDigestDir, dstDigestDir); err != nil {
		return fmt.Errorf("seed digest dir: %w", err)
	}

	if tag != "" {
		linkPath := testPaths.ImageTagSymlink(repository, tag)
		if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
			return fmt.Errorf("create image repository dir: %w", err)
		}
		_ = os.Remove(linkPath)
		if err := os.Symlink(digestHex, linkPath); err != nil {
			return fmt.Errorf("write tag symlink: %w", err)
		}
	}
	return nil
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
		} else if !errors.Is(err, os.ErrExist) {
			if linkErr := copyRegularFile(path, dstPath, info.Mode().Perm()); linkErr != nil {
				return linkErr
			}
		}
		return nil
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

func waitForInstanceState(t *testing.T, ctx context.Context, mgr *manager, instanceID string, want State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		inst, err := mgr.GetInstance(ctx, instanceID)
		if err == nil && inst.State == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for instance %s to reach state %s", instanceID, want)
}
