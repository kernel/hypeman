package volumes

import (
	"time"

	"github.com/kernel/hypeman/lib/tags"
)

// AccessMode defines how a volume attachment can be accessed.
type AccessMode string

const (
	// AccessReadWriteOnce is exclusive read-write: only one instance at a time.
	AccessReadWriteOnce AccessMode = "ReadWriteOnce"
	// AccessReadOnlyMany allows read-only access from multiple instances.
	AccessReadOnlyMany AccessMode = "ReadOnlyMany"
	// AccessReadWriteMany allows shared read-write access via NFS.
	AccessReadWriteMany AccessMode = "ReadWriteMany"
)

// Attachment represents a volume attached to an instance
type Attachment struct {
	InstanceID string
	MountPath  string
	Readonly   bool
	NFS        bool // True if this attachment uses NFS (internal, not exposed in API)
}

// NFSInfo contains NFS serving details for a volume (host-internal).
type NFSInfo struct {
	Host       string // Host IP/address for NFS mount (gateway IP on VM bridge)
	ExportPath string // Exported filesystem path on the host
}

// Volume represents a persistent block storage volume
type Volume struct {
	Id          string
	Name        string
	SizeGb      int
	Tags        tags.Tags
	CreatedAt   time.Time
	Attachments []Attachment // List of current attachments (empty if not attached)
	NFS         *NFSInfo     // Non-nil when the volume is being served via NFS (internal)
}

// CreateVolumeRequest is the domain request for creating a volume
type CreateVolumeRequest struct {
	Name   string
	SizeGb int
	Id     *string // Optional custom ID
	Tags   tags.Tags
}

// AttachVolumeRequest is the domain request for attaching a volume to an instance
type AttachVolumeRequest struct {
	InstanceID string
	MountPath  string
	Readonly   bool
	AccessMode AccessMode // If set, takes precedence over Readonly
}

// ResolveAccessMode returns the effective access mode, applying field precedence rules.
// If AccessMode is set, it wins. Otherwise, Readonly maps to legacy behavior:
//   - readonly=true  → ReadOnlyMany
//   - readonly=false → ReadWriteOnce
func (r *AttachVolumeRequest) ResolveAccessMode() AccessMode {
	if r.AccessMode != "" {
		return r.AccessMode
	}
	if r.Readonly {
		return AccessReadOnlyMany
	}
	return AccessReadWriteOnce
}

// CreateVolumeFromArchiveRequest is the domain request for creating a volume
// pre-populated with content from a tar.gz archive
type CreateVolumeFromArchiveRequest struct {
	Name   string
	SizeGb int     // Maximum size in GB (extraction fails if content exceeds this)
	Id     *string // Optional custom ID
	Tags   tags.Tags
}
