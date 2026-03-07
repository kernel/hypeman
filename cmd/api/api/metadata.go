package api

import (
	"encoding/json"
	"fmt"

	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/tags"
)

func toMapMetadata(metadata *oapi.MetadataTags) map[string]string {
	if metadata == nil {
		return nil
	}
	return tags.Clone(map[string]string(*metadata))
}

func toOAPIMetadata(metadata map[string]string) *oapi.MetadataTags {
	if len(metadata) == 0 {
		return nil
	}
	cloned := oapi.MetadataTags(tags.Clone(metadata))
	return &cloned
}

func matchesMetadataFilter(metadata map[string]string, filter *oapi.MetadataTags) bool {
	if filter == nil {
		return true
	}
	return tags.Matches(metadata, map[string]string(*filter))
}

func parseMetadataJSON(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	var metadata map[string]string
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return nil, fmt.Errorf("parse metadata JSON: %w", err)
	}
	return metadata, nil
}
