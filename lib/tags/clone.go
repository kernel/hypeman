package tags

// Clone returns a deep copy of a tag map and normalizes empty maps to nil.
func Clone(resourceTags Tags) Tags {
	if len(resourceTags) == 0 {
		return nil
	}
	out := make(Tags, len(resourceTags))
	for k, v := range resourceTags {
		out[k] = v
	}
	return out
}
