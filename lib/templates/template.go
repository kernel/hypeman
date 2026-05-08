// Package templates models VM templates: tagged Standby instances promoted
// to "fork-only" parents whose snapshot can be reused for many forked
// instances. Templates are the foundation for one-snapshot-to-N-forks
// fan-out: rather than every fork copying or diffing against its own private
// snapshot, forks descend from a shared template. The actual sharing of
// memory and rootfs CoW between fork and template is implemented in the
// hypervisor and forkvm packages; this package just owns the lifecycle and
// indexing primitives.
package templates

import (
	"errors"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/tags"
)

// Common errors returned by the templates package.
var (
	ErrNotFound      = errors.New("template not found")
	ErrAlreadyExists = errors.New("template already exists")
	ErrInUse         = errors.New("template is in use by one or more forks")
	ErrInvalid       = errors.New("invalid template")
)

// Template is the persisted record describing a fork-only parent instance.
// It points at a source instance directory whose snapshot artifacts are
// shared by many forks, and tracks how many live forks reference it so we
// don't GC the underlying memory file or rootfs out from under them.
type Template struct {
	// ID is the template's stable identifier. It is independent of the
	// source instance ID so a template can outlive its source.
	ID string `json:"id"`

	// Name is a human-readable label, unique across templates.
	Name string `json:"name"`

	// SourceInstanceID is the instance the template was promoted from.
	// Its on-disk directory holds the canonical snapshot used by forks.
	SourceInstanceID string `json:"source_instance_id"`

	// Image is the OCI reference the source instance was created from.
	// Used for indexing templates by image when picking a fanout parent.
	Image string `json:"image,omitempty"`

	// ImageDigest is the resolved image digest (sha256:…) at the time of
	// promotion. Two templates with the same digest are interchangeable
	// for the purposes of fan-out pool selection.
	ImageDigest string `json:"image_digest,omitempty"`

	// HypervisorType records which hypervisor produced the snapshot.
	// Templates can only be forked by the same hypervisor type.
	HypervisorType hypervisor.Type `json:"hypervisor_type"`

	// HypervisorVersion is the hypervisor binary version used to take the
	// snapshot. Restoring on a different version may work but isn't
	// guaranteed; we store it so we can warn or refuse on mismatch.
	HypervisorVersion string `json:"hypervisor_version,omitempty"`

	// MemoryBytes is the guest memory size the snapshot was taken at.
	// Forks must be configured with at least this much memory.
	MemoryBytes int64 `json:"memory_bytes,omitempty"`

	// VCPUs is the vCPU count the snapshot was taken at. Snapshots are
	// vCPU-count-specific on most hypervisors.
	VCPUs int `json:"vcpus,omitempty"`

	// Tags carries arbitrary user metadata, e.g. release identifiers.
	Tags tags.Tags `json:"tags,omitempty"`

	// CreatedAt is when the template was first registered.
	CreatedAt time.Time `json:"created_at"`

	// LastUsedAt is updated whenever a fork is created from the template.
	// Useful as a proxy for popularity when GC-ing stale templates.
	LastUsedAt time.Time `json:"last_used_at,omitempty"`

	// ForkCount is the number of live forks descended from this template.
	// While > 0, the template (and its underlying snapshot files) must not
	// be deleted. PR 4 owns reference counting; PR 2 just records the field.
	ForkCount int `json:"fork_count"`

	// HotPagesPath optionally points at a baked "hot page list" used by
	// the UFFD page server to prefetch known-touched pages before resume.
	// PR 8 wires this in; PR 2 just reserves the field.
	HotPagesPath string `json:"hot_pages_path,omitempty"`
}

// Validate checks that required fields are populated.
func (t *Template) Validate() error {
	if t == nil {
		return errors.New("nil template")
	}
	if t.ID == "" {
		return errors.New("template id is required")
	}
	if t.Name == "" {
		return errors.New("template name is required")
	}
	if t.SourceInstanceID == "" {
		return errors.New("template source_instance_id is required")
	}
	if t.HypervisorType == "" {
		return errors.New("template hypervisor_type is required")
	}
	return nil
}
