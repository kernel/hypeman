//go:build linux

package devices

import (
	"context"
	"fmt"
	"path/filepath"
)

func CreateVGPU(ctx context.Context, profileName, instanceID string) (*VGPUDevice, error) {
	mdev, err := CreateMdev(ctx, profileName, instanceID)
	if err != nil {
		return nil, err
	}
	return &VGPUDevice{
		Framework:   VGPUFrameworkMdev,
		VFAddress:   mdev.VFAddress,
		ProfileType: mdev.ProfileType,
		ProfileName: mdev.ProfileName,
		SysfsPath:   mdev.SysfsPath,
		MdevUUID:    mdev.UUID,
	}, nil
}

func DestroyVGPU(ctx context.Context, framework VGPUFramework, devicePath, mdevUUID string) error {
	if framework != VGPUFrameworkNone && framework != VGPUFrameworkMdev {
		return fmt.Errorf("unknown vGPU framework %q", framework)
	}
	if mdevUUID == "" {
		if devicePath == "" {
			return nil
		}
		mdevUUID = filepath.Base(devicePath)
	}
	return DestroyMdev(ctx, mdevUUID)
}
