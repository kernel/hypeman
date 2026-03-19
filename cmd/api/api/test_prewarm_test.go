package api

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kernel/hypeman/lib/images"
)

const (
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

func isTestPrewarmStrict() bool {
	v := strings.TrimSpace(os.Getenv(testPrewarmStrictEnv))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}
