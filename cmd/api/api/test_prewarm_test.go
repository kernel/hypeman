package api

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPrewarmDirEnv    = "HYPEMAN_TEST_PREWARM_DIR"
	testPrewarmStrictEnv = "HYPEMAN_TEST_PREWARM_STRICT"
	testRegistryEnv      = "HYPEMAN_TEST_REGISTRY"
)

var apiRegistryLogOnce sync.Once
var apiRegistryLogMessage string

func apiTestImageRef(t *testing.T, source string) string {
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

	var mapped string
	switch {
	case ref.Tag() != "":
		mapped = registry + "/" + repo + ":" + ref.Tag()
	case ref.Digest() != "":
		mapped = registry + "/" + repo + "@" + ref.Digest()
	default:
		mapped = registry + "/" + repo + ":latest"
	}

	apiRegistryLogOnce.Do(func() {
		apiRegistryLogMessage = fmt.Sprintf("using test registry mirror source=%s mapped=%s", source, mapped)
	})
	if apiRegistryLogMessage != "" {
		t.Log(apiRegistryLogMessage)
	}
	return mapped
}

// prepareAPITestDataDir links the CI prewarm cache into a test data dir so
// each test reuses the kernels and initrd instead of downloading and building
// them from scratch.
func prepareAPITestDataDir(t *testing.T, dataDir string) {
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
	dstSystemDir := filepath.Join(dataDir, "system")
	if err := os.MkdirAll(dstSystemDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dstSystemDir, err)
	}
	linkAPITestSubdir(t, srcSystemDir, dstSystemDir, "kernel", true)
	linkAPITestSubdir(t, srcSystemDir, dstSystemDir, "initrd", true)
	linkAPITestSubdir(t, srcSystemDir, dstSystemDir, "binaries", runtime.GOOS == "linux")
	linkAPITestSubdir(t, srcSystemDir, dstSystemDir, "oci-cache", runtime.GOOS == "linux")
}

func linkAPITestSubdir(t *testing.T, srcSystemDir, dstSystemDir, subdir string, required bool) {
	t.Helper()

	src := filepath.Join(srcSystemDir, subdir)
	if _, err := os.Stat(src); err != nil {
		if required && isTestPrewarmStrict() {
			t.Fatalf("prewarm system subdir missing at %s: %v", src, err)
		}
		return
	}

	dst := filepath.Join(dstSystemDir, subdir)
	if err := os.Symlink(src, dst); err != nil && !os.IsExist(err) {
		t.Fatalf("symlink %s -> %s: %v", dst, src, err)
	}
}

func isTestPrewarmStrict() bool {
	v := strings.TrimSpace(os.Getenv(testPrewarmStrictEnv))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func TestPrepareAPITestDataDirLinksPrewarmCache(t *testing.T) {
	prewarmDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(prewarmDir, "prewarm-manifest.json"), []byte("{}"), 0o644))
	for _, subdir := range []string{"kernel", "initrd"} {
		require.NoError(t, os.MkdirAll(filepath.Join(prewarmDir, "system", subdir), 0o755))
	}
	t.Setenv(testPrewarmDirEnv, prewarmDir)

	dataDir := t.TempDir()
	prepareAPITestDataDir(t, dataDir)

	for _, subdir := range []string{"kernel", "initrd"} {
		target, err := os.Readlink(filepath.Join(dataDir, "system", subdir))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(prewarmDir, "system", subdir), target)
	}
}
