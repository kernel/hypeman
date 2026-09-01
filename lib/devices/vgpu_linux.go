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
	if framework == VGPUFrameworkVendorVFIO {
		return nil, fmt.Errorf("vendor VFIO vGPU requires a metadata claim")
	}
	if framework != VGPUFrameworkMdev {
		return nil, fmt.Errorf("vGPU framework not available")
	}
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

// ListVendorVFIOProfileTypes returns the profiles advertised by each VF.
func ListVendorVFIOProfileTypes(vfs []VirtualFunction) (map[string][]VGPUProfileType, error) {
	return hostVendorVFIO.profileTypes(vfs)
}

// ConfigureVGPU idempotently configures a claimed vendor VFIO VF.
func ConfigureVGPU(ctx context.Context, vfAddress, profileType string) error {
	return hostVendorVFIO.configure(ctx, vfAddress, profileType)
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
			if assignment.DevicePath == "" {
				return nil
			}
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

func mdevReconcileInfos(protectedDevicePaths map[string]struct{}) []MdevReconcileInfo {
	instanceInfos := make([]MdevReconcileInfo, 0, len(protectedDevicePaths))
	for devicePath := range protectedDevicePaths {
		instanceInfos = append(instanceInfos, MdevReconcileInfo{
			MdevUUID:  filepath.Base(devicePath),
			IsRunning: true,
		})
	}
	return instanceInfos
}

// ReconcileVGPUs releases orphaned vGPU assignments.
func ReconcileVGPUs(ctx context.Context, protectedDevicePaths map[string]struct{}, sweepDevices bool) error {
	framework, _, err := DiscoverVGPU()
	if err != nil {
		return err
	}
	if !sweepDevices {
		return nil
	}

	switch framework {
	case VGPUFrameworkMdev:
		return ReconcileMdevs(ctx, mdevReconcileInfos(protectedDevicePaths))
	case VGPUFrameworkVendorVFIO:
		return nil
	}
	return nil
}
