package instances

// deepCopyMetadata returns a metadata copy that can be mutated without
// affecting the originally loaded instance metadata.
func deepCopyMetadata(src *metadata) *metadata {
	if src == nil {
		return nil
	}

	return &metadata{
		StoredMetadata: cloneStoredMetadataForFork(src.StoredMetadata),
	}
}
