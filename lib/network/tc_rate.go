package network

import "fmt"

// formatTcRate formats bytes per second as a tc rate string.
// It uses the largest unit that exactly represents the value to avoid
// truncation from integer division (e.g., 2.5 Gbps becomes "2500mbit" not "2gbit").
func formatTcRate(bytesPerSec int64) string {
	bitsPerSec := bytesPerSec * 8
	switch {
	case bitsPerSec >= 1000000000 && bitsPerSec%1000000000 == 0:
		return fmt.Sprintf("%dgbit", bitsPerSec/1000000000)
	case bitsPerSec >= 1000000 && bitsPerSec%1000000 == 0:
		return fmt.Sprintf("%dmbit", bitsPerSec/1000000)
	case bitsPerSec >= 1000 && bitsPerSec%1000 == 0:
		return fmt.Sprintf("%dkbit", bitsPerSec/1000)
	default:
		return fmt.Sprintf("%dbit", bitsPerSec)
	}
}
