package hypervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// BalloonTargetCache keeps the last requested guest-memory target warm across
// reconnects and Hypeman restarts.
type BalloonTargetCache struct {
	targets sync.Map
	keys    sync.Map
}

func (c *BalloonTargetCache) Store(socketPath string, bytes int64) {
	key := SocketCacheKey(socketPath)
	c.targets.Store(key, bytes)
	c.keys.Store(socketPath, key)
	_ = os.WriteFile(balloonTargetStatePath(socketPath), []byte(fmt.Sprintf("%s\n%d\n", key, bytes)), 0o600)
}

func (c *BalloonTargetCache) Load(socketPath string) (int64, bool) {
	key := SocketCacheKey(socketPath)
	if value, ok := c.loadKey(key); ok {
		c.keys.Store(socketPath, key)
		return value, true
	}

	if indexedKey, ok := c.keys.Load(socketPath); ok {
		if keyString, ok := indexedKey.(string); ok {
			if value, ok := c.loadKey(keyString); ok {
				return value, true
			}
		}
	}

	value, ok := loadBalloonTargetState(socketPath, key)
	if !ok {
		return 0, false
	}
	c.targets.Store(key, value)
	c.keys.Store(socketPath, key)
	return value, true
}

func (c *BalloonTargetCache) Delete(socketPath string) {
	if indexedKey, ok := c.keys.LoadAndDelete(socketPath); ok {
		if keyString, ok := indexedKey.(string); ok {
			c.targets.Delete(keyString)
		}
	}
	c.targets.Delete(SocketCacheKey(socketPath))
	_ = os.Remove(balloonTargetStatePath(socketPath))
}

func (c *BalloonTargetCache) loadKey(key string) (int64, bool) {
	target, ok := c.targets.Load(key)
	if !ok {
		return 0, false
	}
	value, ok := target.(int64)
	return value, ok
}

func balloonTargetStatePath(socketPath string) string {
	base := filepath.Base(socketPath)
	return filepath.Join(filepath.Dir(socketPath), "."+base+".balloon-target")
}

func loadBalloonTargetState(socketPath, expectedKey string) (int64, bool) {
	data, err := os.ReadFile(balloonTargetStatePath(socketPath))
	if err != nil {
		return 0, false
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != expectedKey {
		return 0, false
	}

	value, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
