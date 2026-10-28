package images

import (
	"fmt"
	"os"
	"path/filepath"
)

// validateStoredMacOSImage checks the complete cold-boot payload before it is
// reused or promoted. The platform configuration lives in image metadata.
func validateStoredMacOSImage(layout imageLayout, meta *imageMetadata) error {
	if meta.Platform != "darwin/arm64" || meta.MacOS.Validate() != nil {
		return fmt.Errorf("invalid stored macOS platform configuration")
	}
	if layout.disk != filepath.Join(layout.dir, machineDiskFile) {
		return fmt.Errorf("macOS image requires a raw boot disk")
	}
	for _, file := range []string{machineDiskFile, "aux.img"} {
		info, err := os.Lstat(filepath.Join(layout.dir, file))
		if err != nil {
			return fmt.Errorf("macOS image %s: %w", file, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Mode().Perm() != 0600 {
			return fmt.Errorf("macOS image %s must be a nonempty private regular file", file)
		}
	}
	return nil
}
