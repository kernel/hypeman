package instances

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/uffdpager"
)

const firecrackerSnapshotMemoryRelPath = "snapshots/snapshot-latest/memory"

func (m *manager) useFirecrackerUFFD(stored *StoredMetadata) bool {
	return stored != nil &&
		stored.HypervisorType == hypervisor.TypeFirecracker &&
		m.firecrackerSnapshotMemoryBackend == uffdpager.BackendUFFD
}

func useFirecrackerUFFDOnNextRestore(hvType hypervisor.Type, sourceIsStandby bool, targetState State) bool {
	return hvType == hypervisor.TypeFirecracker && sourceIsStandby && targetState != StateStopped
}

func (m *manager) shouldDeferFirecrackerSnapshotMemoryCopy(stored *StoredMetadata, sourceIsStandby bool, targetState State) bool {
	return m.useFirecrackerUFFD(stored) && useFirecrackerUFFDOnNextRestore(stored.HypervisorType, sourceIsStandby, targetState)
}

func firecrackerSnapshotMemoryPathInGuestDir(guestDir string) string {
	return filepath.Join(guestDir, firecrackerSnapshotMemoryRelPath)
}

func (m *manager) lockFirecrackerSnapshotSource(path string) func() {
	key := firecrackerSnapshotSourceLockKey(path)
	if key == "" {
		return func() {}
	}
	value, _ := m.snapshotSourceLocks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func firecrackerSnapshotSourceLockKey(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return ""
	}
	if filepath.Base(path) != "memory" {
		return path
	}
	snapshotDir := filepath.Dir(path)
	if base := filepath.Base(snapshotDir); base != "snapshot-latest" && base != "snapshot-base" {
		return path
	}
	snapshotsDir := filepath.Dir(snapshotDir)
	if filepath.Base(snapshotsDir) != "snapshots" {
		return path
	}
	return filepath.Dir(snapshotsDir)
}

func resolveFirecrackerSnapshotMemoryBackingPath(memoryPath string) (string, error) {
	if _, err := os.Stat(memoryPath); err == nil {
		return memoryPath, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat firecracker snapshot memory backing: %w", err)
	}

	alternatePath := alternateFirecrackerRetainedSnapshotMemoryPath(memoryPath)
	if alternatePath == "" {
		return memoryPath, nil
	}
	if _, err := os.Stat(alternatePath); err == nil {
		return alternatePath, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat alternate firecracker snapshot memory backing: %w", err)
	}
	return memoryPath, nil
}

func alternateFirecrackerRetainedSnapshotMemoryPath(memoryPath string) string {
	if filepath.Base(memoryPath) != "memory" {
		return ""
	}
	snapshotDir := filepath.Dir(memoryPath)
	snapshotsDir := filepath.Dir(snapshotDir)
	switch filepath.Base(snapshotDir) {
	case "snapshot-base":
		return filepath.Join(snapshotsDir, "snapshot-latest", "memory")
	case "snapshot-latest":
		return filepath.Join(snapshotsDir, "snapshot-base", "memory")
	default:
		return ""
	}
}

func firecrackerDeferredSnapshotMemoryPath(stored *StoredMetadata, guestDir string) string {
	if stored != nil {
		if path := strings.TrimSpace(stored.FirecrackerDeferredSnapshotMemoryPath); path != "" {
			return path
		}
	}
	return firecrackerSnapshotMemoryPathInGuestDir(guestDir)
}

func (m *manager) firecrackerSnapshotRestoreOptions(stored *StoredMetadata, snapshotDir string) (hypervisor.RestoreOptions, error) {
	opts := hypervisor.RestoreOptions{SnapshotMemoryBackend: hypervisor.SnapshotMemoryBackendFile}
	if stored == nil || stored.HypervisorType != hypervisor.TypeFirecracker {
		return opts, nil
	}
	if !m.useFirecrackerUFFD(stored) || !stored.FirecrackerUseUFFDOnNextRestore {
		stored.FirecrackerUFFDSessionID = ""
		stored.FirecrackerUFFDPagerVersion = ""
		return opts, nil
	}
	if m.firecrackerUFFDPager == nil {
		return opts, fmt.Errorf("firecracker uffd snapshot restore is enabled but the pager is not configured")
	}

	backingMemoryPath := strings.TrimSpace(stored.FirecrackerDeferredSnapshotMemoryPath)
	if backingMemoryPath == "" {
		backingMemoryPath = filepath.Join(snapshotDir, "memory")
	}
	backingMemoryPath, err := resolveFirecrackerSnapshotMemoryBackingPath(backingMemoryPath)
	if err != nil {
		return opts, err
	}
	cacheKey := strings.TrimSpace(stored.FirecrackerSnapshotCacheKey)
	if cacheKey == "" {
		var err error
		cacheKey, err = firecrackerSnapshotCacheKeyForPaths(stored, backingMemoryPath, filepath.Join(snapshotDir, "state"))
		if err != nil {
			return opts, err
		}
		stored.FirecrackerSnapshotCacheKey = cacheKey
	}
	stored.FirecrackerUFFDSessionID = stored.Id
	stored.FirecrackerUFFDPagerVersion = m.firecrackerUFFDPager.VersionKey()
	opts.SnapshotMemoryBackend = hypervisor.SnapshotMemoryBackendUFFD
	opts.SnapshotMemoryBackingPath = backingMemoryPath
	opts.SnapshotMemoryCacheKey = cacheKey
	opts.SnapshotMemorySessionID = stored.FirecrackerUFFDSessionID
	return opts, nil
}

func (m *manager) refreshFirecrackerSnapshotCacheKey(stored *StoredMetadata, snapshotDir string) error {
	if stored == nil || stored.HypervisorType != hypervisor.TypeFirecracker {
		return nil
	}
	key, err := firecrackerSnapshotCacheKey(stored, snapshotDir)
	if err != nil {
		return err
	}
	stored.FirecrackerSnapshotCacheKey = key
	return nil
}

func (m *manager) closeFirecrackerUFFDSession(ctx context.Context, stored *StoredMetadata) {
	if stored == nil || stored.HypervisorType != hypervisor.TypeFirecracker || stored.FirecrackerUFFDSessionID == "" {
		return
	}
	log := logger.FromContext(ctx)
	if m.firecrackerUFFDPager == nil {
		log.WarnContext(ctx, "cannot close firecracker uffd session; pager is not configured",
			"instance_id", stored.Id,
			"session_id", stored.FirecrackerUFFDSessionID,
			"pager_version", stored.FirecrackerUFFDPagerVersion)
		stored.FirecrackerUFFDSessionID = ""
		stored.FirecrackerUFFDPagerVersion = ""
		return
	}
	version := stored.FirecrackerUFFDPagerVersion
	if version == "" {
		version = m.firecrackerUFFDPager.VersionKey()
	}
	closeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := m.firecrackerUFFDPager.CloseSessionVersion(closeCtx, version, stored.FirecrackerUFFDSessionID); err != nil {
		log.WarnContext(ctx, "failed to close firecracker uffd session",
			"instance_id", stored.Id,
			"session_id", stored.FirecrackerUFFDSessionID,
			"pager_version", version,
			"error", err)
	}
	stored.FirecrackerUFFDSessionID = ""
	stored.FirecrackerUFFDPagerVersion = ""
}

func (m *manager) checkFirecrackerUFFDSessionHealth(ctx context.Context, stored *StoredMetadata) error {
	if stored == nil || stored.HypervisorType != hypervisor.TypeFirecracker || stored.FirecrackerUFFDSessionID == "" {
		return nil
	}
	if m.firecrackerUFFDPager == nil {
		return fmt.Errorf("firecracker uffd session %s has no configured pager", stored.FirecrackerUFFDSessionID)
	}
	version := stored.FirecrackerUFFDPagerVersion
	if version == "" {
		version = m.firecrackerUFFDPager.VersionKey()
	}
	healthCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := m.firecrackerUFFDPager.HealthVersion(healthCtx, version); err != nil {
		return fmt.Errorf("firecracker uffd pager %s for session %s is unhealthy: %w", version, stored.FirecrackerUFFDSessionID, err)
	}
	return nil
}

func firecrackerSnapshotCacheKey(stored *StoredMetadata, snapshotDir string) (string, error) {
	return firecrackerSnapshotCacheKeyForPaths(stored, filepath.Join(snapshotDir, "memory"), filepath.Join(snapshotDir, "state"))
}

func firecrackerSnapshotCacheKeyForPaths(stored *StoredMetadata, memoryPath string, statePath string) (string, error) {
	memoryInfo, err := os.Stat(memoryPath)
	if err != nil {
		return "", fmt.Errorf("stat firecracker snapshot memory for uffd cache key: %w", err)
	}
	stateInfo, err := os.Stat(statePath)
	if err != nil {
		return "", fmt.Errorf("stat firecracker snapshot state for uffd cache key: %w", err)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s:%s:%d:%d:%d:%d",
		stored.Id,
		fileStatFingerprint(memoryInfo),
		memoryInfo.Size(),
		memoryInfo.ModTime().UnixNano(),
		stateInfo.Size(),
		stateInfo.ModTime().UnixNano(),
	)))
	return hex.EncodeToString(sum[:])[:24], nil
}

func fileStatFingerprint(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%s:%d:%d:%d", info.Name(), info.Mode(), stat.Dev, stat.Ino)
	}
	return fmt.Sprintf("%s:%d", info.Name(), info.Mode())
}
