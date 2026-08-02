package volumes

import "strings"

// managedVolumeIDPrefixes are volume ID prefixes reserved for volumes the
// build manager creates internally ("build-cache-" covers the per-scope
// BuildKit cache volumes). Public volume APIs reject caller-supplied IDs
// with these prefixes, and instance volume attachments reject them unless
// the request is internal, so a caller cannot squat on or tamper with an
// internal build volume.
var managedVolumeIDPrefixes = []string{
	"build-cache-",
	"build-config-",
	"build-disk-",
	"build-source-",
}

// ReservedVolumeIDPrefix returns the reserved internal prefix id starts
// with, or "" when id is usable by API callers.
func ReservedVolumeIDPrefix(id string) string {
	for _, p := range managedVolumeIDPrefixes {
		if strings.HasPrefix(id, p) {
			return p
		}
	}
	return ""
}

// SystemTagNamespace is the tag key namespace reserved for internal,
// server-managed metadata (e.g. the tag marking build cache volumes).
const SystemTagNamespace = "hypeman.system/"

// ReservedTagNamespace returns the reserved internal tag namespace key
// starts with, or "" when key is usable by API callers. Public APIs must
// reject caller-supplied tags in this namespace so a caller cannot
// impersonate an internally managed volume.
func ReservedTagNamespace(key string) string {
	if strings.HasPrefix(key, SystemTagNamespace) {
		return SystemTagNamespace
	}
	return ""
}
