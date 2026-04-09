package volumes

import (
	"time"

	"github.com/kernel/hypeman/lib/tags"
)

// AccessMode describes how a volume can be accessed by instances.
type AccessMode string

const (
	// AccessModeReadWriteOnce allows a single instance to mount the volume read-write.
	// This is the default for block-backed volumes.
	AccessModeReadWriteOnce AccessMode = "ReadWriteOnce"

	// AccessModeReadOnlyMany allows multiple instances to mount the volume read-only.
	AccessModeReadOnlyMany AccessMode = "ReadOnlyMany"

	// AccessModeReadWriteMany allows multiple instances to mount the volume read-write
	// simultaneously. Requires an NFS backing store.
	AccessModeReadWriteMany AccessMode = "ReadWriteMany"
)

// NFSConfig contains NFS server connection details for ReadWriteMany volumes.
type NFSConfig struct {
	Server     string // NFS server hostname or IP address
	ExportPath string // NFS export path (e.g., "/exports/shared-data")
	Version    string // NFS version (e.g., "4.1"); defaults to "4.1" if empty
	Options    string // Additional NFS mount options (e.g., "hard,timeo=600")
}

// Attachment represents a volume attached to an instance
type Attachment struct {
	InstanceID string
	MountPath  string
	Readonly   bool
}

// Volume represents a persistent block storage volume
type Volume struct {
	Id          string
	Name        string
	SizeGb      int
	AccessMode  AccessMode // Volume access mode (default: ReadWriteOnce)
	NFS         *NFSConfig // NFS configuration (only set when AccessMode is ReadWriteMany)
	Tags        tags.Tags
	CreatedAt   time.Time
	Attachments []Attachment // List of current attachments (empty if not attached)
}

// CreateVolumeRequest is the domain request for creating a volume
type CreateVolumeRequest struct {
	Name       string
	SizeGb     int
	AccessMode AccessMode // Optional; defaults to ReadWriteOnce
	NFS        *NFSConfig // Required when AccessMode is ReadWriteMany
	Id         *string    // Optional custom ID
	Tags       tags.Tags
}

// AttachVolumeRequest is the domain request for attaching a volume to an instance
type AttachVolumeRequest struct {
	InstanceID string
	MountPath  string
	Readonly   bool
}

// CreateVolumeFromArchiveRequest is the domain request for creating a volume
// pre-populated with content from a tar.gz archive
type CreateVolumeFromArchiveRequest struct {
	Name   string
	SizeGb int     // Maximum size in GB (extraction fails if content exceeds this)
	Id     *string // Optional custom ID
	Tags   tags.Tags
}

// ValidAccessMode returns true if mode is a recognized AccessMode value.
func ValidAccessMode(mode AccessMode) bool {
	switch mode {
	case AccessModeReadWriteOnce, AccessModeReadOnlyMany, AccessModeReadWriteMany:
		return true
	default:
		return false
	}
}
