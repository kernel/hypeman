//go:build linux

package resources

import (
	"syscall"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/paths"
)

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
