package images

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeToLocalRef(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "docker hub user image with tag",
			input:    "docker.io/onkernel/nodejs22-base:0.1.1",
			expected: "onkernel/nodejs22-base:0.1.1",
		},
		{
			name:     "docker hub user image without registry prefix",
			input:    "onkernel/nodejs22-base:0.1.1",
			expected: "onkernel/nodejs22-base:0.1.1",
		},
		{
			name:     "docker hub official image with tag",
			input:    "docker.io/library/alpine:3.21",
			expected: "library/alpine:3.21",
		},
		{
			name:     "docker hub official image short form",
			input:    "alpine:3.21",
			expected: "library/alpine:3.21",
		},
		{
			name:     "docker hub image with index.docker.io",
			input:    "index.docker.io/onkernel/nodejs22-base:0.1.1",
			expected: "onkernel/nodejs22-base:0.1.1",
		},
		{
			name:     "gcr.io image",
			input:    "gcr.io/google-containers/pause:3.2",
			expected: "gcr.io/google-containers/pause:3.2",
		},
		{
			name:     "ghcr.io image",
			input:    "ghcr.io/some-org/some-image:v1.0",
			expected: "ghcr.io/some-org/some-image:v1.0",
		},
		{
			name:     "image with latest tag",
			input:    "nginx:latest",
			expected: "library/nginx:latest",
		},
		{
			name:     "image without tag uses latest",
			input:    "nginx",
			expected: "library/nginx:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := name.ParseReference(tt.input)
			require.NoError(t, err)
			result := normalizeToLocalRef(ref)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestStripScheme(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://localhost:8080", "localhost:8080"},
		{"http://localhost:8080", "localhost:8080"},
		{"localhost:8080", "localhost:8080"},
		{"https://registry.example.com", "registry.example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := stripScheme(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// A digest-pinned FROM usually pins the multi-arch *index* digest (it is the
// digest `docker pull` prints), while the mirror pulls the platform-resolved
// image whose digest differs. Pushing that image under the index digest is
// refused by any registry that verifies content addresses — so an index named
// by digest must arrive at the local registry as the index itself, retrievable
// under exactly the digest the caller pinned.
//
// Tested at the pushMirrored seam: httptest registries live on 127.0.0.1:port,
// and a host:port cannot ride through normalizeToLocalRef as a path prefix the
// way docker.io/gcr.io sources do in production.
func TestPushMirroredPreservesIndexDigest(t *testing.T) {
	src := httptest.NewServer(registry.New())
	defer src.Close()
	dst := httptest.NewServer(registry.New())
	defer dst.Close()
	srcHost := strings.TrimPrefix(src.URL, "http://")
	dstHost := strings.TrimPrefix(dst.URL, "http://")

	// A two-manifest index, pushed to the source registry and then referenced
	// by its digest — the exact shape of a digest-pinned Docker Hub image.
	idx, err := random.Index(1024, 1, 2)
	require.NoError(t, err)
	indexDigest, err := idx.Digest()
	require.NoError(t, err)
	seed, err := name.ParseReference(srcHost + "/library/python:3.13-alpine")
	require.NoError(t, err)
	require.NoError(t, remote.WriteIndex(seed, idx))

	srcRef, err := name.ParseReference(srcHost + "/library/python@" + indexDigest.String())
	require.NoError(t, err)
	desc, err := remote.Get(srcRef)
	require.NoError(t, err)

	dstRef, err := name.ParseReference(dstHost + "/library/python@" + indexDigest.String())
	require.NoError(t, err)
	digest, err := pushMirrored(desc, true, dstRef)
	require.NoError(t, err)
	assert.Equal(t, indexDigest, digest,
		"the mirrored digest must be the digest the caller pinned")

	// The proof the old code could not give: the content is retrievable from
	// the destination under the pinned digest, i.e. it verified on push.
	mirrored, err := remote.Get(dstRef)
	require.NoError(t, err, "the pinned digest must resolve at the local registry")
	assert.Equal(t, indexDigest, mirrored.Digest)
	assert.True(t, mirrored.MediaType.IsIndex(), "an index must arrive as an index")
}

// The storage-saving path is unchanged: a tag reference mirrors only the
// (platform-resolved) image, not the whole index.
func TestPushMirroredByTagStillMirrorsImage(t *testing.T) {
	src := httptest.NewServer(registry.New())
	defer src.Close()
	dst := httptest.NewServer(registry.New())
	defer dst.Close()
	srcHost := strings.TrimPrefix(src.URL, "http://")
	dstHost := strings.TrimPrefix(dst.URL, "http://")

	img, err := random.Image(1024, 1)
	require.NoError(t, err)
	imgDigest, err := img.Digest()
	require.NoError(t, err)
	srcRef, err := name.ParseReference(srcHost + "/library/alpine:3.21")
	require.NoError(t, err)
	require.NoError(t, remote.Write(srcRef, img))

	desc, err := remote.Get(srcRef)
	require.NoError(t, err)
	dstRef, err := name.ParseReference(dstHost + "/library/alpine:3.21")
	require.NoError(t, err)
	digest, err := pushMirrored(desc, false, dstRef)
	require.NoError(t, err)
	assert.Equal(t, imgDigest, digest)

	mirrored, err := remote.Get(dstRef)
	require.NoError(t, err)
	assert.False(t, mirrored.MediaType.IsIndex())
}

// The tag path does not just avoid mirroring the whole index — it resolves a
// multi-platform index down to *the requested platform's* image. The single-
// platform test above cannot see this, because with one manifest there is
// nothing to pick between. Here the source tag names a two-platform index and
// the request asks for linux/arm64: the mirror must arrive as that arm64 image,
// not as the index and not as the other platform's amd64 image.
func TestPushMirroredByTagResolvesMultiPlatformIndexToRequestedPlatform(t *testing.T) {
	src := httptest.NewServer(registry.New())
	defer src.Close()
	dst := httptest.NewServer(registry.New())
	defer dst.Close()
	srcHost := strings.TrimPrefix(src.URL, "http://")
	dstHost := strings.TrimPrefix(dst.URL, "http://")

	// Two images with distinct digests (distinct sizes), each pinned to a
	// distinct platform in the index manifest — the shape of a real multi-arch
	// Docker Hub tag.
	amd64Img, err := random.Image(1024, 1)
	require.NoError(t, err)
	arm64Img, err := random.Image(2048, 1)
	require.NoError(t, err)
	amd64Digest, err := amd64Img.Digest()
	require.NoError(t, err)
	arm64Digest, err := arm64Img.Digest()
	require.NoError(t, err)
	require.NotEqual(t, amd64Digest, arm64Digest, "the platforms must be distinguishable")

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add:        amd64Img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
		},
		mutate.IndexAddendum{
			Add:        arm64Img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		},
	)
	srcRef, err := name.ParseReference(srcHost + "/library/multi:latest")
	require.NoError(t, err)
	require.NoError(t, remote.WriteIndex(srcRef, idx))

	// The production tag path: fetch the tag with a requested platform, then
	// mirror as a non-digest reference (isDigestRef=false).
	desc, err := remote.Get(srcRef, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "arm64"}))
	require.NoError(t, err)
	dstRef, err := name.ParseReference(dstHost + "/library/multi:latest")
	require.NoError(t, err)
	digest, err := pushMirrored(desc, false, dstRef)
	require.NoError(t, err)
	assert.Equal(t, arm64Digest, digest,
		"the tag path must resolve to the requested platform's image, not the index or another platform")
	assert.NotEqual(t, amd64Digest, digest, "it must not mirror the other platform")

	mirrored, err := remote.Get(dstRef)
	require.NoError(t, err)
	assert.False(t, mirrored.MediaType.IsIndex(),
		"a tagged multi-platform index must arrive as a single platform-resolved image")
	assert.Equal(t, arm64Digest, mirrored.Digest)
}
