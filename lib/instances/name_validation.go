package instances

import (
	"fmt"
	"regexp"
)

var instanceNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func validateInstanceName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(name) > 63 {
		return fmt.Errorf("name must be 63 characters or less")
	}
	if !instanceNamePattern.MatchString(name) {
		return fmt.Errorf("name must contain only lowercase letters, digits, and dashes; cannot start or end with a dash")
	}
	return nil
}
