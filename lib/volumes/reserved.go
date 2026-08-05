package volumes

import "strings"

// SystemTagNamespace prefixes tag keys reserved for internal use. Public
// volume APIs reject caller-supplied tags in this namespace so callers
// cannot spoof ownership markers that internal managers rely on.
const SystemTagNamespace = "hypeman.system/"

// reservedVolumeIDPrefixes are volume ID prefixes reserved for volumes that
// internal managers create. Public volume APIs reject caller-supplied IDs
// with these prefixes so a caller cannot squat on or tamper with an
// internal volume.
var reservedVolumeIDPrefixes = []string{
	"builder-disk-",
}

// ReservedVolumeIDPrefix returns the reserved internal prefix id starts
// with, or "" when id is usable by API callers.
func ReservedVolumeIDPrefix(id string) string {
	for _, p := range reservedVolumeIDPrefixes {
		if strings.HasPrefix(id, p) {
			return p
		}
	}
	return ""
}

// ReservedTagNamespace returns the reserved internal namespace key starts
// with, or "" when key is usable by API callers.
func ReservedTagNamespace(key string) string {
	if strings.HasPrefix(key, SystemTagNamespace) {
		return SystemTagNamespace
	}
	return ""
}
