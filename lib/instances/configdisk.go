package instances

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/network"
)

// createConfigDisk generates an erofs disk with instance configuration
// The disk contains:
// - /config.sh - Shell script sourced by init
// - /metadata.json - JSON metadata for programmatic access
func (m *manager) createConfigDisk(inst *Instance, imageInfo *images.Image, netConfig *network.NetworkConfig) error {
	// Create temporary directory for config files
	tmpDir, err := os.MkdirTemp("", "hypeman-config-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Generate config.sh
	configScript := m.generateConfigScript(inst, imageInfo, netConfig)
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
func (m *manager) generateConfigScript(inst *Instance, imageInfo *images.Image, netConfig *network.NetworkConfig) string {
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
	// Use netConfig directly instead of trying to derive it (VM hasn't started yet)
	networkSection := ""
	if inst.NetworkEnabled && netConfig != nil {
		// Convert netmask to CIDR prefix length for ip command
		cidr := netmaskToCIDR(netConfig.Netmask)
		networkSection = fmt.Sprintf(`
# Network configuration
GUEST_IP="%s"
GUEST_CIDR="%d"
GUEST_GW="%s"
GUEST_DNS="%s"
`, netConfig.IP, cidr, netConfig.Gateway, netConfig.DNS)
	}
	
	// Generate script as a readable template block
	// ENTRYPOINT and CMD contain shell-quoted arrays that will be eval'd in init
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
// Returns a string that when assigned to a variable and later eval'd, will be properly split
func shellQuoteArray(arr []string) string {
	if len(arr) == 0 {
		return ""
	}

	quoted := make([]string, len(arr))
	for i, s := range arr {
		quoted[i] = shellQuote(s)
	}

	// Join with spaces and return as-is (will be eval'd later in init script)
	return strings.Join(quoted, " ")
}

// netmaskToCIDR converts dotted decimal netmask to CIDR prefix length
// e.g., "255.255.255.0" -> 24, "255.255.0.0" -> 16
func netmaskToCIDR(netmask string) int {
	parts := strings.Split(netmask, ".")
	if len(parts) != 4 {
		return 24 // default to /24
	}
	bits := 0
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		for n > 0 {
			bits += n & 1
			n >>= 1
		}
	}
	return bits
}
