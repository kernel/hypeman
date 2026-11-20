package instances

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/firecracker"
	"github.com/kernel/hypeman/lib/paths"
)

// RecoverParkedForkSources puts back directories renamed by Firecracker's
// snapshot alias fallback. Call under the data directory lock, before even
// listing instances: a lookup by source ID would otherwise read the fork.
func RecoverParkedForkSources(p *paths.Paths) error {
	entries, err := os.ReadDir(p.GuestsDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	backups := make(map[string]string)
	for _, entry := range entries {
		id, stamp, found := strings.Cut(entry.Name(), firecracker.ForkBackupSeparator)
		if !found || !entry.IsDir() {
			continue
		}
		if _, err := strconv.ParseUint(stamp, 10, 63); err != nil {
			continue
		}
		if err := paths.ValidatePathComponent(id); err != nil {
			return fmt.Errorf("invalid source ID in backup %q", entry.Name())
		}
		if other, ok := backups[id]; ok {
			return fmt.Errorf("ambiguous backups for %q: %q and %q", id, other, entry.Name())
		}
		backups[id] = entry.Name()
	}
	for id, backup := range backups {
		if err := restoreParkedForkSource(p, id, backup); err != nil {
			return fmt.Errorf("restore %s from %s: %w", id, backup, err)
		}
	}
	if len(backups) > 0 {
		return syncDir(p.GuestsDir())
	}
	return nil
}

func restoreParkedForkSource(p *paths.Paths, id, backup string) error {
	parked, err := readForkRecoveryMetadata(p, backup, id)
	if err != nil {
		return err
	}
	if parked.ForkPending != nil && !parked.ForkPending.gone() {
		return fmt.Errorf("parked source has a live fork owner")
	}
	source := p.InstanceDir(id)
	link, err := os.Readlink(source)
	switch {
	case err == nil:
		if err := verifyForkAlias(p, source, link); err != nil {
			return err
		}
		if err := os.Remove(source); err != nil {
			return err
		}
	case os.IsNotExist(err):
		// A kill before creating the link or after removing it leaves only the backup.
	default:
		// Never replace an ordinary source directory.
		return fmt.Errorf("source path exists but is not an alias link: %w", err)
	}
	return os.Rename(p.InstanceDir(backup), source)
}

func verifyForkAlias(p *paths.Paths, source, link string) error {
	if !filepath.IsAbs(link) {
		link = filepath.Join(p.GuestsDir(), link)
	}
	id := filepath.Base(link)
	if link != p.InstanceDir(id) || link == source {
		return fmt.Errorf("unexpected source alias target %q", link)
	}
	info, err := os.Lstat(link)
	if os.IsNotExist(err) {
		// The fork the link pointed at has been deleted, which an older
		// binary allows. Nothing can be read through a link that dangles.
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("alias target %q is not a guest directory", link)
	}
	target, err := readForkRecoveryMetadata(p, id, id)
	if err != nil {
		return err
	}
	if target.HypervisorType != hypervisor.TypeFirecracker || (target.ForkPending != nil && !target.ForkPending.gone()) {
		return fmt.Errorf("alias target %q is not an abandoned Firecracker restore", link)
	}
	recorded, err := firecracker.SnapshotSourceAlias(link)
	if err != nil {
		return err
	}
	if filepath.Clean(recorded) != source {
		return fmt.Errorf("target records source %q, not %q", recorded, source)
	}
	return nil
}

func readForkRecoveryMetadata(p *paths.Paths, dirID, expectedID string) (*StoredMetadata, error) {
	data, err := os.ReadFile(p.InstanceMetadata(dirID))
	if err != nil {
		return nil, err
	}
	var meta StoredMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	if meta.Id != expectedID {
		return nil, fmt.Errorf("metadata in %q names %q, expected %q", dirID, meta.Id, expectedID)
	}
	return &meta, nil
}
