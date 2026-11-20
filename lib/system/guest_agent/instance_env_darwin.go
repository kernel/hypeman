//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A macOS guest created with env gets it as env.json on a read-only ISO volume
// (system.BuildMacOSConfigDisk). On every boot the agent copies it to guestEnvPath,
// readable by root only, and unmounts the volume, which every guest user can read
// while mounted. A guest without the disk gets {}. Values are not logged.
const guestEnvPath = "/var/run/hypeman/env.json"

var (
	diskImage  = regexp.MustCompile(`(?m)^\s*Protocol:\s+Disk Image\s*$`)
	mountPoint = regexp.MustCompile(`(?m)^\s*Mount Point:\s+(\S.*?)\s*$`)
)

func deliverInstanceEnv() {
	env, err := readInstanceEnv()
	if err == nil {
		err = writeGuestEnv(env)
	}
	if err != nil {
		log.Printf("[guest-agent] instance env: %v", err)
		return
	}
	log.Printf("[guest-agent] instance env: wrote %d variables to %s", len(env), guestEnvPath)
}

// readInstanceEnv finds the config disk by the ISO volume descriptor on each raw
// device: Disk Arbitration can report it seconds after the agent starts, and a
// mounted volume's block device is busy. The kernel has every disk hypeman
// attached before launchd starts the agent.
func readInstanceEnv() (map[string]string, error) {
	env := map[string]string{}
	raws, _ := filepath.Glob("/dev/rdisk*")
	for _, raw := range raws {
		pvd := make([]byte, 2048)
		f, err := os.Open(raw)
		if err != nil {
			continue
		}
		_, err = f.ReadAt(pvd, 16*2048)
		f.Close()
		if err != nil || string(pvd[1:6]) != "CD001" || strings.TrimRight(string(pvd[40:72]), " \x00") != "HYPEMANCONFIG" {
			continue
		}
		dev := "/dev/" + strings.TrimPrefix(raw, "/dev/r")
		mount, err := mountReadOnly(dev)
		if err != nil {
			return nil, err
		}
		defer exec.Command("/usr/sbin/diskutil", "unmount", dev).Run()
		data, err := os.ReadFile(filepath.Join(mount, "env.json"))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, err
		}
		break
	}
	return env, nil
}

// mountReadOnly mounts the config disk read-only if macOS has not and returns its
// mount point. Like the agent launcher, it refuses a disk image a guest user
// attached under the same name.
func mountReadOnly(dev string) (string, error) {
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
		out, err := exec.Command("/usr/sbin/diskutil", "info", dev).Output()
		if err == nil && diskImage.Match(out) {
			return "", fmt.Errorf("refusing %s: it is a disk image, not hypeman's config disk", dev)
		}
		if m := mountPoint.FindSubmatch(out); err == nil && m != nil {
			return string(m[1]), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not mount", dev)
		}
		exec.Command("/usr/sbin/diskutil", "mount", "readOnly", dev).Run()
	}
}

func writeGuestEnv(env map[string]string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if err := os.WriteFile(guestEnvPath+".tmp", data, 0600); err != nil {
		return err
	}
	return os.Rename(guestEnvPath+".tmp", guestEnvPath)
}
