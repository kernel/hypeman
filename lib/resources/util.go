package resources

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/c2h5oh/datasize"
)

// ParseBandwidth parses a bandwidth string like "10Gbps", "1GB/s", "125MB/s".
// Handles both bit-based (bps) and byte-based (/s) formats.
// Returns bytes per second.
func ParseBandwidth(limit string) (int64, error) {
	limit = strings.TrimSpace(limit)
	limit = strings.ToLower(limit)

	// Handle bps variants (bits per second)
	if strings.HasSuffix(limit, "bps") {
		// Remove "bps" suffix
		numPart := strings.TrimSuffix(limit, "bps")
		numPart = strings.TrimSpace(numPart)

		// Check for multiplier prefix
		var multiplier int64 = 1
		if strings.HasSuffix(numPart, "g") {
			multiplier = 1000 * 1000 * 1000
			numPart = strings.TrimSuffix(numPart, "g")
		} else if strings.HasSuffix(numPart, "m") {
			multiplier = 1000 * 1000
			numPart = strings.TrimSuffix(numPart, "m")
		} else if strings.HasSuffix(numPart, "k") {
			multiplier = 1000
			numPart = strings.TrimSuffix(numPart, "k")
		}

		bits, err := strconv.ParseInt(strings.TrimSpace(numPart), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid number: %s", numPart)
		}

		// Convert bits to bytes
		return (bits * multiplier) / 8, nil
	}

	// Handle byte-based variants (e.g., "125MB/s", "1GB")
	limit = strings.TrimSuffix(limit, "/s")
	limit = strings.TrimSuffix(limit, "ps")

	var ds datasize.ByteSize
	if err := ds.UnmarshalText([]byte(limit)); err != nil {
		return 0, fmt.Errorf("parse as bytes: %w", err)
	}

	return int64(ds.Bytes()), nil
}
