package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCacheRepo(t *testing.T) {
	cases := map[string]bool{
		"cache/global/node":                 true,
		"cache/tenant-x":                    true,
		"10.102.0.1:8083/cache/global/node": true,
		"builds/abc123":                     false,
		"docker.io/library/alpine":          false,
		"":                                  false,
		"prefixcache/foo":                   false,
	}
	for repo, want := range cases {
		assert.Equal(t, want, isCacheRepo(repo), "repo=%q", repo)
	}
}

func TestLiveCacheManifestDigestsTracksAndDeduplicates(t *testing.T) {
	r := &Registry{pushedTags: map[string]string{}}

	assert.Empty(t, r.LiveCacheManifestDigests(), "empty registry has no cache roots")

	r.recordPushedTag("cache/global/node", "v1", "sha256:aaa")
	r.recordPushedTag("cache/global/node", "v2", "sha256:bbb")
	// Two tags pointing at the same digest should yield one entry.
	r.recordPushedTag("cache/global/python", "v1", "sha256:aaa")

	got := r.LiveCacheManifestDigests()
	assert.ElementsMatch(t, []string{"sha256:aaa", "sha256:bbb"}, got)

	// Overwriting a tag replaces the old digest for that tag.
	r.recordPushedTag("cache/global/node", "v1", "sha256:ccc")
	got = r.LiveCacheManifestDigests()
	assert.ElementsMatch(t, []string{"sha256:aaa", "sha256:bbb", "sha256:ccc"}, got)
}
