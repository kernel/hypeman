package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/onkernel/hypeman/lib/images"
)

// createConfigDisk generates an erofs disk with instance configuration
// The disk contains:
// - /config.sh - Shell script sourced by init
// - /metadata.json - JSON metadata for programmatic access
func (m *manager) createConfigDisk(inst *Instance, imageInfo *images.Image) error {
	// Create temporary directory for config files
	tmpDir, err := os.MkdirTemp("", "hypeman-config-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Generate config.sh
	configScript := m.generateConfigScript(inst, imageInfo)
	configPath := filepath.Join(tmpDir, "config.sh")
	if err := os.WriteFile(configPath, []byte(configScript), 0644); err != nil {
		return fmt.Errorf("write config.sh: %w", err)
	}

	// Generate metadata.json
	configMeta := map[string]interface{}{
		"instance_id":   inst.Id,
		"instance_name": inst.Name,
		"image":         inst.Image,
		"entrypoint":    imageInfo.Entrypoint,
		"cmd":           imageInfo.Cmd,
		"workdir":       imageInfo.WorkingDir,
		"env":           mergeEnv(imageInfo.Env, inst.Env),
	}
	metaData, err := json.MarshalIndent(configMeta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	metaPath := filepath.Join(tmpDir, "metadata.json")
	if err := os.WriteFile(metaPath, metaData, 0644); err != nil {
		return fmt.Errorf("write metadata.json: %w", err)
	}

	// Create ext4 disk with config files
	// Use ext4 for now (can switch to erofs when kernel supports it)
	diskPath := m.paths.InstanceConfigDisk(inst.Id)
	
	// Calculate size (config files are tiny, use 1MB minimum)
	_, err = images.ExportRootfs(tmpDir, diskPath, images.FormatExt4)
	if err != nil {
		return fmt.Errorf("create config disk: %w", err)
	}

	return nil
}

// generateConfigScript creates the shell script that will be sourced by init
func (m *manager) generateConfigScript(inst *Instance, imageInfo *images.Image) string {
	// Prepare entrypoint value
	entrypoint := ""
	if len(imageInfo.Entrypoint) > 0 {
		entrypoint = shellQuoteArray(imageInfo.Entrypoint)
	}
	
	// Prepare cmd value
	cmd := ""
	if len(imageInfo.Cmd) > 0 {
		cmd = shellQuoteArray(imageInfo.Cmd)
	}
	
	// Prepare workdir value
	workdir := shellQuote("/")
	if imageInfo.WorkingDir != "" {
		workdir = shellQuote(imageInfo.WorkingDir)
	}
	
	// Build environment variable exports
	var envLines strings.Builder
	mergedEnv := mergeEnv(imageInfo.Env, inst.Env)
	for key, value := range mergedEnv {
		envLines.WriteString(fmt.Sprintf("export %s=%s\n", key, shellQuote(value)))
	}
	
	// Build network configuration section
	networkSection := ""
	if inst.Network != "" {
		// Get allocation from network manager
		alloc, err := m.networkManager.GetAllocation(context.Background(), inst.Id)
		if err == nil && alloc != nil {
			// Get network details for gateway and DNS
			network, err := m.networkManager.GetNetwork(context.Background(), inst.Network)
			if err == nil {
				// Parse netmask from subnet
				_, ipNet, err := net.ParseCIDR(network.Subnet)
				// TODO @sjmiller609 review: why do we assume /24 from instance manager, seems questionable?
				netmask := "255.255.255.0" // Default
				if err == nil {
					netmask = fmt.Sprintf("%d.%d.%d.%d",
						ipNet.Mask[0], ipNet.Mask[1], ipNet.Mask[2], ipNet.Mask[3])
				}

				networkSection = fmt.Sprintf(`
# Network configuration
GUEST_IP="%s"
GUEST_MASK="%s"
GUEST_GW="%s"
GUEST_DNS="%s"
`, alloc.IP, netmask, network.Gateway, network.Gateway)
			}
		}
	}
	
	// Generate script as a readable template block
	script := fmt.Sprintf(`#!/bin/sh
# Generated config for instance: %s

# Container execution parameters
ENTRYPOINT="%s"
CMD="%s"
WORKDIR=%s

# Environment variables
%s%s`, 
		inst.Id,
		entrypoint,
		cmd,
		workdir,
		envLines.String(),
		networkSection,
	)
	
	return script
}

// mergeEnv merges image environment variables with instance overrides
func mergeEnv(imageEnv map[string]string, instEnv map[string]string) map[string]string {
	result := make(map[string]string)

	// Start with image env
	for k, v := range imageEnv {
		result[k] = v
	}

	// Override with instance env
	for k, v := range instEnv {
		result[k] = v
	}

	return result
}

// shellQuote quotes a string for safe use in shell scripts
func shellQuote(s string) string {
	// Simple quoting: wrap in single quotes and escape single quotes
	s = strings.ReplaceAll(s, "'", "'\\''")
	return "'" + s + "'"
}

// shellQuoteArray quotes each element of an array for safe shell evaluation
// Each element is single-quoted to preserve special characters like semicolons
func shellQuoteArray(arr []string) string {
	if len(arr) == 0 {
		return "\"\""
	}

	quoted := make([]string, len(arr))
	for i, s := range arr {
		quoted[i] = shellQuote(s)
	}

	return strings.Join(quoted, " ")
}


