//go:build !darwin

package instances

import "fmt"

func cloneMacOSStorage(string, string, string, string) error {
	return fmt.Errorf("macOS image clones require APFS on macOS")
}
