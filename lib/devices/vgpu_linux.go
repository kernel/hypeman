//go:build linux

package devices

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

func DetectVGPUFramework() VGPUFramework {
	vfs, err := discoverMdevVFs()
	if err == nil && len(vfs) > 0 {
		return VGPUFrameworkMdev
	}
	if hostVendorVFIO.available() {
		return VGPUFrameworkVendorVFIO
	}
	return VGPUFrameworkNone
}

func DiscoverVFs() ([]VirtualFunction, error) {
	switch DetectVGPUFramework() {
	case VGPUFrameworkMdev:
		return discoverMdevVFs()
	case VGPUFrameworkVendorVFIO:
		return hostVendorVFIO.discoverVFs()
	default:
		return nil, nil
	}
}

func ListGPUProfiles() ([]GPUProfile, error) {
	vfs, err := DiscoverVFs()
	if err != nil {
		return nil, err
	}
	return ListGPUProfilesWithVFs(vfs)
}

func ListGPUProfilesWithVFs(vfs []VirtualFunction) ([]GPUProfile, error) {
	switch DetectVGPUFramework() {
	case VGPUFrameworkMdev:
		return listMdevGPUProfilesWithVFs(vfs)
	case VGPUFrameworkVendorVFIO:
		return hostVendorVFIO.listProfiles(vfs)
	default:
		return nil, nil
	}
}

func CreateVGPU(ctx context.Context, profileName, instanceID string) (*VGPUDevice, error) {
	switch DetectVGPUFramework() {
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

func DestroyVGPU(ctx context.Context, framework VGPUFramework, devicePath, mdevUUID string) error {
	if framework == VGPUFrameworkNone {
		switch {
		case mdevUUID != "":
			framework = VGPUFrameworkMdev
		case strings.HasPrefix(devicePath, pciDevicesPath+string(filepath.Separator)):
			framework = VGPUFrameworkVendorVFIO
		}
	}

	switch framework {
	case VGPUFrameworkMdev:
		if mdevUUID == "" {
			mdevUUID = filepath.Base(devicePath)
		}
		return DestroyMdev(ctx, mdevUUID)
	case VGPUFrameworkVendorVFIO:
		return hostVendorVFIO.destroy(ctx, filepath.Base(devicePath))
	case VGPUFrameworkNone:
		return nil
	default:
		return fmt.Errorf("unknown vGPU framework %q", framework)
	}
}

func ReconcileVGPUs(ctx context.Context) error {
	switch DetectVGPUFramework() {
	case VGPUFrameworkMdev:
		return ReconcileMdevs(ctx, nil)
	case VGPUFrameworkVendorVFIO:
		return hostVendorVFIO.reconcile(ctx)
	default:
		return nil
	}
}
