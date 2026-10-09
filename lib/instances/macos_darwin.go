//go:build darwin

package instances

import (
	"os"

	"golang.org/x/sys/unix"
)

func cloneMacOSStorage(rootDisk, rootAux, disk, aux string) (err error) {
	var created []string
	defer func() {
		if err != nil {
			// Only remove files this operation created, never pre-existing storage.
			for _, path := range created {
				os.Remove(path)
			}
		}
	}()
	for _, file := range []struct{ src, dst string }{
		{rootDisk, disk},
		{rootAux, aux},
	} {
		if err = unix.Clonefile(file.src, file.dst, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY); err != nil {
			return err
		}
		created = append(created, file.dst)
		if err = os.Chmod(file.dst, 0600); err != nil {
			return err
		}
	}
	return nil
}
