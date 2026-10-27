package hypervisor

import "context"

// SnapshotSupportValidator checks configuration-specific support before pausing.
type SnapshotSupportValidator interface {
	ValidateSnapshotSupport(context.Context) error
}
