package instances

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/firecracker"
	"github.com/kernel/hypeman/lib/paths"
)

// RecoverParkedForkSources puts back directories renamed by Firecracker's
// snapshot alias fallback and removes verified aliases whose sources no longer
// exist. A source it cannot recover is left exactly as it is and named in the
// returned error, and the rest are still recovered. Network admission must
// reserve addresses from any backups left behind. Call under the data
// directory lock, before listing instances.
func RecoverParkedForkSources(p *paths.Paths) error {
	entries, err := os.ReadDir(p.GuestsDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	backups := make(map[string]string)
	ambiguous := make(map[string]bool)
	for _, entry := range entries {
		id, found := paths.ForkBackupSource(entry.Name())
		if !found || !entry.IsDir() {
			continue
		}
		if err := paths.ValidatePathComponent(id); err != nil {
			errs = append(errs, fmt.Errorf("invalid source ID in backup %q", entry.Name()))
			continue
		}
		if other, ok := backups[id]; ok {
			errs = append(errs, fmt.Errorf("ambiguous backups for %q: %q and %q", id, other, entry.Name()))
			ambiguous[id] = true
			continue
		}
		backups[id] = entry.Name()
	}
	changed := false
	for id, backup := range backups {
		if ambiguous[id] {
			continue
		}
		if err := restoreParkedForkSource(p, id, backup); err != nil {
			errs = append(errs, fmt.Errorf("restore %s from %s: %w", id, backup, err))
			continue
		}
		changed = true
	}
	// A link whose source has a backup was handled above or is left with it.
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 || backups[entry.Name()] != "" {
			continue
		}
		removed, err := removeUnparkedForkAlias(p, p.InstanceDir(entry.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("remove source alias %s: %w", entry.Name(), err))
			continue
		}
		changed = changed || removed
	}
	if changed {
		if err := syncDir(p.GuestsDir()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeUnparkedForkAlias(p *paths.Paths, source string) (bool, error) {
	link, err := os.Readlink(source)
	if err != nil {
		return false, err
	}
	// Without a backup we need the target itself to prove this is a restore alias.
	if err := verifyForkAlias(p, source, link, false); err != nil {
		slog.Warn("leaving unverified source alias", "path", source, "error", err)
		return false, nil
	}
	err = os.Remove(source)
	return err == nil, err
}

func restoreParkedForkSource(p *paths.Paths, id, backup string) error {
	if _, err := readForkRecoveryMetadata(p, backup, id); err != nil {
		return err
	}
	source := p.InstanceDir(id)
	link, err := os.Readlink(source)
	switch {
	case err == nil:
		if err := verifyForkAlias(p, source, link, true); err != nil {
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

func verifyForkAlias(p *paths.Paths, source, link string, allowDangling bool) error {
	if !filepath.IsAbs(link) {
		link = filepath.Join(p.GuestsDir(), link)
	}
	id := filepath.Base(link)
	if link != p.InstanceDir(id) || link == source {
		return fmt.Errorf("unexpected source alias target %q", link)
	}
	info, err := os.Lstat(link)
	if allowDangling && os.IsNotExist(err) {
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
	if target.HypervisorType != hypervisor.TypeFirecracker {
		return fmt.Errorf("alias target %q is not a Firecracker guest", link)
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
