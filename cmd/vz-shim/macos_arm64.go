//go:build darwin && arm64

package main

import (
	"encoding/base64"
	"fmt"

	"github.com/Code-Hex/vz/v3"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

func newMacBootLoader(c *shimconfig.ShimConfig) (vz.BootLoader, error) {
	if c.MacHardwareModelData == "" || c.MacMachineIdentifierData == "" || c.MacAuxStoragePath == "" {
		return nil, fmt.Errorf("macOS boot requires hardware model, machine identifier, and auxiliary storage")
	}
	return vz.NewMacOSBootLoader()
}
func configureMacPlatform(vc *vz.VirtualMachineConfiguration, c *shimconfig.ShimConfig) error {
	modelData, e := base64.StdEncoding.DecodeString(c.MacHardwareModelData)
	if e != nil {
		return fmt.Errorf("decode Mac hardware model: %w", e)
	}
	model, e := vz.NewMacHardwareModelWithData(modelData)
	if e != nil {
		return e
	}
	if !model.Supported() {
		return fmt.Errorf("Mac hardware model is unsupported on this host")
	}
	idData, e := base64.StdEncoding.DecodeString(c.MacMachineIdentifierData)
	if e != nil {
		return fmt.Errorf("decode Mac machine identifier: %w", e)
	}
	id, e := vz.NewMacMachineIdentifierWithData(idData)
	if e != nil {
		return e
	}
	aux, e := vz.NewMacAuxiliaryStorage(c.MacAuxStoragePath)
	if e != nil {
		return e
	}
	platform, e := vz.NewMacPlatformConfiguration(vz.WithMacHardwareModel(model), vz.WithMacMachineIdentifier(id), vz.WithMacAuxiliaryStorage(aux))
	if e != nil {
		return e
	}
	vc.SetPlatformVirtualMachineConfiguration(platform)
	graphics, e := vz.NewMacGraphicsDeviceConfiguration()
	if e != nil {
		return e
	}
	display, e := vz.NewMacGraphicsDisplayConfiguration(1280, 800, 80)
	if e != nil {
		return e
	}
	graphics.SetDisplays(display)
	vc.SetGraphicsDevicesVirtualMachineConfiguration([]vz.GraphicsDeviceConfiguration{graphics})
	keyboard, e := vz.NewMacKeyboardConfiguration()
	if e != nil {
		return e
	}
	trackpad, e := vz.NewMacTrackpadConfiguration()
	if e != nil {
		return e
	}
	vc.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{keyboard})
	vc.SetPointingDevicesVirtualMachineConfiguration([]vz.PointingDeviceConfiguration{trackpad})
	return nil
}
