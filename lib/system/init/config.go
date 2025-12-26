package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/onkernel/hypeman/lib/vmconfig"
)

// readConfig mounts and reads the config disk, parsing the JSON configuration.
func readConfig(log *Logger) (*vmconfig.Config, error) {
	const configMount = "/mnt/config"
	const configFile = "/mnt/config/config.json"

	// Create mount point
	if err := os.MkdirAll(configMount, 0755); err != nil {
		return nil, fmt.Errorf("mkdir config mount: %w", err)
	}

	// Mount config disk (/dev/vdc) read-only
	cmd := exec.Command("/bin/mount", "-o", "ro", "/dev/vdc", configMount)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("mount config disk: %s: %s", err, output)
	}
	log.Info("config", "mounted config disk")

	// Read and parse config.json
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg vmconfig.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config json: %w", err)
	}

	// Set defaults
	if cfg.InitMode == "" {
		cfg.InitMode = "exec"
	}
	if cfg.Env == nil {
		cfg.Env = make(map[string]string)
	}

	log.Info("config", "parsed configuration")
	return &cfg, nil
}
