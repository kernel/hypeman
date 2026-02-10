//go:build darwin

package resources

import (
	"os"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/paths"
	"golang.org/x/sys/unix"
)

// NewDiskResource discovers disk capacity on macOS.
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
		// Auto-detect from filesystem using statfs
		var stat unix.Statfs_t
		dataDir := cfg.DataDir
		if err := unix.Statfs(dataDir, &stat); err != nil {
			// Fallback: try to stat the root if data dir doesn't exist yet
			if os.IsNotExist(err) {
				if err := unix.Statfs("/", &stat); err != nil {
					return nil, err
				}
			} else {
				return nil, err
			}
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
