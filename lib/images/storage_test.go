package images

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestImageMetadataToImage_ClonesMetadata(t *testing.T) {
	createdAt := time.Now().UTC().Truncate(time.Second)
	source := &imageMetadata{
		Name:      "docker.io/library/alpine:latest",
		Digest:    "sha256:abc",
		Status:    StatusReady,
		Metadata:  map[string]string{"team": "backend", "env": "staging"},
		SizeBytes: 123,
		CreatedAt: createdAt,
	}

	img := source.toImage()
	require.Equal(t, source.Name, img.Name)
	require.Equal(t, source.Digest, img.Digest)
	require.Equal(t, map[string]string{"team": "backend", "env": "staging"}, img.Metadata)
	require.NotNil(t, img.SizeBytes)
	require.Equal(t, int64(123), *img.SizeBytes)

	source.Metadata["team"] = "mutated"
	require.Equal(t, "backend", img.Metadata["team"])
}

func TestImageMetadataToImage_EmptyMetadataOmitted(t *testing.T) {
	img := (&imageMetadata{
		Name:      "docker.io/library/alpine:latest",
		Digest:    "sha256:abc",
		Status:    StatusPending,
		CreatedAt: time.Now().UTC(),
	}).toImage()

	require.Nil(t, img.Metadata)
}
