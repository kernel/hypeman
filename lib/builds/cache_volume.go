package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/tags"
	"github.com/kernel/hypeman/lib/volumes"
)

// CacheVolumeConfig configures the local persistent build cache volumes.
// The zero value is disabled.
type CacheVolumeConfig struct {
	// Enabled turns on persistent per-scope cache volumes.
	Enabled bool

	// SizeGB is the fixed size of each cache volume.
	// Values <= 0 use DefaultCacheVolumeSizeGB.
	SizeGB int

	// IdleTTL is how long a cache volume may go unused before it is deleted.
	// Values <= 0 use DefaultCacheVolumeIdleTTL.
	IdleTTL time.Duration

	// MaxBytes caps the total size of all cache volumes on the host
	// (0 = unlimited). Idle volumes are evicted LRU-first to stay under it.
	MaxBytes int64

	// MaxVolumes caps the number of cache volumes on the host
	// (0 = unlimited). Idle volumes are evicted LRU-first to stay under it.
	MaxVolumes int
}

const (
	// DefaultCacheVolumeSizeGB is the default fixed size of a cache volume.
	DefaultCacheVolumeSizeGB = 50

	// DefaultCacheVolumeIdleTTL is the default idle lifetime of a cache volume.
	DefaultCacheVolumeIdleTTL = 24 * time.Hour

	// cacheVolumeIDPrefix prefixes all cache volume IDs.
	cacheVolumeIDPrefix = "build-cache-"

	// cacheVolumeManagedTagKey and cacheVolumeManagedTagValue mark volumes
	// created by the build cache manager. The tag distinguishes cache
	// volumes from caller-created volumes that happen to share the ID
	// prefix: the manager only reuses tagged volumes and the reaper only
	// evicts tagged volumes.
	cacheVolumeManagedTagKey   = "hypeman.system/managed-by"
	cacheVolumeManagedTagValue = "build-cache"

	// cacheVolumeSweepInterval is how often the reaper evaluates eviction.
	cacheVolumeSweepInterval = time.Minute
)

// cacheVolumeID returns the volume ID for a cache scope:
// build-cache-<sha256(scope)>.
func cacheVolumeID(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return cacheVolumeIDPrefix + hex.EncodeToString(sum[:])
}

// cacheVolumeManager owns the lifecycle of persistent per-scope BuildKit cache
// volumes: creation, last-used tracking, and eviction. It never deletes a
// volume that is attached or in use by a running build.
type cacheVolumeManager struct {
	config        CacheVolumeConfig
	paths         *paths.Paths
	volumeManager volumes.Manager
	logger        *slog.Logger

	mu       sync.Mutex // guards lastUsed, loaded, and inUse
	lastUsed map[string]time.Time
	loaded   bool
	inUse    map[string]int

	scopeLocks sync.Map // map[string]*sync.Mutex — per-scope build serialization

	now func() time.Time // overridable for tests
}

func newCacheVolumeManager(config CacheVolumeConfig, p *paths.Paths, volumeMgr volumes.Manager, logger *slog.Logger) *cacheVolumeManager {
	if config.SizeGB <= 0 {
		config.SizeGB = DefaultCacheVolumeSizeGB
	}
	if config.IdleTTL <= 0 {
		config.IdleTTL = DefaultCacheVolumeIdleTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &cacheVolumeManager{
		config:        config,
		paths:         p,
		volumeManager: volumeMgr,
		logger:        logger,
		inUse:         make(map[string]int),
		now:           time.Now,
	}
}

// lockScope serializes builds for a cache scope. The returned function
// releases the lock.
func (c *cacheVolumeManager) lockScope(scope string) func() {
	l, _ := c.scopeLocks.LoadOrStore(scope, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// acquireVolume marks a volume as in use by a running build so the reaper
// never evicts it, including the window between creation and attachment.
// The returned function releases the guard.
func (c *cacheVolumeManager) acquireVolume(volID string) func() {
	c.mu.Lock()
	c.inUse[volID]++
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.inUse[volID]--
		if c.inUse[volID] <= 0 {
			delete(c.inUse, volID)
		}
	}
}

// ensureCacheVolume returns the cache volume for a scope, creating it at the
// configured fixed size if it does not exist yet, and marks it used.
func (c *cacheVolumeManager) ensureCacheVolume(ctx context.Context, scope string) (string, error) {
	volID := cacheVolumeID(scope)

	if vol, err := c.volumeManager.GetVolume(ctx, volID); err == nil {
		if !isManagedCacheVolume(vol) {
			return "", fmt.Errorf("cache volume %s exists but was not created by the build cache; refusing to reuse it", volID)
		}
		c.touchLastUsed(volID)
		return volID, nil
	} else if !errors.Is(err, volumes.ErrNotFound) {
		return "", fmt.Errorf("get cache volume: %w", err)
	}

	_, err := c.volumeManager.CreateVolume(ctx, volumes.CreateVolumeRequest{
		Id:     &volID,
		Name:   volID,
		SizeGb: c.config.SizeGB,
		Tags:   tags.Tags{cacheVolumeManagedTagKey: cacheVolumeManagedTagValue},
	})
	if errors.Is(err, volumes.ErrAlreadyExists) {
		// Lost a creation race; reuse the winner only if it is ours.
		vol, getErr := c.volumeManager.GetVolume(ctx, volID)
		if getErr != nil {
			return "", fmt.Errorf("get cache volume after create race: %w", getErr)
		}
		if !isManagedCacheVolume(vol) {
			return "", fmt.Errorf("cache volume %s exists but was not created by the build cache; refusing to reuse it", volID)
		}
	} else if err != nil {
		return "", fmt.Errorf("create cache volume: %w", err)
	}

	c.touchLastUsed(volID)
	return volID, nil
}

// isManagedCacheVolume reports whether a volume carries the internal tag
// marking it as created by the build cache manager.
func isManagedCacheVolume(vol *volumes.Volume) bool {
	return vol.Tags[cacheVolumeManagedTagKey] == cacheVolumeManagedTagValue
}

// touchLastUsed records that a cache volume was used now.
func (c *cacheVolumeManager) touchLastUsed(volID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.loadLocked()
	c.lastUsed[volID] = c.now()
	if err := c.saveLocked(); err != nil {
		c.logger.Warn("failed to persist cache volume state", "volume", volID, "error", err)
	}
}

// statePath returns the path of the last-used metadata file.
func (c *cacheVolumeManager) statePath() string {
	return filepath.Join(c.paths.BuildsDir(), "cache-volumes.json")
}

// loadLocked loads last-used metadata from disk on first use.
// c.mu must be held.
func (c *cacheVolumeManager) loadLocked() {
	if c.loaded {
		return
	}
	c.lastUsed = make(map[string]time.Time)
	if data, err := os.ReadFile(c.statePath()); err == nil {
		raw := make(map[string]time.Time)
		if json.Unmarshal(data, &raw) == nil {
			c.lastUsed = raw
		}
	}
	c.loaded = true
}

// saveLocked persists last-used metadata atomically.
// c.mu must be held.
func (c *cacheVolumeManager) saveLocked() error {
	data, err := json.Marshal(c.lastUsed)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.statePath()), 0755); err != nil {
		return err
	}
	tmp := c.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, c.statePath())
}

// startReaper periodically evicts cache volumes that exceed the idle TTL or
// the host-wide byte/count limits.
func (c *cacheVolumeManager) startReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(cacheVolumeSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.reap(ctx)
			}
		}
	}()
}

// reap deletes cache volumes that are idle past the TTL, then evicts idle
// volumes LRU-first until the byte and count limits are satisfied. Attached
// or in-use volumes are never evicted. Only volumes tagged as managed by the
// build cache are considered; caller-created volumes that happen to share
// the ID prefix are left alone.
func (c *cacheVolumeManager) reap(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()

	vols, err := c.volumeManager.ListVolumes(ctx)
	if err != nil {
		c.logger.Warn("failed to list volumes for cache reaper", "error", err)
		return
	}

	cacheVols := make([]volumes.Volume, 0)
	for _, v := range vols {
		if isManagedCacheVolume(&v) {
			cacheVols = append(cacheVols, v)
		}
	}

	now := c.now()

	deletableLocked := func(v volumes.Volume) bool {
		if len(v.Attachments) > 0 {
			return false
		}
		return c.inUse[v.Id] == 0
	}

	lastUsedLocked := func(v volumes.Volume) time.Time {
		if t, ok := c.lastUsed[v.Id]; ok {
			return t
		}
		return v.CreatedAt
	}

	deleteLocked := func(v volumes.Volume) bool {
		if err := c.volumeManager.DeleteVolume(ctx, v.Id); err != nil {
			c.logger.Warn("failed to evict cache volume", "volume", v.Id, "error", err)
			return false
		}
		c.logger.Info("evicted cache volume", "volume", v.Id)
		delete(c.lastUsed, v.Id)
		return true
	}

	// Idle TTL eviction. Volumes whose delete fails stay in remaining so the
	// limit accounting below still counts them and a later pass can retry.
	remaining := make([]volumes.Volume, 0, len(cacheVols))
	for _, v := range cacheVols {
		if now.Sub(lastUsedLocked(v)) > c.config.IdleTTL && deletableLocked(v) && deleteLocked(v) {
			continue
		}
		remaining = append(remaining, v)
	}

	// Host-wide limits: evict least recently used idle volumes first
	sort.Slice(remaining, func(i, j int) bool {
		return lastUsedLocked(remaining[i]).Before(lastUsedLocked(remaining[j]))
	})

	totalBytes := int64(0)
	for _, v := range remaining {
		totalBytes += int64(v.SizeGb) * 1024 * 1024 * 1024
	}

	present := len(remaining)
	for _, v := range remaining {
		overBytes := c.config.MaxBytes > 0 && totalBytes > c.config.MaxBytes
		overCount := c.config.MaxVolumes > 0 && present > c.config.MaxVolumes
		if (overBytes || overCount) && deletableLocked(v) && deleteLocked(v) {
			totalBytes -= int64(v.SizeGb) * 1024 * 1024 * 1024
			present--
		}
	}

	if err := c.saveLocked(); err != nil {
		c.logger.Warn("failed to persist cache volume state", "error", err)
	}
}
