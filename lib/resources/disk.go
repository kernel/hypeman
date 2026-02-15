package resources

import (
	"context"
	"strings"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/paths"
	"golang.org/x/sys/unix"
)

// DiskResource implements Resource for disk space discovery and tracking.
type DiskResource struct {
	capacity       int64 // bytes
	dataDir        string
	instanceLister InstanceLister
	imageLister    ImageLister
	volumeLister   VolumeLister
}

// NewDiskResource discovers disk capacity for the data directory.
// If cfg.Capacity.Disk is set, uses that as capacity; otherwise auto-detects via statfs.
func NewDiskResource(cfg *config.Config, p *paths.Paths, instLister InstanceLister, imgLister ImageLister, volLister VolumeLister) (*DiskResource, error) {
	var capacity int64

	if cfg.Capacity.Disk != "" {
		// Parse configured limit
		var ds datasize.ByteSize
		if err := ds.UnmarshalText([]byte(cfg.Capacity.Disk)); err != nil {
			return nil, err
		}
		capacity = int64(ds.Bytes())
	} else {
		// Auto-detect from filesystem
		var stat unix.Statfs_t
		if err := unix.Statfs(cfg.DataDir, &stat); err != nil {
			return nil, err
		}
		capacity = int64(stat.Blocks) * int64(stat.Bsize)
	}

	return &DiskResource{
		capacity:       capacity,
		dataDir:        cfg.DataDir,
		instanceLister: instLister,
		imageLister:    imgLister,
		volumeLister:   volLister,
	}, nil
}

// Type returns the resource type.
func (d *DiskResource) Type() ResourceType {
	return ResourceDisk
}

// Capacity returns the disk capacity in bytes.
func (d *DiskResource) Capacity() int64 {
	return d.capacity
}

// Allocated returns currently allocated disk space.
func (d *DiskResource) Allocated(ctx context.Context) (int64, error) {
	breakdown, err := d.GetBreakdown(ctx)
	if err != nil {
		return 0, err
	}
	return breakdown.Images + breakdown.OCICache + breakdown.Volumes + breakdown.Overlays, nil
}

// GetBreakdown returns disk usage broken down by category.
func (d *DiskResource) GetBreakdown(ctx context.Context) (*DiskBreakdown, error) {
	var breakdown DiskBreakdown

	// Get image sizes
	if d.imageLister != nil {
		imageBytes, err := d.imageLister.TotalImageBytes(ctx)
		if err == nil {
			breakdown.Images = imageBytes
		}
		ociCacheBytes, err := d.imageLister.TotalOCICacheBytes(ctx)
		if err == nil {
			breakdown.OCICache = ociCacheBytes
		}
	}

	// Get volume sizes
	if d.volumeLister != nil {
		volumeBytes, err := d.volumeLister.TotalVolumeBytes(ctx)
		if err == nil {
			breakdown.Volumes = volumeBytes
		}
	}

	// Get overlay sizes from instances
	if d.instanceLister != nil {
		instances, err := d.instanceLister.ListInstanceAllocations(ctx)
		if err == nil {
			for _, inst := range instances {
				if isActiveState(inst.State) {
					breakdown.Overlays += inst.OverlayBytes + inst.VolumeOverlayBytes
				}
			}
		}
	}

	return &breakdown, nil
}

// parseDiskIOLimit parses a disk I/O limit string like "500MB/s", "1GB/s".
// Returns bytes per second.
func parseDiskIOLimit(limit string) (int64, error) {
	limit = strings.TrimSpace(limit)
	limit = strings.ToLower(limit)

	// Remove "/s" or "ps" suffix if present
	limit = strings.TrimSuffix(limit, "/s")
	limit = strings.TrimSuffix(limit, "ps")

	var ds datasize.ByteSize
	if err := ds.UnmarshalText([]byte(limit)); err != nil {
		return 0, err
	}

	return int64(ds.Bytes()), nil
}

// DiskIOResource implements Resource for disk I/O bandwidth tracking.
type DiskIOResource struct {
	capacity       int64 // bytes per second
	instanceLister InstanceLister
}

// NewDiskIOResource creates a disk I/O resource with the given capacity.
func NewDiskIOResource(capacity int64, instLister InstanceLister) *DiskIOResource {
	return &DiskIOResource{capacity: capacity, instanceLister: instLister}
}

// Type returns the resource type.
func (d *DiskIOResource) Type() ResourceType {
	return ResourceDiskIO
}

// Capacity returns the total disk I/O capacity in bytes per second.
func (d *DiskIOResource) Capacity() int64 {
	return d.capacity
}

// Allocated returns total disk I/O allocated across all active instances.
func (d *DiskIOResource) Allocated(ctx context.Context) (int64, error) {
	if d.instanceLister == nil {
		return 0, nil
	}
	instances, err := d.instanceLister.ListInstanceAllocations(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, inst := range instances {
		if isActiveState(inst.State) {
			total += inst.DiskIOBps
		}
	}
	return total, nil
}
