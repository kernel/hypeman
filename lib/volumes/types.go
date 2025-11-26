package volumes

import "time"

// Volume represents a persistent block storage volume
type Volume struct {
	Id        string
	Name      string
	SizeGb    int
	CreatedAt time.Time

	// Attachment state (nil/empty if not attached)
	AttachedTo *string // Instance ID if attached
	MountPath  *string // Mount path in guest if attached
	Readonly   bool    // Whether mounted read-only
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

