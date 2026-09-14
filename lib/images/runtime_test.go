package images

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSharedBaseDigestDependsOnOrderedBaseLayers(t *testing.T) {
	model := &imageManifestModel{
		BaseLayerCount: 2,
		Layers: []layerDescriptor{
			{Digest: "sha256:" + strings.Repeat("a", 64)},
			{Digest: "sha256:" + strings.Repeat("b", 64)},
			{Digest: "sha256:" + strings.Repeat("c", 64)},
		},
	}
	base := sharedBaseDigest(model)

	model.Layers[2].Digest = "sha256:" + strings.Repeat("d", 64)
	require.Equal(t, base, sharedBaseDigest(model))

	model.Layers[1].Digest = "sha256:" + strings.Repeat("d", 64)
	require.NotEqual(t, base, sharedBaseDigest(model))
	require.True(t, strings.HasPrefix(base, "sha256:"))
}

func TestValidateManifestModelRejectsInvalidSharedBase(t *testing.T) {
	digest := strings.Repeat("a", 64)
	model := &imageManifestModel{
		SchemaVersion:  manifestModelSchemaVersion,
		Digest:         "sha256:" + digest,
		RootFSType:     "layers",
		BaseDigest:     "sha256:" + strings.Repeat("b", 64),
		BaseLayerCount: 2,
		Config:         manifestConfigRef{Digest: "sha256:" + strings.Repeat("c", 64), MediaType: "application/vnd.oci.image.config.v1+json", DiffIDs: []string{"sha256:" + strings.Repeat("d", 64)}},
		Layers:         []layerDescriptor{{Digest: "sha256:" + strings.Repeat("e", 64), DiffID: "sha256:" + strings.Repeat("d", 64)}},
	}
	require.ErrorContains(t, validateManifestModel(digest, model), "base layer count")
}
