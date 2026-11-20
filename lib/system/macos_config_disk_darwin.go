//go:build darwin

package system

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// BuildMacOSConfigDisk writes a read-only ISO 9660 disk at path holding env as
// MacOSConfigEnvFile. ISO is used because macOS mounts it natively and hdiutil
// makes one in milliseconds, where an APFS image takes seconds. The staging
// directory and the disk are readable by the hypeman user only.
func BuildMacOSConfigDisk(ctx context.Context, env map[string]string, path string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("encode macOS guest env: %w", err)
	}
	work, err := os.MkdirTemp(filepath.Dir(path), ".config-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")
	if err := os.Mkdir(src, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(src, MacOSConfigEnvFile), data, 0400); err != nil {
		return err
	}
	out := filepath.Join(work, "config.iso")
	cmd := exec.CommandContext(ctx, "/usr/bin/hdiutil", "makehybrid", "-quiet", "-iso", "-joliet", "-default-volume-name", MacOSConfigVolume, "-o", out, src)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build macOS config disk: %w: %s", err, output)
	}
	if err := os.Chmod(out, 0400); err != nil {
		return err
	}
	return os.Rename(out, path)
}
