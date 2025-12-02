package instances

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateVolumeAttachments_MaxVolumes(t *testing.T) {
	// Create 24 volumes (exceeds limit of 23)
	volumes := make([]VolumeAttachment, 24)
	for i := range volumes {
		volumes[i] = VolumeAttachment{
			VolumeID:  "vol-" + string(rune('a'+i)),
			MountPath: "/mnt/vol" + string(rune('a'+i)),
		}
	}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot attach more than 23 volumes")
}

func TestValidateVolumeAttachments_SystemDirectory(t *testing.T) {
	volumes := []VolumeAttachment{{
		VolumeID:  "vol-1",
		MountPath: "/etc/secrets", // system directory
	}}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "system directory")
}

func TestValidateVolumeAttachments_DuplicatePaths(t *testing.T) {
	volumes := []VolumeAttachment{
		{VolumeID: "vol-1", MountPath: "/mnt/data"},
		{VolumeID: "vol-2", MountPath: "/mnt/data"}, // duplicate
	}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate mount path")
}

func TestValidateVolumeAttachments_RelativePath(t *testing.T) {
	volumes := []VolumeAttachment{{
		VolumeID:  "vol-1",
		MountPath: "relative/path", // not absolute
	}}

	err := validateVolumeAttachments(volumes)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
}

func TestValidateVolumeAttachments_Valid(t *testing.T) {
	volumes := []VolumeAttachment{
		{VolumeID: "vol-1", MountPath: "/mnt/data"},
		{VolumeID: "vol-2", MountPath: "/mnt/logs"},
	}

	err := validateVolumeAttachments(volumes)
	assert.NoError(t, err)
}

func TestValidateVolumeAttachments_Empty(t *testing.T) {
	err := validateVolumeAttachments(nil)
	assert.NoError(t, err)

	err = validateVolumeAttachments([]VolumeAttachment{})
	assert.NoError(t, err)
}

