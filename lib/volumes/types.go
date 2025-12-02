package volumes

import "time"

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
	CreatedAt   time.Time
	Attachments []Attachment // List of current attachments (empty if not attached)
}

// CreateVolumeRequest is the domain request for creating a volume
type CreateVolumeRequest struct {
	Name   string
	SizeGb int
	Id     *string // Optional custom ID
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
}

