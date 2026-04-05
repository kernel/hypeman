package instances

func cloneMetadata(src *metadata) *metadata {
	if src == nil {
		return nil
	}

	return &metadata{
		StoredMetadata: cloneStoredMetadataForFork(src.StoredMetadata),
	}
}
