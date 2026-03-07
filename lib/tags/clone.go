package tags

// Clone returns a deep copy of metadata map and normalizes empty maps to nil.
func Clone(metadata Metadata) Metadata {
	if len(metadata) == 0 {
		return nil
	}
	out := make(Metadata, len(metadata))
	for k, v := range metadata {
		out[k] = v
	}
	return out
}
