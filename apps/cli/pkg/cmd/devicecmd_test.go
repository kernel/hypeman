package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeviceCommandStructure(t *testing.T) {
	// Test that deviceCmd has the expected subcommands
	assert.Equal(t, "device", deviceCmd.Name)
	assert.Equal(t, "Manage PCI/GPU devices for passthrough", deviceCmd.Usage)

	// Verify subcommands exist
	subcommandNames := make([]string, len(deviceCmd.Commands))
	for i, cmd := range deviceCmd.Commands {
		subcommandNames[i] = cmd.Name
	}

	assert.Contains(t, subcommandNames, "available")
	assert.Contains(t, subcommandNames, "register")
	assert.Contains(t, subcommandNames, "list")
	assert.Contains(t, subcommandNames, "get")
	assert.Contains(t, subcommandNames, "delete")
}

func TestDeviceAvailableCmdStructure(t *testing.T) {
	assert.Equal(t, "available", deviceAvailableCmd.Name)
	assert.Equal(t, "Discover passthrough-capable devices on host", deviceAvailableCmd.Usage)
}

func TestDeviceRegisterCmdStructure(t *testing.T) {
	assert.Equal(t, "register", deviceRegisterCmd.Name)
	assert.Equal(t, "Register a device for passthrough", deviceRegisterCmd.Usage)

	// Check flags exist
	flagNames := make([]string, 0)
	for _, flag := range deviceRegisterCmd.Flags {
		flagNames = append(flagNames, flag.Names()...)
	}

	assert.Contains(t, flagNames, "pci-address")
	assert.Contains(t, flagNames, "name")
}

func TestDeviceDeleteCmdAliases(t *testing.T) {
	// Verify delete has aliases
	assert.Contains(t, deviceDeleteCmd.Aliases, "rm")
	assert.Contains(t, deviceDeleteCmd.Aliases, "unregister")
}
