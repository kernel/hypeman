package system

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGithubAPIRequestAddsAuthentication(t *testing.T) {
	req, err := githubAPIRequest(context.Background(), "https://api.github.com/repos/kernel/linux/releases/assets/1", "test-token", "application/octet-stream")
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-token", req.Header.Get("Authorization"))
	assert.Equal(t, "application/octet-stream", req.Header.Get("Accept"))
}

func TestGithubReleaseAsset(t *testing.T) {
	repo, tag, asset, ok := githubReleaseAsset("https://github.com/kernel/linux/releases/download/ch-6.12.8/kernel-headers-x86_64.tar.gz")
	require.True(t, ok)
	assert.Equal(t, "kernel/linux", repo)
	assert.Equal(t, "ch-6.12.8", tag)
	assert.Equal(t, "kernel-headers-x86_64.tar.gz", asset)

	_, _, _, ok = githubReleaseAsset("https://example.com/kernel/linux/releases/download/ch-6.12.8/kernel")
	assert.False(t, ok)
}
