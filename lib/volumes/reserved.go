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
