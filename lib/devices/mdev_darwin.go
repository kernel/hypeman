//go:build darwin

package devices

import (
	"context"
	"fmt"
)

// ErrVGPUNotSupportedOnMacOS is returned for vGPU operations on macOS
var ErrVGPUNotSupportedOnMacOS = fmt.Errorf("vGPU (mdev) is not supported on macOS")

// SetGPUProfileCacheTTL is a no-op on macOS.
func SetGPUProfileCacheTTL(ttl string) {
	// No-op on macOS
}

// DiscoverVFs returns an empty list on macOS.
// SR-IOV Virtual Functions are not available on macOS.
func DiscoverVFs() ([]VirtualFunction, error) {
	return []VirtualFunction{}, nil
}

// ListGPUProfiles returns an empty list on macOS.
func ListGPUProfiles() ([]GPUProfile, error) {
	return []GPUProfile{}, nil
}

// ListGPUProfilesWithVFs returns an empty list on macOS.
func ListGPUProfilesWithVFs(vfs []VirtualFunction) ([]GPUProfile, error) {
	return []GPUProfile{}, nil
}

// ListMdevDevices returns an empty list on macOS.
func ListMdevDevices() ([]MdevDevice, error) {
	return []MdevDevice{}, nil
}

// CreateMdev returns an error on macOS as mdev is not supported.
func CreateMdev(ctx context.Context, profileName, instanceID string) (*MdevDevice, error) {
	return nil, ErrVGPUNotSupportedOnMacOS
}

// DestroyMdev is a no-op on macOS.
func DestroyMdev(ctx context.Context, mdevUUID string) error {
	return nil
}

// IsMdevInUse returns false on macOS.
func IsMdevInUse(mdevUUID string) bool {
	return false
}

// ReconcileMdevs is a no-op on macOS.
func ReconcileMdevs(ctx context.Context, instanceInfos []MdevReconcileInfo) error {
	return nil
}
