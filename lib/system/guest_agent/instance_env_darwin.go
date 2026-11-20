//go:build darwin

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

// configMountWait bounds the wait for macOS to know the config disk well enough to
// mount it. Disk Arbitration can take several seconds after the agent starts.
const configMountWait = 2 * time.Minute

var wholeDisk = regexp.MustCompile(`^/dev/disk[0-9]+$`)

// deliverInstanceEnv copies the env hypeman attached to this boot to guestEnvPath.
// Image services wait for a file written since boot, so a stale one is removed
// first and none is written if the config disk cannot be read. A guest without a
// config disk gets an empty env. Values are not logged.
func deliverInstanceEnv() {
	if err := os.Remove(guestEnvPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[guest-agent] instance env: remove stale %s: %v", guestEnvPath, err)
	}
	env := map[string]string{}
	if dev := findConfigDisk(); dev != "" {
		var err error
		if env, err = readEnvFromConfigDisk(dev, configMountWait); err != nil {
			log.Printf("[guest-agent] instance env: %v", err)
			return
		}
	}
	if err := writeGuestEnv(guestEnvPath, env); err != nil {
		log.Printf("[guest-agent] instance env: write %s: %v", guestEnvPath, err)
		return
	}
	log.Printf("[guest-agent] instance env: wrote %d variables to %s", len(env), guestEnvPath)
}

// findConfigDisk returns the device of the config disk, found by reading each disk's
// ISO volume descriptor rather than through Disk Arbitration, which may not have
// seen it yet. The kernel has the disks hypeman attached before launchd starts the
// agent, so no disk means the guest has no env.
func findConfigDisk() string {
	devs, _ := filepath.Glob("/dev/disk*")
	for _, dev := range devs {
		if !wholeDisk.MatchString(dev) {
			continue
		}
		f, err := os.Open(dev)
		if err != nil {
			continue
		}
		pvd := make([]byte, 2048)
		_, err = f.ReadAt(pvd, isoVolumeDescriptorOffset)
		f.Close()
		if err == nil && isConfigISO(pvd) {
			return dev
		}
	}
	return ""
}

// readEnvFromConfigDisk reads the env from the config disk, mounting it read-only if
// macOS has not, then unmounts it: the volume is readable by every guest user while
// mounted, the copy is root's only.
func readEnvFromConfigDisk(dev string, wait time.Duration) (map[string]string, error) {
	deadline := time.Now().Add(wait)
	var info map[string]string
	var err error
	for {
		if info, err = mountedConfigDisk(dev); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if out, err := exec.Command("/usr/sbin/diskutil", "unmount", dev).CombinedOutput(); err != nil {
			log.Printf("[guest-agent] instance env: unmount %s: %v: %s", dev, err, out)
		}
	}()
	data, err := os.ReadFile(filepath.Join(info["Mount Point"], configEnvFile))
	if err != nil {
		return nil, err
	}
	return parseEnv(data)
}

// mountedConfigDisk checks the disk is hypeman's and returns its info once mounted.
func mountedConfigDisk(dev string) (map[string]string, error) {
	info, err := diskInfo(dev)
	if err != nil {
		return nil, err
	}
	if err := checkConfigDisk(info); err != nil {
		return nil, err
	}
	if info["Mounted"] != "Yes" {
		if out, err := exec.Command("/usr/sbin/diskutil", "mount", "readOnly", dev).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("mount %s: %w: %s", dev, err, out)
		}
		if info, err = diskInfo(dev); err != nil {
			return nil, err
		}
	}
	if info["Mount Point"] == "" {
		return nil, fmt.Errorf("%s is not mounted", dev)
	}
	return info, nil
}

func diskInfo(target string) (map[string]string, error) {
	out, err := exec.Command("/usr/sbin/diskutil", "info", target).Output()
	if err != nil {
		return nil, fmt.Errorf("diskutil info %s: %w", target, err)
	}
	return parseDiskInfo(string(out)), nil
}
