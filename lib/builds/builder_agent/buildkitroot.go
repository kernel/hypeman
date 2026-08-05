package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
func ensureBuildkitRoot(root string, requirePersistent bool, isMounted func(string) (bool, error), mountTmpfs func(string) error) (bool, error) {
	mounted, err := isMounted(root)
	if err != nil {
		return false, fmt.Errorf("check mountpoint %s: %w", root, err)
	}
	if mounted {
		log.Printf("Using existing mount at %s for BuildKit", root)
		return true, nil
	}
	if requirePersistent {
		return false, fmt.Errorf("persistent BuildKit cache configured but %s is not mounted", root)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return false, fmt.Errorf("create buildkit root dir: %w", err)
	}
	return false, mountTmpfs(root)
}

// buildkitVersionStampFile records the BuildKit version that last wrote a
// persistent build root.
const buildkitVersionStampFile = ".hypeman-buildkit-version"

// buildkitdVersion returns the version stamp of the installed buildkitd.
func buildkitdVersion() (string, error) {
	output, err := exec.Command("buildkitd", "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("buildkitd --version: %v: %s", err, output)
	}
	return buildkitCompatibilityVersion(string(output))
}

func buildkitCompatibilityVersion(output string) (string, error) {
	for _, field := range strings.Fields(output) {
		if strings.HasPrefix(field, "v") && len(field) > 1 {
			return field, nil
		}
	}
	return "", fmt.Errorf("parse buildkitd version from %q", strings.TrimSpace(output))
}

// reconcileBuildkitVersionStamp resets a persistent BuildKit root written by
// an incompatible BuildKit version: cache contents are best-effort, so on a
// version mismatch the root is emptied before buildkitd starts rather than
// risking snapshotter corruption. A fresh disk (no stamp, nothing but
// lost+found) holds no cache and is stamped without a reset. The stamp is
// (re)written afterwards.
func reconcileBuildkitVersionStamp(root string, version func() (string, error)) error {
	current, err := version()
	if err != nil {
		return err
	}

	stampPath := filepath.Join(root, buildkitVersionStampFile)
	stamped, readErr := os.ReadFile(stampPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("read version stamp: %w", readErr)
	}
	if readErr == nil && strings.TrimSpace(string(stamped)) == current {
		return nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read buildkit root: %w", err)
	}

	if readErr == nil {
		log.Printf("BuildKit version changed (%s -> %s), resetting persistent cache at %s",
			strings.TrimSpace(string(stamped)), current, root)
		if err := resetRoot(root, entries); err != nil {
			return err
		}
	} else if freshDisk(entries) {
		// A fresh ext4 disk has no stamp but also no cache — only
		// lost+found — so it is stamped without a reset.
	} else {
		log.Printf("No BuildKit version stamp at %s, resetting persistent cache", root)
		if err := resetRoot(root, entries); err != nil {
			return err
		}
	}

	if err := os.WriteFile(stampPath, []byte(current+"\n"), 0644); err != nil {
		return fmt.Errorf("write version stamp: %w", err)
	}
	return nil
}

// freshDisk reports whether a BuildKit root holds no cache: empty, or only
// the lost+found directory of a fresh ext4 filesystem.
func freshDisk(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if entry.Name() != "lost+found" {
			return false
		}
	}
	return true
}

// resetRoot empties a BuildKit root whose contents cannot be trusted to
// match the current BuildKit version.
func resetRoot(root string, entries []os.DirEntry) error {
	for _, entry := range entries {
		if entry.Name() == "lost+found" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("reset buildkit root: remove %s: %w", entry.Name(), err)
		}
	}
	return nil
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
