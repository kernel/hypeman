package guestmemory

// KernelPageInitMode controls guest kernel page initialization behavior.
type KernelPageInitMode string

const (
	// KernelPageInitPerformance minimizes guest page touching to preserve lazy host allocation.
	KernelPageInitPerformance KernelPageInitMode = "performance"
	// KernelPageInitHardened enforces page init-on-alloc/free hardening in the guest kernel.
	KernelPageInitHardened KernelPageInitMode = "hardened"
)

// Policy is the normalized, hypervisor-agnostic guest memory policy.
type Policy struct {
	Enabled            bool
	KernelPageInitMode KernelPageInitMode
	ReclaimEnabled     bool
	VZBalloonRequired  bool
}

// Features are generic guest memory toggles consumed by hypervisor backends.
type Features struct {
	EnableBalloon     bool
	FreePageReporting bool
	DeflateOnOOM      bool
	FreePageHinting   bool
	RequireBalloon    bool
}

// DefaultPolicy returns conservative defaults (disabled reclaim, hardened page-init mode).
func DefaultPolicy() Policy {
	return Policy{
		Enabled:            false,
		KernelPageInitMode: KernelPageInitHardened,
		ReclaimEnabled:     true,
		VZBalloonRequired:  true,
	}
}

// Normalize applies defaults and sanitizes invalid modes.
func (p Policy) Normalize() Policy {
	d := DefaultPolicy()

	if p.KernelPageInitMode == "" {
		p.KernelPageInitMode = d.KernelPageInitMode
	}
	if p.KernelPageInitMode != KernelPageInitPerformance && p.KernelPageInitMode != KernelPageInitHardened {
		p.KernelPageInitMode = d.KernelPageInitMode
	}

	if !p.Enabled {
		return Policy{
			Enabled:            false,
			KernelPageInitMode: p.KernelPageInitMode,
			ReclaimEnabled:     false,
			VZBalloonRequired:  p.VZBalloonRequired,
		}
	}

	return p
}

// KernelArgs returns kernel args implied by the policy.
func (p Policy) KernelArgs() []string {
	n := p.Normalize()
	if !n.Enabled {
		return nil
	}

	switch n.KernelPageInitMode {
	case KernelPageInitHardened:
		return []string{"init_on_alloc=1", "init_on_free=1"}
	default:
		return []string{"init_on_alloc=0", "init_on_free=0"}
	}
}

// FeaturesForHypervisor returns generic memory features for backend translation.
func (p Policy) FeaturesForHypervisor() Features {
	n := p.Normalize()
	if !n.Enabled || !n.ReclaimEnabled {
		return Features{}
	}

	return Features{
		EnableBalloon:     true,
		FreePageReporting: true,
		DeflateOnOOM:      true,
		FreePageHinting:   true,
		RequireBalloon:    n.VZBalloonRequired,
	}
}
