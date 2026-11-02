package instances

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Code-Hex/vz/v4/diskimagekit"
	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
)

// createMacOSStorage gives a macOS guest its own copy of an image's boot disk chain
// and auxiliary storage in dataDir, and an empty writable overlay above the chain.
// Copies are clones where the filesystem supports them. On failure only files this
// call created are removed.
func createMacOSStorage(chain []string, aux, dataDir string) (err error) {
	var created []string
	defer func() {
		if err != nil {
			for _, path := range created {
				os.Remove(path)
			}
		}
	}()
	copies := make([]string, len(chain))
	for i, layer := range chain {
		copies[i] = macOSDiskLayer(dataDir, i)
		if err = copyMacOSStorageFile(layer, copies[i], 0400, &created); err != nil {
			return err
		}
	}
	if err = copyMacOSStorageFile(aux, filepath.Join(dataDir, macOSAuxStorageFile), 0600, &created); err != nil {
		return err
	}
	img, err := images.OpenMacOSDiskChain(copies)
	if err != nil {
		return err
	}
	defer img.Close()
	overlay := filepath.Join(dataDir, paths.MacOSBootDiskFile)
	if _, err = os.Lstat(overlay); err == nil {
		return fmt.Errorf("macOS boot overlay already exists: %s", overlay)
	}
	created = append(created, overlay)
	if err = img.CreateLayer(overlay, diskimagekit.Overlay); err != nil {
		return fmt.Errorf("create macOS boot overlay: %w", err)
	}
	return os.Chmod(overlay, 0600)
}

func copyMacOSStorageFile(src, dst string, mode os.FileMode, created *[]string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("macOS storage file already exists: %s", dst)
	}
	if err := forkvm.CopyRegularFile(src, dst); err != nil {
		return err
	}
	*created = append(*created, dst)
	return os.Chmod(dst, mode)
}
