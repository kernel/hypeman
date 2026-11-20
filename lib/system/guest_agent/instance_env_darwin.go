//go:build darwin

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// configVolumeWait bounds the wait for the config volume. The agent starts once
// macOS has mounted the agent disk, so a config disk attached with it has been
// found by then; a guest created without env has none and gets an empty env.
const configVolumeWait = 10 * time.Second

// deliverInstanceEnv copies the env hypeman attached to this boot to guestEnvPath.
// Image services wait for a file written since boot, so a stale one is removed
// first and none is written if the config disk cannot be read. Values are not logged.
func deliverInstanceEnv() {
	if err := os.Remove(guestEnvPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[guest-agent] instance env: remove stale %s: %v", guestEnvPath, err)
	}
	env, err := readConfigVolumeEnv(configVolumeWait)
	if err != nil {
		log.Printf("[guest-agent] instance env: %v", err)
		return
	}
	if err := writeGuestEnv(guestEnvPath, env); err != nil {
		log.Printf("[guest-agent] instance env: write %s: %v", guestEnvPath, err)
		return
	}
	log.Printf("[guest-agent] instance env: wrote %d variables to %s", len(env), guestEnvPath)
}

func readConfigVolumeEnv(wait time.Duration) (map[string]string, error) {
	deadline := time.Now().Add(wait)
	for {
		info, err := diskInfo(configVolume)
		if err == nil {
			return readEnvFromConfigDisk(info)
		}
		if time.Now().After(deadline) {
			return map[string]string{}, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// readEnvFromConfigDisk reads the env from the config volume, mounting it read-only
// if macOS has not, then unmounts it: the volume is readable by every guest user
// while mounted, the copy is root's only.
func readEnvFromConfigDisk(info map[string]string) (map[string]string, error) {
	if err := checkConfigDisk(info); err != nil {
		return nil, err
	}
	dev := info["Device Node"]
	if info["Mounted"] != "Yes" {
		if out, err := exec.Command("/usr/sbin/diskutil", "mount", "readOnly", dev).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("mount %s: %w: %s", dev, err, out)
		}
		var err error
		if info, err = diskInfo(dev); err != nil {
			return nil, err
		}
	}
	defer func() {
		if out, err := exec.Command("/usr/sbin/diskutil", "unmount", dev).CombinedOutput(); err != nil {
			log.Printf("[guest-agent] instance env: unmount %s: %v: %s", dev, err, out)
		}
	}()
	mount := info["Mount Point"]
	if mount == "" {
		return nil, fmt.Errorf("%s is not mounted", dev)
	}
	data, err := os.ReadFile(filepath.Join(mount, configEnvFile))
	if err != nil {
		return nil, err
	}
	return parseEnv(data)
}

func diskInfo(target string) (map[string]string, error) {
	out, err := exec.Command("/usr/sbin/diskutil", "info", target).Output()
	if err != nil {
		return nil, fmt.Errorf("diskutil info %s: %w", target, err)
	}
	return parseDiskInfo(string(out)), nil
}
