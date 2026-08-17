package hypervisor

// Stable per-runtime feature IDs derived from Capabilities. These are part of
// the public capabilities API contract: clients gate behavior on these IDs
// rather than on hypervisor names. Adding a client-visible Capabilities field
// means adding its ID here, deriving it in FeatureIDs, and documenting it in
// the OpenAPI CapabilitiesRuntime schema — HTTP handlers never derive feature
// lists themselves.
const (
	// FeatureSnapshots: snapshot/restore of instance state.
	FeatureSnapshots = "snapshots"
	// FeatureStandby: pause + memory snapshot, with later restore.
	FeatureStandby = "standby"
	// FeatureFork: cloning an instance from a stopped source. Derived from
	// the explicit SupportsFork capability, which each backend keeps aligned
	// with its VMStarter.PrepareFork implementation. Fork of a standby or
	// running source additionally restores/creates snapshots, so clients
	// must gate those operations on FeatureStandby as well — vz on macOS 13
	// advertises fork (stopped-source clones need no machine-state
	// save/restore) without snapshots or standby.
	FeatureFork = "fork"
	// FeaturePause: pause/resume of a running instance.
	FeaturePause = "pause"
	// FeatureHotplugMemory: live memory resize.
	FeatureHotplugMemory = "hotplug-memory"
	// FeatureBalloonControl: runtime balloon target changes.
	FeatureBalloonControl = "balloon-control"
	// FeatureVsock: guest vsock communication.
	FeatureVsock = "vsock"
	// FeatureGPUPassthrough: GPU/PCI device passthrough.
	FeatureGPUPassthrough = "gpu-passthrough"
	// FeatureDiskIOLimit: disk I/O rate limiting.
	FeatureDiskIOLimit = "disk-io-limit"
	// FeatureDiskResize: live disk resize.
	FeatureDiskResize = "disk-resize"
)

// SupportsStandby reports whether standby is genuinely available. Standby
// pauses the VM and then snapshots its memory, so it requires both pause and
// snapshot support — neither alone is sufficient.
func (c Capabilities) SupportsStandby() bool {
	return c.SupportsSnapshot && c.SupportsPause
}

// FeatureIDs returns the stable feature IDs implied by this capability set,
// in a fixed deterministic order. Only client-visible features are included;
// internal lifecycle hints (snapshot base reuse, pager detachability, host
// snapshot version pinning, graceful VMM shutdown) are implementation details
// and deliberately have no IDs. The result is always non-nil so it serializes
// as an empty JSON array rather than null.
func (c Capabilities) FeatureIDs() []string {
	ids := make([]string, 0, 10)
	if c.SupportsSnapshot {
		ids = append(ids, FeatureSnapshots)
	}
	if c.SupportsStandby() {
		ids = append(ids, FeatureStandby)
	}
	if c.SupportsFork {
		ids = append(ids, FeatureFork)
	}
	if c.SupportsPause {
		ids = append(ids, FeaturePause)
	}
	if c.SupportsHotplugMemory {
		ids = append(ids, FeatureHotplugMemory)
	}
	if c.SupportsBalloonControl {
		ids = append(ids, FeatureBalloonControl)
	}
	if c.SupportsVsock {
		ids = append(ids, FeatureVsock)
	}
	if c.SupportsGPUPassthrough {
		ids = append(ids, FeatureGPUPassthrough)
	}
	if c.SupportsDiskIOLimit {
		ids = append(ids, FeatureDiskIOLimit)
	}
	if c.SupportsDiskResize {
		ids = append(ids, FeatureDiskResize)
	}
	return ids
}
