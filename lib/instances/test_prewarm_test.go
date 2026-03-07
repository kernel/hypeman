package instances

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/kernel/hypeman/lib/images"
)

const (
	testPrewarmDirEnv    = "HYPEMAN_TEST_PREWARM_DIR"
	testPrewarmStrictEnv = "HYPEMAN_TEST_PREWARM_STRICT"
	testRegistryEnv      = "HYPEMAN_TEST_REGISTRY"
)

var prewarmLogOnce sync.Once
var registryLogOnce sync.Once

func integrationTestImageRef(t *testing.T, source string) string {
	t.Helper()

	registry := strings.TrimSpace(os.Getenv(testRegistryEnv))
	if registry == "" {
		if isTestPrewarmStrict() {
			t.Fatalf("%s is required when %s is enabled", testRegistryEnv, testPrewarmStrictEnv)
		}
		return source
	}

	registry = strings.TrimPrefix(strings.TrimPrefix(registry, "http://"), "https://")
	if registry == "" {
		t.Fatalf("%s must not be empty", testRegistryEnv)
	}

	ref, err := images.ParseNormalizedRef(source)
	if err != nil {
		t.Fatalf("parse source image ref %q: %v", source, err)
	}

	repo := ref.Repository()
	if !strings.HasPrefix(repo, "docker.io/") {
		return source
	}
	repo = strings.TrimPrefix(repo, "docker.io/")

	if ref.Tag() != "" {
		mapped := registry + "/" + repo + ":" + ref.Tag()
		registryLogOnce.Do(func() {
			t.Logf("using test registry mirror source=%s mapped=%s", source, mapped)
		})
		return mapped
	}
	if ref.Digest() != "" {
		mapped := registry + "/" + repo + "@" + ref.Digest()
		registryLogOnce.Do(func() {
			t.Logf("using test registry mirror source=%s mapped=%s", source, mapped)
		})
		return mapped
	}

	mapped := registry + "/" + repo + ":latest"
	registryLogOnce.Do(func() {
		t.Logf("using test registry mirror source=%s mapped=%s", source, mapped)
	})
	return mapped
}

func prepareIntegrationTestDataDir(t *testing.T, tmpDir string) {
	t.Helper()

	prewarmDir := strings.TrimSpace(os.Getenv(testPrewarmDirEnv))
	if prewarmDir == "" {
		if isTestPrewarmStrict() {
			t.Fatalf("%s is required when %s is enabled", testPrewarmDirEnv, testPrewarmStrictEnv)
		}
		return
	}

	manifest := filepath.Join(prewarmDir, "prewarm-manifest.json")
	if _, err := os.Stat(manifest); err != nil {
		if isTestPrewarmStrict() {
			t.Fatalf("prewarm manifest missing at %s: %v", manifest, err)
		}
		return
	}

	srcSystemDir := filepath.Join(prewarmDir, "system")
	if _, err := os.Stat(srcSystemDir); err != nil {
		if isTestPrewarmStrict() {
			t.Fatalf("prewarm system directory missing at %s: %v", srcSystemDir, err)
		}
		return
	}

	dstSystemDir := filepath.Join(tmpDir, "system")
	if err := os.MkdirAll(dstSystemDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dstSystemDir, err)
	}
	linkSubdir(t, srcSystemDir, dstSystemDir, "kernel", true)
	linkSubdir(t, srcSystemDir, dstSystemDir, "initrd", true)
	linkSubdir(t, srcSystemDir, dstSystemDir, "binaries", runtime.GOOS == "linux")

	prewarmLogOnce.Do(func() {
		t.Logf("using prewarmed test cache dir=%s registry=%s", prewarmDir, os.Getenv(testRegistryEnv))
	})
}

func linkSubdir(t *testing.T, srcSystemDir, dstSystemDir, subdir string, required bool) {
	t.Helper()

	src := filepath.Join(srcSystemDir, subdir)
	if _, err := os.Stat(src); err != nil {
		if required && isTestPrewarmStrict() {
			t.Fatalf("prewarm system subdir missing at %s: %v", src, err)
		}
		return
	}

	dst := filepath.Join(dstSystemDir, subdir)
	if _, err := os.Lstat(dst); err == nil {
		return
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", dst, err)
	}

	if err := os.Symlink(src, dst); err != nil {
		t.Fatalf("symlink %s -> %s: %v", dst, src, err)
	}
}

func isTestPrewarmStrict() bool {
	v := strings.TrimSpace(os.Getenv(testPrewarmStrictEnv))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}
