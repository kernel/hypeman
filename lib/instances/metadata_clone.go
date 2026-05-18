package instances

import "github.com/kernel/hypeman/lib/healthcheck"

// deepCopyMetadata returns a metadata copy that can be mutated without
// affecting the originally loaded instance metadata.
func deepCopyMetadata(src *metadata) *metadata {
	if src == nil {
		return nil
	}

	return &metadata{
		StoredMetadata:     cloneStoredMetadata(src.StoredMetadata),
		AutoStandbyRuntime: cloneAutoStandbyRuntime(src.AutoStandbyRuntime),
		HealthCheckRuntime: healthcheck.CloneRuntime(src.HealthCheckRuntime),
	}
}
