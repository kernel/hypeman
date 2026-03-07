package tags

import "regexp"

const (
	MaxEntries     = 50
	MinKeyLength   = 1
	MaxKeyLength   = 128
	MinValueLength = 0
	MaxValueLength = 256
)

// Allowed characters are aligned with the AWS-like strict metadata contract.
var allowedPattern = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]+$`)
