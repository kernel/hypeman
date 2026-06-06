package uffdgraduate

import "context"

// Instance is a running VM that currently depends on a detachable snapshot
// memory pager. PagerVersion is the version it is bound to.
type Instance struct {
	ID           string
	Name         string
	PagerVersion string
}

// InstanceStore is the controller's view of the instance manager. It is kept
// narrow and free of hypervisor/UFFD types so the controller stays agnostic to
// how graduation is actually performed.
type InstanceStore interface {
	// ListPagerInstances returns running instances that still depend on a
	// detachable snapshot memory pager, each tagged with its bound version.
	ListPagerInstances(ctx context.Context) ([]Instance, error)
	// GraduateInstance detaches the instance from its pager. The call blocks
	// until the pager has populated remaining pages and released the session.
	GraduateInstance(ctx context.Context, id string) error
	// TargetVersion is the pager version new restores bind to. Sessions on a
	// different version are prioritised so old pager versions can retire.
	TargetVersion() string
}
