package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// ensureBuildkitRoot prepares BuildKit's data directory. If root is already a
// mountpoint — e.g. a persistent disk attached by the host — it is used
// directly. Otherwise a tmpfs is mounted there: the VM rootfs is an overlayfs
// (read-only ext4 + writable ext4 upper layer) and BuildKit's native overlayfs
// snapshotter creates char device 0:0 for whiteout markers, but mknod(char 0:0)
// fails on an overlayfs mount because the kernel treats it as an overlayfs
// whiteout rather than a regular device node. tmpfs avoids this
// nested-overlayfs conflict.
//
// A pre-mounted root is shared state: only attach a volume there when builds
// are serialized per volume, never to two builder VMs at once.
func ensureBuildkitRoot(root string, requirePersistent bool, isMounted func(string) (bool, error), mountTmpfs func(string) error) error {
	mounted, err := isMounted(root)
	if err != nil {
		return fmt.Errorf("check mountpoint %s: %w", root, err)
	}
	if mounted {
		log.Printf("Using existing mount at %s for BuildKit", root)
		return nil
	}
	if requirePersistent {
		return fmt.Errorf("persistent BuildKit cache configured but %s is not mounted", root)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return fmt.Errorf("create buildkit root dir: %w", err)
	}
	return mountTmpfs(root)
}

func mountBuildkitTmpfs(root string) error {
	mountCmd := exec.Command("mount", "-t", "tmpfs", "-o", "size=3G", "tmpfs", root)
	if output, err := mountCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount tmpfs at %s (required for native overlayfs snapshotter): %v: %s", root, err, output)
	}
	log.Printf("Mounted tmpfs at %s for BuildKit snapshotter", root)
	return nil
}

// isMountPoint reports whether path is a mountpoint according to
// /proc/self/mounts, which reflects the guest's own mount namespace.
func isMountPoint(path string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false, err
	}
	return mountsContain(string(data), path), nil
}

func mountsContain(mounts, path string) bool {
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}
