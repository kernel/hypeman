package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequestedDiskReservationBytes(t *testing.T) {
	t.Parallel()

	diskBytes := requestedDiskReservationBytes(10, []VolumeAttachment{
		{VolumeID: "base-only", Overlay: false, OverlaySize: 100},
		{VolumeID: "overlay-a", Overlay: true, OverlaySize: 20},
		{VolumeID: "overlay-b", Overlay: true, OverlaySize: 30},
	})

	assert.Equal(t, int64(60), diskBytes)
}

func TestStoredDiskReservationBytes(t *testing.T) {
	t.Parallel()

	diskBytes := storedDiskReservationBytes(&StoredMetadata{
		OverlaySize: 15,
		Volumes: []VolumeAttachment{
			{VolumeID: "base-only", Overlay: false, OverlaySize: 100},
			{VolumeID: "overlay", Overlay: true, OverlaySize: 25},
		},
	})

	assert.Equal(t, int64(40), diskBytes)
}
