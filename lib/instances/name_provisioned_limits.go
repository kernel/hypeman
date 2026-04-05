package instances

import (
	"context"
	"fmt"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/lib/resources"
)

type provisionedResources struct {
	Vcpus       int
	MemoryBytes int64
	DiskBytes   int64
	NetworkBps  int64
	DiskIOBps   int64
}

func provisionedResourcesFromAllocation(alloc resources.InstanceAllocation) provisionedResources {
	networkBps := alloc.NetworkDownloadBps
	if alloc.NetworkUploadBps > networkBps {
		networkBps = alloc.NetworkUploadBps
	}

	return provisionedResources{
		Vcpus:       alloc.Vcpus,
		MemoryBytes: alloc.MemoryBytes,
		DiskBytes:   alloc.OverlayBytes + alloc.VolumeOverlayBytes + alloc.VolumeBytes,
		NetworkBps:  networkBps,
		DiskIOBps:   alloc.DiskIOBps,
	}
}

func (r *provisionedResources) add(other provisionedResources) {
	r.Vcpus += other.Vcpus
	r.MemoryBytes += other.MemoryBytes
	r.DiskBytes += other.DiskBytes
	r.NetworkBps += other.NetworkBps
	r.DiskIOBps += other.DiskIOBps
}

func validateProvisionedResourceLimitsForName(name string, limits ResourceLimits, existing []resources.InstanceAllocation, requested provisionedResources) error {
	patternIndex := limits.matchingPatternIndex(name)
	if patternIndex < 0 {
		return nil
	}
	pattern := limits.NamePatterns[patternIndex]
	if !pattern.hasAggregateProvisionedLimits() {
		return nil
	}

	var current provisionedResources
	for _, alloc := range existing {
		if limits.matchingPatternIndex(alloc.Name) != patternIndex {
			continue
		}
		current.add(provisionedResourcesFromAllocation(alloc))
	}

	projected := current
	projected.add(requested)

	if pattern.MaxTotalVcpus != nil && *pattern.MaxTotalVcpus > 0 && projected.Vcpus > *pattern.MaxTotalVcpus {
		return fmt.Errorf("total provisioned cpu %d for pattern %q exceeds maximum allowed %d", projected.Vcpus, pattern.Pattern, *pattern.MaxTotalVcpus)
	}
	if pattern.MaxTotalMemory != nil && *pattern.MaxTotalMemory > 0 && projected.MemoryBytes > *pattern.MaxTotalMemory {
		return fmt.Errorf("total provisioned memory %s for pattern %q exceeds maximum allowed %s", datasize.ByteSize(projected.MemoryBytes).HR(), pattern.Pattern, datasize.ByteSize(*pattern.MaxTotalMemory).HR())
	}
	if pattern.MaxTotalDisk != nil && *pattern.MaxTotalDisk > 0 && projected.DiskBytes > *pattern.MaxTotalDisk {
		return fmt.Errorf("total provisioned disk %s for pattern %q exceeds maximum allowed %s", datasize.ByteSize(projected.DiskBytes).HR(), pattern.Pattern, datasize.ByteSize(*pattern.MaxTotalDisk).HR())
	}
	if pattern.MaxTotalNetworkBandwidth != nil && *pattern.MaxTotalNetworkBandwidth > 0 && projected.NetworkBps > *pattern.MaxTotalNetworkBandwidth {
		return fmt.Errorf("total provisioned network bandwidth %s/s for pattern %q exceeds maximum allowed %s/s", datasize.ByteSize(projected.NetworkBps).HR(), pattern.Pattern, datasize.ByteSize(*pattern.MaxTotalNetworkBandwidth).HR())
	}
	if pattern.MaxTotalDiskIO != nil && *pattern.MaxTotalDiskIO > 0 && projected.DiskIOBps > *pattern.MaxTotalDiskIO {
		return fmt.Errorf("total provisioned disk I/O %s/s for pattern %q exceeds maximum allowed %s/s", datasize.ByteSize(projected.DiskIOBps).HR(), pattern.Pattern, datasize.ByteSize(*pattern.MaxTotalDiskIO).HR())
	}

	return nil
}

func (m *manager) requestedProvisionedResources(ctx context.Context, overlaySize int64, vcpus int, totalMemory int64, networkDownloadBps int64, networkUploadBps int64, diskIOBps int64, volumes []VolumeAttachment) (provisionedResources, error) {
	diskBytes := overlaySize
	for _, attachment := range volumes {
		if attachment.Overlay {
			diskBytes += attachment.OverlaySize
		}
		if m.volumeManager == nil {
			continue
		}
		volume, err := m.volumeManager.GetVolume(ctx, attachment.VolumeID)
		if err != nil {
			return provisionedResources{}, fmt.Errorf("get volume %s: %w", attachment.VolumeID, err)
		}
		diskBytes += int64(volume.SizeGb) * 1024 * 1024 * 1024
	}

	networkBps := networkDownloadBps
	if networkUploadBps > networkBps {
		networkBps = networkUploadBps
	}

	return provisionedResources{
		Vcpus:       vcpus,
		MemoryBytes: totalMemory,
		DiskBytes:   diskBytes,
		NetworkBps:  networkBps,
		DiskIOBps:   diskIOBps,
	}, nil
}

func (m *manager) validateProvisionedResourceLimitsForName(ctx context.Context, name string, overlaySize int64, vcpus int, totalMemory int64, networkDownloadBps int64, networkUploadBps int64, diskIOBps int64, volumes []VolumeAttachment) error {
	pattern := m.limits.matchingPattern(name)
	if pattern == nil || !pattern.hasAggregateProvisionedLimits() {
		return nil
	}

	requested, err := m.requestedProvisionedResources(ctx, overlaySize, vcpus, totalMemory, networkDownloadBps, networkUploadBps, diskIOBps, volumes)
	if err != nil {
		return err
	}

	existing, err := m.ListInstanceAllocations(ctx)
	if err != nil {
		return fmt.Errorf("list existing instance allocations: %w", err)
	}

	return validateProvisionedResourceLimitsForName(name, m.limits, existing, requested)
}
