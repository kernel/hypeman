//go:build darwin

package system

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kernel/hypeman/lib/paths"
)

// DarwinGuestAgentBinary is the macOS guest agent, built by the Makefile.
//
//go:embed guest_agent/guest-agent-darwin
var DarwinGuestAgentBinary []byte

// macOSAgentDiskBuild serializes disk builds; waiting for one honors ctx.
var macOSAgentDiskBuild = make(chan struct{}, 1)

// MacOSAgentDisk returns the read-only guest agent disk for the embedded macOS
// agent, building it on first use. Disks are named by the agent's hash, so a new
// agent gets a new disk.
func MacOSAgentDisk(ctx context.Context, p *paths.Paths) (string, error) {
	sum := sha256.Sum256(DarwinGuestAgentBinary)
	version := hex.EncodeToString(sum[:6])
	path := p.SystemMacOSAgentDisk(version)
	select {
	case macOSAgentDiskBuild <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-macOSAgentDiskBuild }()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return path, buildMacOSAgentDisk(ctx, DarwinGuestAgentBinary, version, path)
}

// buildMacOSAgentDisk writes a raw APFS disk image at path holding the agent and a
// VERSION file.
func buildMacOSAgentDisk(ctx context.Context, binary []byte, version, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	work, err := os.MkdirTemp(filepath.Dir(path), ".build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")
	if err := os.Mkdir(src, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(src, MacOSAgentBinary), binary, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(src, "VERSION"), []byte(version+"\n"), 0644); err != nil {
		return err
	}
	// UDTO is a raw image, which Virtualization attaches as is. hdiutil appends .cdr.
	out := filepath.Join(work, "agent")
	cmd := exec.CommandContext(ctx, "/usr/bin/hdiutil", "create", "-quiet", "-srcfolder", src, "-volname", MacOSAgentVolume, "-fs", "APFS", "-format", "UDTO", out)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build macOS agent disk: %w: %s", err, output)
	}
	return os.Rename(out+".cdr", path)
}
