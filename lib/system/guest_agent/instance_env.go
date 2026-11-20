package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A macOS guest created with env gets it on a read-only ISO volume, which the agent
// copies to a root-only file on every boot. These names match lib/system.
const (
	configVolume  = "HypemanConfig"
	configEnvFile = "env.json"
	guestEnvPath  = "/var/run/hypeman/env.json"
)

// parseDiskInfo reads the "Key: Value" lines of `diskutil info`.
func parseDiskInfo(out string) map[string]string {
	info := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if ok {
			info[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return info
}

// checkConfigDisk accepts only the disk hypeman attaches: a fixed internal device,
// not a disk image a guest user attached under the same name. The agent launcher
// applies the same test to the agent disk.
func checkConfigDisk(info map[string]string) error {
	if info["Device Location"] != "Internal" || info["Removable Media"] != "Fixed" || info["Protocol"] == "Disk Image" {
		return fmt.Errorf("refusing %s volume %s: it is not hypeman's attached config disk", configVolume, info["Device Node"])
	}
	if info["Device Node"] == "" {
		return fmt.Errorf("%s volume has no device node", configVolume)
	}
	return nil
}

// parseEnv decodes the config disk's env file, a JSON object of strings.
func parseEnv(data []byte) (map[string]string, error) {
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode %s: %w", configEnvFile, err)
	}
	if env == nil {
		env = map[string]string{}
	}
	return env, nil
}

// writeGuestEnv replaces path with env as a JSON object readable by root only.
func writeGuestEnv(path string, env map[string]string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".env-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
