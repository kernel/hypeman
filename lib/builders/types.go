// Package builders implements persistent cache resources used by disposable
// build VMs. A Builder is the unit of build-cache isolation; one build at a
// time may run on a Builder.
package builders

import (
	"time"

	"github.com/kernel/hypeman/lib/tags"
)

// Builder status constants
const (
	StatusReady    = "ready"
	StatusPruning  = "pruning"
	StatusDeleting = "deleting"
	StatusError    = "error"
)

const (
	// DefaultDiskSizeGb is the disk size used when a create request does
	// not specify one.
	DefaultDiskSizeGb = 50

	// diskVolumePrefix prefixes the deterministic volume ID of every
	// builder disk. The prefix is reserved from public volume APIs.
	diskVolumePrefix = "builder-disk-"

	// managedByTagValue marks volumes created by the builders manager.
	managedByTagValue = "builder"
)

// Builder is a first-class build-cache resource backed by a persistent disk.
type Builder struct {
	ID           string
	Name         string // optional, non-unique metadata
	DiskSizeGb   int
	Tags         tags.Tags
	Status       string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	DiskVolumeID string
}

// CreateBuilderRequest is the domain request for creating a builder
type CreateBuilderRequest struct {
	ID         *string // optional caller-supplied ID
	Name       string
	DiskSizeGb int // 0 uses the configured default; immutable after creation
	Tags       tags.Tags
}

// Config holds configuration for the builders manager
type Config struct {
	// MaxCount caps the number of builders (0 = unlimited)
	MaxCount int

	// DefaultDiskSizeGb is the disk size for creates that omit it
	// (<= 0 uses DefaultDiskSizeGb)
	DefaultDiskSizeGb int

	// MaxDiskSizeGb caps the requested disk size (0 = unlimited)
	MaxDiskSizeGb int

	// IdleTTL is how long a builder may go unused before it is deleted.
	// Deletion is total and irreversible. 0 disables the reaper.
	IdleTTL time.Duration
}

// DiskVolumeID returns the deterministic volume ID of a builder's disk.
func DiskVolumeID(builderID string) string {
	return diskVolumePrefix + builderID
}
