package instances

import (
	"fmt"
	"os"

	"github.com/kernel/hypeman/lib/images"
)

// MacOSBuildExportReceipt describes one strict stop, not persisted state.
type MacOSBuildExportReceipt struct{ Graceful, VMMExited bool }

// MacOSStorage is a stopped macOS guest's closed boot disk and platform: the
// read-only ASIF layers below its writable overlay, base first, the overlay, and its
// auxiliary storage. A standalone base disk is an Overlay with no Layers.
type MacOSStorage struct {
	Layers   []string
	Overlay  string
	Aux      string
	Platform images.MacOSImage
}

// Files returns the storage's paths: the overlay, the auxiliary storage, then the layers.
func (s MacOSStorage) Files() []string {
	return append([]string{s.Overlay, s.Aux}, s.Layers...)
}

// CheckFiles requires every storage file to be a distinct nonempty regular file.
func (s MacOSStorage) CheckFiles() error {
	var seen []os.FileInfo
	for _, path := range s.Files() {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("machine storage must be nonempty regular files")
		}
		for _, other := range seen {
			if os.SameFile(other, info) {
				return fmt.Errorf("machine storage files must be distinct")
			}
		}
		seen = append(seen, info)
	}
	return nil
}
