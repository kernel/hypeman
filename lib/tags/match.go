package tags

// Matches returns true when resource metadata satisfies all filter pairs.
func Matches(resource Metadata, filter Metadata) bool {
	if len(filter) == 0 {
		return true
	}
	if len(resource) == 0 {
		return false
	}
	for k, v := range filter {
		actual, ok := resource[k]
		if !ok || actual != v {
			return false
		}
	}
	return true
}
