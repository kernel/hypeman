package tags

import (
	"fmt"
	"sort"
	"unicode/utf8"
)

// Validate enforces tag constraints for all mutable resources.
func Validate(resourceTags Tags) error {
	if len(resourceTags) == 0 {
		return nil
	}

	if len(resourceTags) > MaxEntries {
		return fmt.Errorf("%w: too many entries: %d (max %d)", ErrInvalidTags, len(resourceTags), MaxEntries)
	}

	keys := make([]string, 0, len(resourceTags))
	for key := range resourceTags {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := resourceTags[key]

		keyLen := utf8.RuneCountInString(key)
		if keyLen < MinKeyLength || keyLen > MaxKeyLength {
			return fmt.Errorf("%w: key %q length %d out of range [%d,%d]", ErrInvalidTags, key, keyLen, MinKeyLength, MaxKeyLength)
		}
		if !allowedPattern.MatchString(key) {
			return fmt.Errorf("%w: key %q contains unsupported characters", ErrInvalidTags, key)
		}

		valueLen := utf8.RuneCountInString(value)
		if valueLen < MinValueLength || valueLen > MaxValueLength {
			return fmt.Errorf("%w: value for key %q length %d out of range [%d,%d]", ErrInvalidTags, key, valueLen, MinValueLength, MaxValueLength)
		}
		if valueLen > 0 && !allowedPattern.MatchString(value) {
			return fmt.Errorf("%w: value for key %q contains unsupported characters", ErrInvalidTags, key)
		}
	}

	return nil
}
