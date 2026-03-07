package tags

import (
	"fmt"
	"sort"
	"unicode/utf8"
)

// Validate enforces metadata constraints for all mutable resources.
func Validate(metadata Metadata) error {
	if len(metadata) == 0 {
		return nil
	}

	if len(metadata) > MaxEntries {
		return fmt.Errorf("%w: too many entries: %d (max %d)", ErrInvalidMetadata, len(metadata), MaxEntries)
	}

	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := metadata[key]

		keyLen := utf8.RuneCountInString(key)
		if keyLen < MinKeyLength || keyLen > MaxKeyLength {
			return fmt.Errorf("%w: key %q length %d out of range [%d,%d]", ErrInvalidMetadata, key, keyLen, MinKeyLength, MaxKeyLength)
		}
		if !allowedPattern.MatchString(key) {
			return fmt.Errorf("%w: key %q contains unsupported characters", ErrInvalidMetadata, key)
		}

		valueLen := utf8.RuneCountInString(value)
		if valueLen < MinValueLength || valueLen > MaxValueLength {
			return fmt.Errorf("%w: value for key %q length %d out of range [%d,%d]", ErrInvalidMetadata, key, valueLen, MinValueLength, MaxValueLength)
		}
		if valueLen > 0 && !allowedPattern.MatchString(value) {
			return fmt.Errorf("%w: value for key %q contains unsupported characters", ErrInvalidMetadata, key)
		}
	}

	return nil
}
