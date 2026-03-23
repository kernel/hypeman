package system

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func replaceSymlinkAtomic(linkPath, target string) error {
	tmpLink := filepath.Join(
		filepath.Dir(linkPath),
		".tmp-"+filepath.Base(linkPath)+"-"+strconv.FormatInt(time.Now().UnixNano(), 10),
	)

	if err := os.Symlink(target, tmpLink); err != nil {
		return err
	}

	if err := os.Rename(tmpLink, linkPath); err != nil {
		_ = os.Remove(tmpLink)
		return fmt.Errorf("rename temp symlink: %w", err)
	}

	return nil
}
