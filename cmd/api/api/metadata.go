package api

import (
	"encoding/json"
	"fmt"

	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/tags"
)

func toMapTags(resourceTags *oapi.Tags) map[string]string {
	if resourceTags == nil {
		return nil
	}
	return tags.Clone(map[string]string(*resourceTags))
}

func toOAPITags(resourceTags map[string]string) *oapi.Tags {
	if len(resourceTags) == 0 {
		return nil
	}
	cloned := oapi.Tags(tags.Clone(resourceTags))
	return &cloned
}

func matchesTagsFilter(resourceTags map[string]string, filter *oapi.Tags) bool {
	if filter == nil {
		return true
	}
	return tags.Matches(resourceTags, map[string]string(*filter))
}

func parseTagsJSON(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	var resourceTags map[string]string
	if err := json.Unmarshal([]byte(raw), &resourceTags); err != nil {
		return nil, fmt.Errorf("parse tags JSON: %w", err)
	}
	return resourceTags, nil
}
