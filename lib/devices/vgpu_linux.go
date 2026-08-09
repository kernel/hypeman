//go:build linux

package devices

import (
	"context"
	"fmt"
	"path/filepath"
)

// DiscoverVGPU returns the host's active vGPU framework and virtual functions.
func DiscoverVGPU() (VGPUFramework, []VirtualFunction, error) {
	return discoverVGPUWith(discoverMdevVFs, hostVendorVFIO.discoverVFs)
}

func discoverVGPUWith(discoverMdev, discoverVendorVFIO func() ([]VirtualFunction, error)) (VGPUFramework, []VirtualFunction, error) {
	vfs, err := discoverMdev()
	if err != nil {
		return VGPUFrameworkNone, nil, fmt.Errorf("discover mdev VFs: %w", err)
	}
	if len(vfs) > 0 {
		return VGPUFrameworkMdev, vfs, nil
	}

	vfs, err = discoverVendorVFIO()
	if err != nil {
		return VGPUFrameworkNone, nil, fmt.Errorf("discover vendor VFIO VFs: %w", err)
	}
	if len(vfs) == 0 {
		return VGPUFrameworkNone, nil, nil
	}
	return VGPUFrameworkVendorVFIO, vfs, nil
}

// ListGPUProfiles returns available vGPU profiles with availability counts.
func ListGPUProfiles() ([]GPUProfile, error) {
	framework, vfs, err := DiscoverVGPU()
	if err != nil {
		return nil, err
	}
	return ListGPUProfilesWithVFs(framework, vfs)
}

// ListGPUProfilesWithVFs returns available profiles for discovered VFs.
func ListGPUProfilesWithVFs(framework VGPUFramework, vfs []VirtualFunction) ([]GPUProfile, error) {
	switch framework {
	case VGPUFrameworkMdev:
		return listMdevGPUProfilesWithVFs(vfs)
	case VGPUFrameworkVendorVFIO:
		return hostVendorVFIO.listProfiles(vfs)
	default:
		return nil, nil
	}
}

func CreateVGPU(ctx context.Context, profileName, instanceID string) (*VGPUDevice, error) {
	framework, _, err := DiscoverVGPU()
	if err != nil {
		return nil, err
	}
	switch framework {
	case VGPUFrameworkMdev:
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
	case VGPUFrameworkVendorVFIO:
		return hostVendorVFIO.create(ctx, profileName, instanceID)
	default:
		return nil, fmt.Errorf("vGPU framework not available")
	}
}

func DestroyVGPU(ctx context.Context, assignment VGPUAssignment) error {
	framework := assignment.Framework
	if framework == VGPUFrameworkNone && assignment.MdevUUID != "" {
		framework = VGPUFrameworkMdev
	}

	switch framework {
	case VGPUFrameworkMdev:
		mdevUUID := assignment.MdevUUID
		if mdevUUID == "" {
			mdevUUID = filepath.Base(assignment.DevicePath)
		}
		return DestroyMdev(ctx, mdevUUID)
	case VGPUFrameworkVendorVFIO:
		return hostVendorVFIO.destroy(ctx, filepath.Base(assignment.DevicePath), assignment.InstanceID)
	case VGPUFrameworkNone:
		return nil
	default:
		return fmt.Errorf("unknown vGPU framework %q", framework)
	}
}

// ReconcileVGPUs releases orphaned vGPU assignments.
func ReconcileVGPUs(ctx context.Context, protectedDevicePaths map[string]struct{}) error {
	framework, _, err := DiscoverVGPU()
	if err != nil {
		return err
	}

	switch framework {
	case VGPUFrameworkMdev:
		return ReconcileMdevs(ctx, nil)
	case VGPUFrameworkVendorVFIO:
		if protectedDevicePaths == nil {
			return nil
		}
		return hostVendorVFIO.reconcile(ctx, protectedDevicePaths)
	default:
		return nil
	}
}
