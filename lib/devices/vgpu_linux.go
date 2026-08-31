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

func DestroyVGPU(ctx context.Context, assignment VGPUAssignment) error {
	if assignment.Framework != VGPUFrameworkNone && assignment.Framework != VGPUFrameworkMdev {
		return fmt.Errorf("unknown vGPU framework %q", assignment.Framework)
	}
	mdevUUID := assignment.MdevUUID
	if mdevUUID == "" {
		if assignment.DevicePath == "" {
			return nil
		}
		mdevUUID = filepath.Base(assignment.DevicePath)
	}
	return DestroyMdev(ctx, mdevUUID)
}
