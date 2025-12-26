package resources

import (
	"context"
	"syscall"

	"github.com/c2h5oh/datasize"
	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/paths"
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
// If cfg.DiskLimit is set, uses that as capacity; otherwise auto-detects via statfs.
func NewDiskResource(cfg *config.Config, p *paths.Paths, instLister InstanceLister, imgLister ImageLister, volLister VolumeLister) (*DiskResource, error) {
	var capacity int64

	if cfg.DiskLimit != "" {
		// Parse configured limit
		var ds datasize.ByteSize
		if err := ds.UnmarshalText([]byte(cfg.DiskLimit)); err != nil {
			return nil, err
		}
		capacity = int64(ds.Bytes())
	} else {
		// Auto-detect from filesystem
		var stat syscall.Statfs_t
		if err := syscall.Statfs(cfg.DataDir, &stat); err != nil {
			return nil, err
		}
		// Total space = blocks * block size
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

// Capacity returns the total disk space in bytes.
func (d *DiskResource) Capacity() int64 {
	return d.capacity
}

// Allocated returns total disk space used by images, OCI cache, volumes, and overlays.
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

	// Get image sizes (exported rootfs disks)
	if d.imageLister != nil {
		imageBytes, err := d.imageLister.TotalImageBytes(ctx)
		if err == nil {
			breakdown.Images = imageBytes
		}
		// Get OCI layer cache size
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

	// Get overlay sizes from instances (rootfs overlays + volume overlays)
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
